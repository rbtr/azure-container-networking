package ipampool

import (
	"context"
	"sync"
	"time"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/logger"
	"github.com/Azure/azure-container-networking/cns/metric"
	"github.com/Azure/azure-container-networking/crd/nodenetworkconfig/api/v1alpha"
	"github.com/pkg/errors"
)

const (
	// DefaultRefreshDelay pool monitor poll delay default in seconds.
	DefaultRefreshDelay = 1 * time.Second
	// DefaultMaxIPs default maximum allocatable IPs
	DefaultMaxIPs = 250
)

type nodeNetworkConfigSpecUpdater interface {
	UpdateSpec(context.Context, *v1alpha.NodeNetworkConfigSpec) (*v1alpha.NodeNetworkConfig, error)
}

type ipStatusManager interface {
	GetPendingReleaseIPConfigs() []cns.IPConfigurationStatus
	GetPodIPConfigState() map[string]cns.IPConfigurationStatus
	MarkIPAsPendingRelease(numberToMark int) (map[string]cns.IPConfigurationStatus, error)
}

// metaState is the Monitor's configuration state for the IP pool.
type metaState struct {
	batch          int64
	max            int64
	maxFreeCount   int64
	maxFreePercent float64
	minFreeCount   int64
	minFreePercent float64
	notInUseCount  int64
}

type Options struct {
	RefreshDelay time.Duration
	MaxIPs       int64
}

type Monitor struct {
	opts        *Options
	spec        v1alpha.NodeNetworkConfigSpec
	metastate   metaState
	nnccli      nodeNetworkConfigSpecUpdater
	ipm         ipStatusManager
	initialized chan interface{}
	nncSource   chan v1alpha.NodeNetworkConfig
	once        sync.Once
}

func NewMonitor(ipm ipStatusManager, nnccli nodeNetworkConfigSpecUpdater, opts *Options) *Monitor {
	if opts.RefreshDelay < 1 {
		opts.RefreshDelay = DefaultRefreshDelay
	}
	if opts.MaxIPs < 1 {
		opts.MaxIPs = DefaultMaxIPs
	}
	return &Monitor{
		opts:        opts,
		ipm:         ipm,
		nnccli:      nnccli,
		initialized: make(chan interface{}),
		nncSource:   make(chan v1alpha.NodeNetworkConfig),
	}
}

func (pm *Monitor) Start(ctx context.Context) error {
	logger.Printf("[ipam-pool-monitor] Starting CNS IPAM Pool Monitor")

	ticker := time.NewTicker(pm.opts.RefreshDelay)
	defer ticker.Stop()

	for {
		// proceed when things happen:
		select {
		case <-ctx.Done(): // calling context has closed, we'll exit.
			return errors.Wrap(ctx.Err(), "pool monitor context closed")
		case <-ticker.C: // attempt to reconcile every tick.
			select {
			case <-pm.initialized: // this blocks until we have initialized
				// if we have initialized and enter this case, we proceed out of the select and continue to reconcile.
			default:
				// if we have NOT initialized and enter this case, we continue out of this iteration and let the for loop begin again.
				continue
			}
		case nnc := <-pm.nncSource: // received a new NodeNetworkConfig, extract the data from it and re-reconcile.
			pm.spec = nnc.Spec
			scaler := nnc.Status.Scaler
			pm.metastate.batch = scaler.BatchSize
			pm.metastate.max = scaler.MaxIPCount
			pm.metastate.minFreePercent, pm.metastate.maxFreePercent = minFreePercent(scaler), maxFreePercent(scaler)
			pm.metastate.minFreeCount, pm.metastate.maxFreeCount = CalculateMinFreeIPs(scaler), CalculateMaxFreeIPs(scaler)
			pm.once.Do(func() { close(pm.initialized) }) // close the init channel the first time we receive a NodeNetworkConfig.
		}
		// if control has flowed through the select(s) to this point, we can now reconcile.
		err := pm.reconcile(ctx)
		if err != nil {
			logger.Printf("[ipam-pool-monitor] Reconcile failed with err %v", err)
		}
	}
}

func (pm *Monitor) reconcile(ctx context.Context) error {
	allocatedIPs := pm.ipm.GetPodIPConfigState()
	pool := newIPPool(allocatedIPs, pm.spec)
	logger.Printf("ipam-pool-monitor state %+v", pool)
	observeIPPoolState(pool, pm.metastate)

	switch {
	// pod count is increasing
	case pool.shouldScaleUp(pm.metastate.minFreeCount, pm.metastate.max):
		logger.Printf("[ipam-pool-monitor] Increasing pool size...")
		return pm.increasePoolSize(ctx, pool)

	case pool.shouldScaleDown(pm.metastate.maxFreeCount):
		logger.Printf("[ipam-pool-monitor] Decreasing pool size...")
		return pm.decreasePoolSize(ctx, pool)

	// CRD has reconciled CNS state, and target spec is now the same size as the state
	// free to remove the IPs from the CRD
	case pool.shouldCleanPendingRelease(int64(len(pm.spec.IPsNotInUse))):
		logger.Printf("[ipam-pool-monitor] Removing Pending Release IPs from CRD...")
		return pm.cleanPendingRelease(ctx)

	// no pods scheduled
	case pool.assigned == 0:
		logger.Printf("[ipam-pool-monitor] No pods scheduled")
		return nil
	}

	return nil
}

func (pm *Monitor) increasePoolSize(ctx context.Context, pool ipPool) error {
	prev := pool.requested
	pool = pool.scale(pm.metastate.batch, pm.metastate.minFreePercent, pm.metastate.maxFreePercent)
	logger.Printf("scaling pool up to %+v", pool)

	if pool.requested > pm.metastate.max {
		// We don't want to ask for more ips than the max
		logger.Printf("[ipam-pool-monitor] Requested IP count (%d) is over max limit (%d), requesting max limit instead.", pool.requested, pm.metastate.max)
		pool.requested = pm.metastate.max
	}

	// If the requested IP count is same as before, then don't do anything
	if pool.requested == prev {
		logger.Printf("[ipam-pool-monitor] Previously requested IP count %d is same as updated IP count %d, doing nothing", prev, pool.requested)
		return nil
	}

	logger.Printf("[ipam-pool-monitor] Increasing pool size, pool %+v", pool)

	spec := pm.createNNCSpecForCRD()
	spec.RequestedIPCount = pool.requested
	if _, err := pm.nnccli.UpdateSpec(ctx, &spec); err != nil {
		// caller will retry to update the CRD again
		return errors.Wrap(err, "failed to increase pool size")
	}

	logger.Printf("[ipam-pool-monitor] Increasing pool size: UpdateCRDSpec succeeded for spec %+v", spec)
	// start an alloc timer
	metric.StartPoolIncreaseTimer(pm.metastate.batch)
	// save the updated state to cachedSpec
	pm.spec = spec
	return nil
}

func (pm *Monitor) decreasePoolSize(ctx context.Context, pool ipPool) error {
	prev := pool.requested
	pool = pool.scale(pm.metastate.batch, pm.metastate.minFreePercent, pm.metastate.maxFreePercent)
	deallocate := prev - pool.requested
	logger.Printf("scaling pool down to %+v", pool)

	newIpsMarkedAsPending := false
	pendingIPAddresses := map[string]cns.IPConfigurationStatus{}
	if pm.metastate.notInUseCount == 0 || pm.metastate.notInUseCount < pool.pendingRelease {
		logger.Printf("[ipam-pool-monitor] Marking IPs as PendingRelease, ipsToBeReleasedCount %d", deallocate)
		var err error
		if pendingIPAddresses, err = pm.ipm.MarkIPAsPendingRelease(int(deallocate)); err != nil {
			return errors.Wrapf(err, "failed to deallocate %d IPs", deallocate)
		}

		newIpsMarkedAsPending = true
	}

	spec := pm.createNNCSpecForCRD()
	spec.RequestedIPCount = pool.requested

	if newIpsMarkedAsPending {
		// cache the updatingPendingRelease so that we dont re-set new IPs to PendingRelease in case UpdateCRD call fails
		pm.metastate.notInUseCount = int64(len(spec.IPsNotInUse))
	}
	logger.Printf("[ipam-pool-monitor] Releasing IPCount in this batch %d, updatingPendingIpsNotInUse count %d", len(pendingIPAddresses), pm.metastate.notInUseCount)

	logger.Printf("[ipam-pool-monitor] Decreasing pool size, pool %+v, spec %+v", pool, spec)

	_, err := pm.nnccli.UpdateSpec(ctx, &spec)
	if err != nil {
		return errors.Wrap(err, "failed to decrease pool size")
	}

	logger.Printf("[ipam-pool-monitor] Decreasing pool size: UpdateCRDSpec succeeded for spec %+v", spec)
	// start a dealloc timer
	metric.StartPoolDecreaseTimer(pm.metastate.batch)

	// save the updated state to cachedSpec
	pm.spec = spec

	// clear the updatingPendingIpsNotInUse, as we have Updated the CRD
	logger.Printf("[ipam-pool-monitor] cleaning the updatingPendingIpsNotInUse, existing length %d", pm.metastate.notInUseCount)
	pm.metastate.notInUseCount = 0

	return nil
}

// cleanPendingRelease removes IPs from the cache and CRD if the request controller has reconciled
// CNS state and the pending IP release map is empty.
func (pm *Monitor) cleanPendingRelease(ctx context.Context) error {
	spec := pm.createNNCSpecForCRD()
	_, err := pm.nnccli.UpdateSpec(ctx, &spec)
	if err != nil {
		// caller will retry to update the CRD again
		return errors.Wrap(err, "failed to clean PendingRelease IPs")
	}

	logger.Printf("[ipam-pool-monitor] cleanPendingRelease: UpdateCRDSpec succeeded for spec %+v", spec)

	// save the updated state to cachedSpec
	pm.spec = spec
	return nil
}

// createNNCSpecForCRD translates CNS's map of IPs to be released and requested IP count into an NNC Spec.
func (pm *Monitor) createNNCSpecForCRD() v1alpha.NodeNetworkConfigSpec {
	var spec v1alpha.NodeNetworkConfigSpec

	// Update the count from cached spec
	spec.RequestedIPCount = pm.spec.RequestedIPCount

	// Get All Pending IPs from CNS and populate it again.
	pendingIPs := pm.ipm.GetPendingReleaseIPConfigs()
	for _, pendingIP := range pendingIPs {
		spec.IPsNotInUse = append(spec.IPsNotInUse, pendingIP.ID)
	}

	return spec
}

// GetStateSnapshot gets a snapshot of the IPAMPoolMonitor struct.
func (pm *Monitor) GetStateSnapshot() cns.IpamPoolMonitorStateSnapshot {
	spec, state := pm.spec, pm.metastate
	return cns.IpamPoolMonitorStateSnapshot{
		MinimumFreeIps:           state.minFreeCount,
		MaximumFreeIps:           state.maxFreeCount,
		UpdatingIpsNotInUseCount: state.notInUseCount,
		CachedNNC: v1alpha.NodeNetworkConfig{
			Spec: spec,
		},
	}
}

// Update ingests a NodeNetworkConfig, clamping some values to ensure they are legal and then
// pushing it to the Monitor's source channel.
func (pm *Monitor) Update(nnc *v1alpha.NodeNetworkConfig) {
	pm.clampScaler(&nnc.Status.Scaler)

	// if the nnc has converged, observe the pool scaling latency (if any).
	allocatedIPs := len(pm.ipm.GetPodIPConfigState()) - len(pm.ipm.GetPendingReleaseIPConfigs())
	if int(nnc.Spec.RequestedIPCount) == allocatedIPs {
		// observe elapsed duration for IP pool scaling
		metric.ObserverPoolScaleLatency()
	}
	pm.nncSource <- *nnc
}

// clampScaler makes sure that the values stored in the scaler are sane.
// we usually expect these to be correctly set for us, but we could crash
// without these checks. if they are incorrectly set, there will be some weird
// IP pool behavior for a while until the nnc reconciler corrects the state.
func (pm *Monitor) clampScaler(scaler *v1alpha.Scaler) {
	if scaler.MaxIPCount < 1 {
		scaler.MaxIPCount = pm.opts.MaxIPs
	}
	if scaler.BatchSize < 1 {
		scaler.BatchSize = 1
	}
	if scaler.BatchSize > scaler.MaxIPCount {
		scaler.BatchSize = scaler.MaxIPCount
	}
	if scaler.RequestThresholdPercent < 1 {
		scaler.RequestThresholdPercent = 1
	}
	if scaler.RequestThresholdPercent > 100 { //nolint:gomnd // it's a percent
		scaler.RequestThresholdPercent = 100
	}
	if scaler.ReleaseThresholdPercent < scaler.RequestThresholdPercent+100 {
		scaler.ReleaseThresholdPercent = scaler.RequestThresholdPercent + 100 //nolint:gomnd // it's a percent
	}
}

// minFreePercent calculates the minimum free IP fraction (as a percentage of the batch).
func minFreePercent(scaler v1alpha.Scaler) float64 {
	return float64(scaler.RequestThresholdPercent) / 100 //nolint:gomnd // it's a percent
}

// CalculateMinFreeIPs calculates the minimum free IP quantity based on the Scaler
// in the passed NodeNetworkConfig.
//nolint:gocritic // ignore hugeparam
func CalculateMinFreeIPs(scaler v1alpha.Scaler) int64 {
	return int64(float64(scaler.BatchSize) * minFreePercent(scaler))
}

// maxFreePercent calculates the maximum free IP fraction (as a percentage of the batch).
func maxFreePercent(scaler v1alpha.Scaler) float64 {
	return float64(scaler.ReleaseThresholdPercent) / 100 //nolint:gomnd // it's a percent
}

// CalculateMaxFreeIPs calculates the maximum free IP quantity based on the Scaler
// in the passed NodeNetworkConfig.
//nolint:gocritic // ignore hugeparam
func CalculateMaxFreeIPs(scaler v1alpha.Scaler) int64 {
	return int64(float64(scaler.BatchSize) * maxFreePercent(scaler))
}
