package ipampool

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/logger"
	"github.com/Azure/azure-container-networking/cns/metric"
	"github.com/Azure/azure-container-networking/cns/types"
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

// metaState is the Monitor's configuration state for the IP pool.
type metaState struct {
	batch              int64
	buffer             float64
	exhausted          bool
	max                int64
	notInUseCount      int64
	primaryIPAddresses map[string]struct{}
	subnet             string
	subnetARMID        string
	subnetCIDR         string
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
			pm.metastate.exhausted = css.Status.Exhausted
			logger.Printf("subnet exhausted status = %t", pm.metastate.exhausted)
			ipamSubnetExhaustionCount.With(prometheus.Labels{
				subnetLabel: pm.metastate.subnet, subnetCIDRLabel: pm.metastate.subnetCIDR,
				podnetARMIDLabel: pm.metastate.subnetARMID, subnetExhaustionStateLabel: strconv.FormatBool(pm.metastate.exhausted),
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
				pm.metastate.subnet = nnc.Status.NetworkContainers[0].SubnetName
				pm.metastate.subnetCIDR = nnc.Status.NetworkContainers[0].SubnetAddressSpace
				pm.metastate.subnetARMID = GenerateARMID(&nnc.Status.NetworkContainers[0])
			}
			pm.metastate.primaryIPAddresses = make(map[string]struct{})
			// Add Primary IP to Map, if not present.
			// This is only for Swift i.e. if NC Type is vnet.
			for i := 0; i < len(nnc.Status.NetworkContainers); i++ {
				if nnc.Status.NetworkContainers[i].Type == "" ||
					nnc.Status.NetworkContainers[i].Type == v1alpha.VNET {
					pm.metastate.primaryIPAddresses[nnc.Status.NetworkContainers[i].PrimaryIP] = struct{}{}
				}
			}

			scaler := nnc.Status.Scaler
			pm.metastate.batch = scaler.BatchSize
			pm.metastate.max = scaler.MaxIPCount
			pm.metastate.buffer = float64(scaler.RequestThresholdPercent) / 100 //nolint:gomnd // it's a percentage
			pm.once.Do(func() {
				pm.spec = nnc.Spec // set the spec from the NNC initially (afterwards we write the Spec so we know target state).
				logger.Printf("[ipam-pool-monitor] set initial pool spec %+v", pm.spec)
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

// ipPoolState is the current actual state of the CNS IP pool.
type ipPoolState struct {
	// allocated are all the IPs allocated to CNS by DNC.
	allocated int64
	// assigned are the IPs that CNS has assigned to Pods.
	assigned int64
	// available are the allocated IPs in state "Available".
	available int64
	// currentAvailable are the current available IPs.
	// currentAvailable = allocated - assigned - pendingRelease.
	currentAvailable int64
	// expectedAvailable are the "future" available allocated IPs, if the requested IP count is honored.
	// expectedAvailable = requested - assigned.
	expectedAvailable int64
	// pendingProgramming are the allocated IPs in state "PendingProgramming".
	pendingProgramming int64
	// pendingRelease are the allocated IPs in state "PendingRelease".
	pendingRelease int64
	// requested are the target allocation of IPs that CNS has requested from DNC.
	requested int64
}

func buildIPPoolState(ips map[string]cns.IPConfigurationStatus, spec v1alpha.NodeNetworkConfigSpec) ipPoolState {
	state := ipPoolState{
		allocated: int64(len(ips)),
		requested: spec.RequestedIPCount,
	}
	for i := range ips {
		ip := ips[i]
		switch ip.GetState() {
		case types.Assigned:
			state.assigned++
		case types.Available:
			state.available++
		case types.PendingProgramming:
			state.pendingProgramming++
		case types.PendingRelease:
			state.pendingRelease++
		}
	}
	state.currentAvailable = state.allocated - state.assigned - state.pendingRelease
	state.expectedAvailable = state.requested - state.assigned
	return state
}

var statelogDownsample int

func (pm *Monitor) reconcile(ctx context.Context) error {
	allocatedIPs := pm.httpService.GetPodIPConfigState()
	meta := pm.metastate
	state := buildIPPoolState(allocatedIPs, pm.spec)
	observeIPPoolState(state, meta)

	// log every 30th reconcile to reduce the AI load. we will always log when the monitor
	// changes the pool, below.
	if statelogDownsample = (statelogDownsample + 1) % 30; statelogDownsample == 0 { //nolint:gomnd //downsample by 30
		logger.Printf("ipam-pool-monitor state: %+v, meta: %+v", state, meta)
	}

	// if the subnet is exhausted, overwrite the batch/minfree/maxfree in the meta copy for this iteration
	if meta.exhausted {
		meta.batch = 1
		meta.buffer = 1
	}

	demand := state.assigned
	targetRequest := calculateTargetIPCount(demand, meta.batch, meta.buffer)

	switch {
	// pod count is increasing
	case targetRequest > state.requested:
		// case state.expectedAvailable < meta.minFreeCount:
		if state.requested == meta.max {
			// if we're already at the maxIPCount, don't try to increase.
			return nil
		}
		if targetRequest > meta.max {
			// if the target is above the max, cap at the max.
			targetRequest = meta.max
		}
		logger.Printf("ipam-pool-monitor state %+v", state)
		logger.Printf("[ipam-pool-monitor] Increasing pool size...")
		return pm.increasePoolSize(ctx, targetRequest)

	// pod count is decreasing
	case targetRequest < state.requested:
		// case state.currentAvailable >= meta.maxFreeCount:
		logger.Printf("ipam-pool-monitor state %+v", state)
		logger.Printf("[ipam-pool-monitor] Decreasing pool size...")
		return pm.decreasePoolSize(ctx, state, targetRequest)

	// CRD has reconciled CNS state, and target spec is now the same size as the state
	// free to remove the IPs from the CRD
	case int64(len(pm.spec.IPsNotInUse)) != state.pendingRelease:
		logger.Printf("ipam-pool-monitor state %+v", state)
		logger.Printf("[ipam-pool-monitor] Removing Pending Release IPs from CRD...")
		return pm.cleanPendingRelease(ctx)

	// no pods scheduled
	case state.assigned == 0:
		logger.Printf("ipam-pool-monitor state %+v", state)
		logger.Printf("[ipam-pool-monitor] No pods scheduled")
		return nil
	}

	return nil
}

func (pm *Monitor) increasePoolSize(ctx context.Context, target int64) error {
	tempNNCSpec := pm.createNNCSpecForCRD(target)
	tempNNCSpec.RequestedIPCount = target

	if _, err := pm.nnccli.UpdateSpec(ctx, &tempNNCSpec); err != nil {
		// caller will retry to update the CRD again
		return errors.Wrap(err, "executing UpdateSpec with NNC CLI")
	}

	logger.Printf("[ipam-pool-monitor] Increasing pool size: UpdateCRDSpec succeeded for spec %+v", tempNNCSpec)
	// start an alloc timer
	metric.StartPoolIncreaseTimer(pm.metastate.batch)
	// save the updated state to cachedSpec
	pm.spec = tempNNCSpec
	return nil
}

func (pm *Monitor) decreasePoolSize(ctx context.Context, state ipPoolState, target int64) error {
	// mark n number of IPs as pending
	var pendingIPAddresses map[string]cns.IPConfigurationStatus
	decreaseIPCountBy := pm.spec.RequestedIPCount - target
	newIpsMarkedAsPending := false
	if pm.metastate.notInUseCount == 0 || pm.metastate.notInUseCount < state.pendingRelease {
		logger.Printf("[ipam-pool-monitor] Marking IPs as PendingRelease, ipsToBeReleasedCount %d", decreaseIPCountBy)
		var err error
		if pendingIPAddresses, err = pm.httpService.MarkIPAsPendingRelease(int(decreaseIPCountBy)); err != nil {
			return errors.Wrap(err, "marking IPs that are pending release")
		}
		newIpsMarkedAsPending = true
	}
	tempNNCSpec := pm.createNNCSpecForCRD(target)
	if newIpsMarkedAsPending {
		// cache the updatingPendingRelease so that we dont re-set new IPs to PendingRelease in case UpdateCRD call fails
		pm.metastate.notInUseCount = int64(len(tempNNCSpec.IPsNotInUse))
	}

	// we decrease the IP count on a best-effort basis; if we were unable to release the required number for the new lower
	// requested IP count, we should correct the requested IP count that we push to DNC-RC to match our real released IP count.
	logger.Printf("[ipam-pool-monitor] Releasing IPCount in this batch %d, updatingPendingIpsNotInUse count %d", len(pendingIPAddresses), pm.metastate.notInUseCount)
	tempNNCSpec.RequestedIPCount += decreaseIPCountBy - int64(len(pendingIPAddresses))
	logger.Printf("[ipam-pool-monitor] Decreasing pool size, pool %+v, spec %+v", state, tempNNCSpec)

	_, err := pm.nnccli.UpdateSpec(ctx, &tempNNCSpec)
	if err != nil {
		// caller will retry to update the CRD again
		return errors.Wrap(err, "executing UpdateSpec with NNC CLI")
	}

	logger.Printf("[ipam-pool-monitor] Decreasing pool size: UpdateCRDSpec succeeded for spec %+v", tempNNCSpec)
	// start a dealloc timer
	metric.StartPoolDecreaseTimer(pm.metastate.batch)

	// save the updated state to cachedSpec
	pm.spec = tempNNCSpec

	// clear the updatingPendingIpsNotInUse, as we have Updated the CRD
	logger.Printf("[ipam-pool-monitor] cleaning the updatingPendingIpsNotInUse, existing length %d", pm.metastate.notInUseCount)
	pm.metastate.notInUseCount = 0
	return nil
}

// cleanPendingRelease removes IPs from the cache and CRD if the request controller has reconciled
// CNS state and the pending IP release map is empty.
func (pm *Monitor) cleanPendingRelease(ctx context.Context) error {
	tempNNCSpec := pm.createNNCSpecForCRD(pm.spec.RequestedIPCount)
	_, err := pm.nnccli.UpdateSpec(ctx, &tempNNCSpec)
	if err != nil {
		// caller will retry to update the CRD again
		return errors.Wrap(err, "executing UpdateSpec with NNC CLI")
	}

	logger.Printf("[ipam-pool-monitor] cleanPendingRelease: UpdateCRDSpec succeeded for spec %+v", tempNNCSpec)

	// save the updated state to cachedSpec
	pm.spec = tempNNCSpec
	return nil
}

// createNNCSpecForCRD translates CNS's map of IPs to be released and requested IP count into an NNC Spec.
func (pm *Monitor) createNNCSpecForCRD(target int64) v1alpha.NodeNetworkConfigSpec {
	// Get All Pending IPs from CNS and populate it again.
	pendingIPs := pm.httpService.GetPendingReleaseIPConfigs()
	spec := v1alpha.NodeNetworkConfigSpec{
		RequestedIPCount: target,
		IPsNotInUse:      make([]string, len(pendingIPs)),
	}
	for i := range pendingIPs {
		spec.IPsNotInUse[i] = pendingIPs[i].ID
	}
	return spec
}

// GetStateSnapshot gets a snapshot of the IPAMPoolMonitor struct.
func (pm *Monitor) GetStateSnapshot() cns.IpamPoolMonitorStateSnapshot {
	spec, state := pm.spec, pm.metastate
	return cns.IpamPoolMonitorStateSnapshot{
		MinimumFreeIps:           int64(state.buffer * float64(state.batch)),
		UpdatingIpsNotInUseCount: state.notInUseCount,
		CachedNNC: v1alpha.NodeNetworkConfig{
			Spec: spec,
		},
	}
}

// GenerateARMID uses the Subnet ARM ID format to populate the ARM ID with the metadata.
// If either of the metadata attributes are empty, then the ARM ID will be an empty string.
func GenerateARMID(nc *v1alpha.NetworkContainer) string {
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

// CalculateMinFreeIPs calculates the minimum free IP quantity based on the Scaler
// in the passed NodeNetworkConfig.
// Half of odd batches are rounded up!
//
//nolint:gocritic // ignore hugeparam
func CalculateMinFreeIPs(scaler v1alpha.Scaler) int64 {
	return int64(float64(scaler.BatchSize)*(float64(scaler.RequestThresholdPercent)/100) + .5) //nolint:gomnd // it's a percent
}

// CalculateMaxFreeIPs calculates the maximum free IP quantity based on the Scaler
// in the passed NodeNetworkConfig.
// Half of odd batches are rounded up!
//
//nolint:gocritic // ignore hugeparam
func CalculateMaxFreeIPs(scaler v1alpha.Scaler) int64 {
	return int64(float64(scaler.BatchSize)*(float64(scaler.ReleaseThresholdPercent)/100) + .5) //nolint:gomnd // it's a percent
}
