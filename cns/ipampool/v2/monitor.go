package v2

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/ipampool"
	"github.com/Azure/azure-container-networking/cns/logger"
	"github.com/Azure/azure-container-networking/cns/metric"
	"github.com/Azure/azure-container-networking/crd/clustersubnetstate/api/v1alpha1"
	"github.com/Azure/azure-container-networking/crd/nodenetworkconfig/api/v1alpha"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// DefaultRefreshDelay pool monitor poll delay default in seconds.
	DefaultRefreshDelay = 1 * time.Second
	// DefaultMaxIPs default maximum allocatable IPs
	DefaultMaxIPs = 250
	// Subnet ARM ID /subscriptions/$(SUB)/resourceGroups/$(GROUP)/providers/Microsoft.Network/virtualNetworks/$(VNET)/subnets/$(SUBNET)
	subnetARMIDTemplate = "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/virtualNetworks/%s/subnets/%s"
)

type nodeNetworkConfigSpecUpdater interface {
	UpdateSpec(context.Context, *v1alpha.NodeNetworkConfigSpec) (*v1alpha.NodeNetworkConfig, error)
}

type stateService interface {
	GetPodIPConfigState() map[string]cns.IPConfigurationStatus
	GetPendingReleaseIPConfigs() map[string]cns.IPConfigurationStatus
	MarkNIPsPendingRelease(int) (map[string]cns.IPConfigurationStatus, error)
}

type subnet struct {
	Name, CIDR, ARMID string
}

type scaler struct {
	batch     int64
	buffer    float64
	exhausted bool
	max       int64
}

type Options = ipampool.Options

type Monitor struct {
	opts        *Options
	pool        pool
	scaler      scaler
	subnet      subnet
	nnccli      nodeNetworkConfigSpecUpdater
	httpService cns.HTTPService
	cssSource   <-chan v1alpha1.ClusterSubnetState
	nncSource   chan v1alpha.NodeNetworkConfig
	started     chan interface{}
	once        sync.Once
}

func NewMonitor(httpService cns.HTTPService, nnccli nodeNetworkConfigSpecUpdater, cssSource <-chan v1alpha1.ClusterSubnetState, opts *Options) *Monitor {
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
		cssSource:   cssSource,
		nncSource:   make(chan v1alpha.NodeNetworkConfig),
		started:     make(chan interface{}),
	}
}

// Start begins the Monitor's pool reconcile loop.
// On first run, it will block until a NodeNetworkConfig is received (through a call to Update()).
// Subsequently, it will run run once per RefreshDelay and attempt to re-reconcile the pool.
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
			default:
				// if we have NOT initialized and enter this case, we continue out of this iteration and let the for loop begin again.
				continue
			case <-pm.started: // this blocks until we have initialized
				// if we have initialized and enter this case, we proceed out of the select and continue to reconcile.
			}
		case css := <-pm.cssSource: // received an updated ClusterSubnetState
			pm.scaler.exhausted = css.Status.Exhausted
			logger.Printf("subnet exhausted status = %t", pm.scaler.exhausted)
			ipamSubnetExhaustionCount.With(prometheus.Labels{
				subnetLabel: pm.subnet.Name, subnetCIDRLabel: pm.subnet.CIDR,
				podnetARMIDLabel: pm.subnet.ARMID, subnetExhaustionStateLabel: strconv.FormatBool(pm.scaler.exhausted),
			}).Inc()
			select {
			default:
				// if we have NOT initialized and enter this case, we continue out of this iteration and let the for loop begin again.
				continue
			case <-pm.started: // this blocks until we have initialized
				// if we have initialized and enter this case, we proceed out of the select and continue to reconcile.
			}
		case nnc := <-pm.nncSource: // received a new NodeNetworkConfig, extract the data from it and re-reconcile.
			if len(nnc.Status.NetworkContainers) > 0 {
				// Set SubnetName, SubnetAddressSpace and Pod Network ARM ID values to the global subnet, subnetCIDR and subnetARM variables.
				pm.subnet.Name = nnc.Status.NetworkContainers[0].SubnetName
				pm.subnet.CIDR = nnc.Status.NetworkContainers[0].SubnetAddressSpace
				pm.subnet.ARMID = generateARMID(&nnc.Status.NetworkContainers[0])
				for _, nc := range nnc.Status.NetworkContainers {
					primaryIPs := 0
					if nc.Type == "" || nc.Type == v1alpha.VNET {
						primaryIPs++
					}
					if nc.Type == v1alpha.VNETBlock {
						if _, err := netip.ParsePrefix(nc.PrimaryIP); err != nil {
							return errors.Wrapf(err, "unable to parse IP prefix: %s", nc.PrimaryIP)
						}
						primaryIPs++
					}
					pm.pool.primaryIPs = primaryIPs
				}
			}

			scaler := nnc.Status.Scaler
			pm.scaler.batch = scaler.BatchSize
			pm.scaler.max = scaler.MaxIPCount
			pm.scaler.buffer = float64(scaler.RequestThresholdPercent) / 100 //nolint:gomnd // it's a percentage
			pm.once.Do(func() {
				// we need to grab the RequestedIPCount from the first NodeNetworkConfig we receive
				// (until either we are the one creating the NNC or it is created with an initial request of 0)
				// TODO(rbtr)
				pm.pool.requested = nnc.Spec.RequestedIPCount
				close(pm.started) // close the init channel the first time we fully receive a NodeNetworkConfig.
			})
		}
		// if control has flowed through the select(s) to this point, we can now reconcile.
		err := pm.reconcile(ctx)
		if err != nil {
			logger.Printf("[ipam-pool-monitor] Reconcile failed with err %v", err)
		}
	}
}

var statelogDownsample int

func (pm *Monitor) reconcile(ctx context.Context) error {
	prev := pm.pool // cache the previous pool state for later
	// recalculate the pool state from the current pod IP config state
	knownIPs := pm.httpService.GetPodIPConfigState()
	pm.pool = pm.pool.repopulatePoolState(knownIPs)
	observeIPPoolState(pm.pool, pm.scaler, pm.subnet)

	// log every 30th reconcile to reduce the AI load. we will always log when the monitor
	// changes the pool, below.
	if statelogDownsample = (statelogDownsample + 1) % 30; statelogDownsample == 0 { //nolint:gomnd //downsample by 30
		logger.Printf("ipam-pool-monitor state: %+v", pm.pool)
	}

	// if the subnet is exhausted, locally overwrite the batch/minfree/maxfree in the meta copy for this iteration
	// (until the controlplane owns this and modifies the scaler values for us directly instead of writing "exhausted")
	// TODO(rbtr)
	scaler := pm.scaler
	if scaler.exhausted {
		scaler.batch = 1
		scaler.buffer = 1
	}

	// calculate the target state from the current pool state and scaler
	targetState := pm.pool.scalePool(scaler)

	switch {
	// pod count is increasing
	case targetState.requested > pm.pool.requested:
		logger.Printf("ipam-pool-monitor state %+v", pm.pool)
		logger.Printf("[ipam-pool-monitor] Increasing pool size...")
		return pm.increasePoolSize(ctx, targetState)

	// pod count is decreasing
	case targetState.requested < pm.pool.requested:
		// case state.currentAvailable >= meta.maxFreeCount:
		logger.Printf("ipam-pool-monitor state %+v", pm.pool)
		logger.Printf("[ipam-pool-monitor] Decreasing pool size...")
		return pm.decreasePoolSize(ctx, pm.pool, targetState)

	// CRD has reconciled CNS state, and target spec is now the same size as the state
	// if the previous and current pending release counts are different, we can to clean up the CRD
	case prev.pendingRelease != pm.pool.pendingRelease:
		logger.Printf("ipam-pool-monitor state %+v", pm.pool)
		logger.Printf("[ipam-pool-monitor] Removing Pending Release IPs from CRD...")
		return pm.cleanPendingRelease(ctx, targetState)
	}
	return nil
}

func (pm *Monitor) increasePoolSize(ctx context.Context, target pool) error {
	spec := pm.buildNNCSpec(target.requested)
	if _, err := pm.nnccli.UpdateSpec(ctx, &spec); err != nil {
		return errors.Wrap(err, "failed to UpdateSpec with NNC client")
	}
	logger.Printf("[ipam-pool-monitor] Increased pool size: UpdateCRDSpec succeeded for spec %+v", spec)
	// start an alloc timer
	metric.StartPoolIncreaseTimer(pm.scaler.batch)
	pm.pool = target
	return nil
}

func (pm *Monitor) decreasePoolSize(ctx context.Context, previous, target pool) error {
	// we need to mark the number of IPs that we are scaling down by as as PendingRelease in CNS
	// before we can update the CRD spec to reflect the new target request.
	decrease := previous.requested - target.requested
	logger.Printf("[ipam-pool-monitor] Marking %d IPs as PendingRelease", decrease)
	if _, err := pm.httpService.MarkNIPsPendingRelease(int(decrease)); err != nil {
		return errors.Wrapf(err, "failed to mark sufficent IPs as PendingRelease, wanted %d", decrease)
	}
	spec := pm.buildNNCSpec(target.requested)
	_, err := pm.nnccli.UpdateSpec(ctx, &spec)
	if err != nil {
		return errors.Wrap(err, "failed to UpdateSpec with NNC client")
	}
	logger.Printf("[ipam-pool-monitor] Decreased pool size: UpdateCRDSpec succeeded for spec %+v", spec)
	// start a dealloc timer
	metric.StartPoolDecreaseTimer(pm.scaler.batch)
	pm.pool = target
	return nil
}

// cleanPendingRelease removes IPs from the cache and CRD if the request controller has reconciled
// CNS state and the pending IP release map is empty.
func (pm *Monitor) cleanPendingRelease(ctx context.Context, target pool) error {
	spec := pm.buildNNCSpec(target.requested)
	_, err := pm.nnccli.UpdateSpec(ctx, &spec)
	if err != nil {
		return errors.Wrap(err, "executing UpdateSpec with NNC client")
	}
	logger.Printf("[ipam-pool-monitor] Cleaned pending release: UpdateCRDSpec succeeded for spec %+v", spec)
	return nil
}

// buildNNCSpec translates CNS's map of IPs to be released and requested IP count into an NNC Spec.
func (pm *Monitor) buildNNCSpec(request int64) v1alpha.NodeNetworkConfigSpec {
	// Get All Pending IPs from CNS and populate it again.
	pendingReleaseIPs := pm.httpService.GetPendingReleaseIPConfigs()
	spec := v1alpha.NodeNetworkConfigSpec{
		RequestedIPCount: request,
		IPsNotInUse:      make([]string, len(pendingReleaseIPs)),
	}
	for i := range pendingReleaseIPs {
		spec.IPsNotInUse[i] = pendingReleaseIPs[i].ID
	}
	return spec
}

// GetStateSnapshot gets a snapshot of the IPAMPoolMonitor struct.
func (pm *Monitor) GetStateSnapshot() cns.IpamPoolMonitorStateSnapshot {
	p, s := pm.pool, pm.scaler
	spec := pm.buildNNCSpec(p.requested)
	return cns.IpamPoolMonitorStateSnapshot{
		MinimumFreeIps:           int64(float64(s.batch) * s.buffer),
		MaximumFreeIps:           int64(float64(s.batch) * (s.buffer + 1)),
		UpdatingIpsNotInUseCount: int64(len(spec.IPsNotInUse)),
		CachedNNC: v1alpha.NodeNetworkConfig{
			Spec: spec,
		},
	}
}

// generateARMID uses the Subnet ARM ID format to populate the ARM ID with the metadata.
// If either of the metadata attributes are empty, then the ARM ID will be an empty string.
func generateARMID(nc *v1alpha.NetworkContainer) string {
	subscription := nc.SubscriptionID
	resourceGroup := nc.ResourceGroupID
	vnetID := nc.VNETID
	subnetID := nc.SubnetID

	if subscription == "" || resourceGroup == "" || vnetID == "" || subnetID == "" {
		return ""
	}
	return fmt.Sprintf(subnetARMIDTemplate, subscription, resourceGroup, vnetID, subnetID)
}

// Update ingests a NodeNetworkConfig, clamping some values to ensure they are legal and then
// pushing it to the Monitor's source channel.
// If the Monitor has been Started but is blocking until it receives an NNC, this will start
// the pool reconcile loop.
// If the Monitor has not been Started, this will block until Start() is called, which will
// immediately read this passed NNC and start the pool reconcile loop.
func (pm *Monitor) Update(nnc *v1alpha.NodeNetworkConfig) error {
	pm.clampScaler(&nnc.Status.Scaler)

	// if the nnc has converged, observe the pool scaling latency (if any).
	allocatedIPs := len(pm.httpService.GetPodIPConfigState()) - len(pm.httpService.GetPendingReleaseIPConfigs())
	if int(nnc.Spec.RequestedIPCount) == allocatedIPs {
		// observe elapsed duration for IP pool scaling
		metric.ObserverPoolScaleLatency()
	}
	logger.Printf("[ipam-pool-monitor] pushing NodeNetworkConfig update, allocatedIPs = %d", allocatedIPs)
	pm.nncSource <- *nnc
	return nil
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
