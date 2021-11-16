package ipampool

import (
	"context"
	"fmt"
	"math"
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

// poolState is the Monitor's view of the IP pool.
type poolState struct {
	minFreeCount  int
	maxFreeCount  int
	notInUseCount int
}

type Options struct {
	RefreshDelay time.Duration
	MaxIPs       int
}

type Monitor struct {
	opts        *Options
	spec        v1alpha.NodeNetworkConfigSpec
	scaler      v1alpha.Scaler
	state       poolState
	nnccli      nodeNetworkConfigSpecUpdater
	httpService cns.HTTPService
	initialized chan interface{}
	nncSource   chan v1alpha.NodeNetworkConfig
	once        sync.Once
}

func NewMonitor(httpService cns.HTTPService, nnccli nodeNetworkConfigSpecUpdater, opts *Options) *Monitor {
	if opts.RefreshDelay < 1 {
		opts.RefreshDelay = DefaultRefreshDelay
	}
	if opts.MaxIPs < 1 {
		opts.MaxIPs = DefaultMaxIPs
	}
	return &Monitor{
		opts:        opts,
		httpService: httpService,
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
			pm.scaler = nnc.Status.Scaler
			pm.state.minFreeCount, pm.state.maxFreeCount = CalculateMinFreeIPs(pm.scaler), CalculateMaxFreeIPs(pm.scaler)
			pm.once.Do(func() { close(pm.initialized) }) // close the init channel the first time we receive a NodeNetworkConfig.
		}
		// if control has flowed through the select(s) to this point, we can now reconcile.
		err := pm.reconcile(ctx)
		if err != nil {
			logger.Printf("[ipam-pool-monitor] Reconcile failed with err %v", err)
		}
	}
}

type state struct {
	batch          int
	free           int
	pendingRelease int
	requested      int
	minFree        float64
	maxFree        float64
}

// scale scales the ipam pool according to the state input and returns a new state object where the
// pool size (via the requested and free parameters) is adjusted to be within the bounds of the scaler spec.
func (s state) scale() state {
	prev := s.requested
	if s.free < int(float64(s.batch)*s.minFree) {
		s.requested = s.batch * int(math.Ceil(s.minFree+(float64(s.requested-s.free)/float64(s.batch))))
	} else {
		s.requested = s.batch * int(math.Floor(s.maxFree+(float64(s.requested-s.free)/float64(s.batch))))
	}
	s.free += s.requested - prev
	return s
}

func (s *state) clamp(maxIPs int) {
	if s.requested > maxIPs {
		s.requested = maxIPs
	}
}

func (pm *Monitor) reconcile(ctx context.Context) error {
	cnsPodIPConfigCount := len(pm.httpService.GetPodIPConfigState())
	pendingProgramCount := len(pm.httpService.GetPendingProgramIPConfigs()) // TODO: add pending program count to real cns
	allocatedPodIPCount := len(pm.httpService.GetAllocatedIPConfigs())
	pendingReleaseIPCount := len(pm.httpService.GetPendingReleaseIPConfigs())
	availableIPConfigCount := len(pm.httpService.GetAvailableIPConfigs()) // TODO: add pending allocation count to real cns
	requestedIPConfigCount := pm.spec.RequestedIPCount
	unallocatedIPConfigCount := cnsPodIPConfigCount - allocatedPodIPCount
	freeIPConfigCount := requestedIPConfigCount - int64(allocatedPodIPCount)
	batchSize := pm.scaler.BatchSize
	maxIPCount := pm.scaler.MaxIPCount

	msg := fmt.Sprintf("[ipam-pool-monitor] Pool Size: %v, Goal Size: %v, BatchSize: %v, MaxIPCount: %v, Allocated: %v, Available: %v, Pending Release: %v, Free: %v, Pending Program: %v",
		cnsPodIPConfigCount, pm.spec.RequestedIPCount, batchSize, maxIPCount, allocatedPodIPCount, availableIPConfigCount, pendingReleaseIPCount, freeIPConfigCount, pendingProgramCount)

	ipamAllocatedIPCount.Set(float64(allocatedPodIPCount))
	ipamAvailableIPCount.Set(float64(availableIPConfigCount))
	ipamBatchSize.Set(float64(batchSize))
	ipamFreeIPCount.Set(float64(freeIPConfigCount))
	ipamIPPool.Set(float64(cnsPodIPConfigCount))
	ipamMaxIPCount.Set(float64(maxIPCount))
	ipamPendingProgramIPCount.Set(float64(pendingProgramCount))
	ipamPendingReleaseIPCount.Set(float64(pendingReleaseIPCount))
	ipamRequestedIPConfigCount.Set(float64(requestedIPConfigCount))
	ipamUnallocatedIPCount.Set(float64(unallocatedIPConfigCount))

	current := state{
		batch:          int(batchSize),
		free:           int(freeIPConfigCount),
		pendingRelease: pendingReleaseIPCount,
		requested:      int(requestedIPConfigCount),
		minFree:        float64(pm.scaler.RequestThresholdPercent) / 100, //nolint:gomnd // percent
		maxFree:        float64(pm.scaler.ReleaseThresholdPercent) / 100, //nolint:gomnd // percent
	}
	target := current.scale()
	target.clamp(int(maxIPCount))

	switch {
	// pod count is increasing
	case target.requested > current.requested:
		logger.Printf("[ipam-pool-monitor] Increasing pool size...%s ", msg)
		metric.StartPoolIncreaseTimer(int(batchSize))
		return pm.increasePoolSize(ctx, current, target)

	// pod count is decreasing
	case target.requested < current.requested:
		logger.Printf("[ipam-pool-monitor] Decreasing pool size...%s ", msg)
		metric.StartPoolDecreaseTimer(int(batchSize))
		return pm.decreasePoolSize(ctx, current, target, pendingReleaseIPCount)

	// CRD has reconciled CNS state, and target spec is now the same size as the state
	// free to remove the IP's from the CRD
	case len(pm.spec.IPsNotInUse) != pendingReleaseIPCount:
		logger.Printf("[ipam-pool-monitor] Removing Pending Release IP's from CRD...%s ", msg)
		return pm.cleanPendingRelease(ctx)

	// no pods scheduled
	case allocatedPodIPCount == 0:
		logger.Printf("[ipam-pool-monitor] No pods scheduled, %s", msg)
		return nil
	}

	return nil
}

func (pm *Monitor) increasePoolSize(ctx context.Context, current, target state) error {
	tempNNCSpec := pm.createNNCSpecForCRD()
	tempNNCSpec.RequestedIPCount = int64(target.requested)
	logger.Printf("[ipam-pool-monitor] Increasing pool size, Current Pool Size: %v, Updated Requested IP Count: %v, Pods with IP's:%v, ToBeDeleted Count: %v",
		len(pm.httpService.GetPodIPConfigState()), tempNNCSpec.RequestedIPCount, len(pm.httpService.GetAllocatedIPConfigs()), len(tempNNCSpec.IPsNotInUse))
	if _, err := pm.nnccli.UpdateSpec(ctx, &tempNNCSpec); err != nil {
		// caller will retry to update the CRD again
		return err
	}
	logger.Printf("[ipam-pool-monitor] Increasing pool size: UpdateCRDSpec succeeded for spec %+v", tempNNCSpec)
	pm.spec = tempNNCSpec
	return nil
}

func (pm *Monitor) decreasePoolSize(ctx context.Context, current, target state, existingPendingReleaseIPCount int) error {
	// mark n number of IP's as pending
	var newIpsMarkedAsPending bool
	var pendingIPAddresses map[string]cns.IPConfigurationStatus

	decreaseIPCountBy := current.requested - target.requested
	logger.Printf("[ipam-pool-monitor] updatedRequestedIPCount %v", target.requested)

	if pm.state.notInUseCount == 0 ||
		pm.state.notInUseCount < existingPendingReleaseIPCount {
		logger.Printf("[ipam-pool-monitor] Marking IPs as PendingRelease, ipsToBeReleasedCount %d", decreaseIPCountBy)
		var err error
		pendingIPAddresses, err = pm.httpService.MarkIPAsPendingRelease(decreaseIPCountBy)
		if err != nil {
			return err
		}

		newIpsMarkedAsPending = true
	}

	tempNNCSpec := pm.createNNCSpecForCRD()

	if newIpsMarkedAsPending {
		// cache the updatingPendingRelease so that we dont re-set new IPs to PendingRelease in case UpdateCRD call fails
		pm.state.notInUseCount = len(tempNNCSpec.IPsNotInUse)
	}

	logger.Printf("[ipam-pool-monitor] Releasing IPCount in this batch %d, updatingPendingIpsNotInUse count %d",
		len(pendingIPAddresses), pm.state.notInUseCount)

	tempNNCSpec.RequestedIPCount -= int64(len(pendingIPAddresses))
	logger.Printf("[ipam-pool-monitor] Decreasing pool size, Current Pool Size: %v, Requested IP Count: %v, Pods with IP's: %v, ToBeDeleted Count: %v", len(pm.httpService.GetPodIPConfigState()), tempNNCSpec.RequestedIPCount, len(pm.httpService.GetAllocatedIPConfigs()), len(tempNNCSpec.IPsNotInUse))

	_, err := pm.nnccli.UpdateSpec(ctx, &tempNNCSpec)
	if err != nil {
		// caller will retry to update the CRD again
		return err
	}

	logger.Printf("[ipam-pool-monitor] Decreasing pool size: UpdateCRDSpec succeeded for spec %+v", tempNNCSpec)
	// start a dealloc timer

	// save the updated state to cachedSpec
	pm.spec = tempNNCSpec

	// clear the updatingPendingIpsNotInUse, as we have Updated the CRD
	logger.Printf("[ipam-pool-monitor] cleaning the updatingPendingIpsNotInUse, existing length %d", pm.state.notInUseCount)
	pm.state.notInUseCount = 0

	return nil
}

// cleanPendingRelease removes IPs from the cache and CRD if the request controller has reconciled
// CNS state and the pending IP release map is empty.
func (pm *Monitor) cleanPendingRelease(ctx context.Context) error {
	tempNNCSpec := pm.createNNCSpecForCRD()

	_, err := pm.nnccli.UpdateSpec(ctx, &tempNNCSpec)
	if err != nil {
		// caller will retry to update the CRD again
		return err
	}

	logger.Printf("[ipam-pool-monitor] cleanPendingRelease: UpdateCRDSpec succeeded for spec %+v", tempNNCSpec)

	// save the updated state to cachedSpec
	pm.spec = tempNNCSpec
	return nil
}

// createNNCSpecForCRD translates CNS's map of IPs to be released and requested IP count into an NNC Spec.
func (pm *Monitor) createNNCSpecForCRD() v1alpha.NodeNetworkConfigSpec {
	var spec v1alpha.NodeNetworkConfigSpec

	// Update the count from cached spec
	spec.RequestedIPCount = pm.spec.RequestedIPCount

	// Get All Pending IPs from CNS and populate it again.
	pendingIPs := pm.httpService.GetPendingReleaseIPConfigs()
	for _, pendingIP := range pendingIPs {
		spec.IPsNotInUse = append(spec.IPsNotInUse, pendingIP.ID)
	}

	return spec
}

// GetStateSnapshot gets a snapshot of the IPAMPoolMonitor struct.
func (pm *Monitor) GetStateSnapshot() cns.IpamPoolMonitorStateSnapshot {
	scaler, spec, state := pm.scaler, pm.spec, pm.state
	return cns.IpamPoolMonitorStateSnapshot{
		MinimumFreeIps:           state.minFreeCount,
		MaximumFreeIps:           state.maxFreeCount,
		UpdatingIpsNotInUseCount: state.notInUseCount,
		CachedNNC: v1alpha.NodeNetworkConfig{
			Spec: spec,
			Status: v1alpha.NodeNetworkConfigStatus{
				Scaler: scaler,
			},
		},
	}
}

// Update ingests a NodeNetworkConfig, clamping some values to ensure they are legal and then
// pushing it to the Monitor's source channel.
func (pm *Monitor) Update(nnc *v1alpha.NodeNetworkConfig) {
	pm.clampScaler(&nnc.Status.Scaler)

	// if the nnc has converged, observe the pool scaling latency (if any).
	allocatedIPs := len(pm.httpService.GetPodIPConfigState()) - len(pm.httpService.GetPendingReleaseIPConfigs())
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
		scaler.MaxIPCount = int64(pm.opts.MaxIPs)
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

// CalculateMinFreeIPs calculates the minimum free IP quantity based on the Scaler
// in the passed NodeNetworkConfig.
//nolint:gocritic // ignore hugeparam
func CalculateMinFreeIPs(scaler v1alpha.Scaler) int {
	return int(float64(scaler.BatchSize) * (float64(scaler.RequestThresholdPercent) / 100)) //nolint:gomnd // it's a percent
}

// CalculateMaxFreeIPs calculates the maximum free IP quantity based on the Scaler
// in the passed NodeNetworkConfig.
//nolint:gocritic // ignore hugeparam
func CalculateMaxFreeIPs(scaler v1alpha.Scaler) int {
	return int(float64(scaler.BatchSize) * (float64(scaler.ReleaseThresholdPercent) / 100)) //nolint:gomnd // it's a percent
}
