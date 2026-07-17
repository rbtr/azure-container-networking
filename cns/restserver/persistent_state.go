package restserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-container-networking/cns"
	persistentstate "github.com/Azure/azure-container-networking/cns/state"
	"github.com/Azure/azure-container-networking/cns/types"
	"github.com/Azure/azure-container-networking/common"
)

func (service *HTTPRestService) requestIPConfigsWithPersistentStateLocked(
	ctx context.Context,
	request cns.IPConfigsRequest,
	podInfo cns.PodInfo,
) ([]cns.PodIpInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("ip config request canceled: %w", err)
	}

	podIPInfo, newlyAssigned, err := requestIPConfigsHelperUntransacted(service, request, podInfo) //nolint:contextcheck // Legacy helper chain has no context parameter.
	if err != nil {
		return podIPInfo, err
	}
	endpointState, _, err := buildEndpointState(service.EndpointState, request, podInfo, podIPInfo)
	if err != nil {
		if newlyAssigned {
			_ = service.releaseIPConfigsUntransacted(podInfo)
		}
		return podIPInfo, err
	}
	endpoint := endpointState[podInfo.InfraContainerID()]
	assignment := persistentstate.AssignmentRecord{
		Pod: persistentstate.PodIdentity{
			PodKey:           podInfo.Key(),
			InfraContainerID: podInfo.InfraContainerID(),
			InterfaceID:      podInfo.InterfaceID(),
			PodName:          podInfo.Name(),
			PodNamespace:     podInfo.Namespace(),
		},
		IPIDs: append([]string(nil), service.PodIPIDByPodInterfaceKey[podInfo.Key()]...),
	}

	service.reachFaultPoint(faultPointAddBeforeEndpointCommit, podInfo.Name(), podInfo.Namespace())
	if err := service.persistentState.AssignEndpoint(
		ctx,
		assignment,
		endpointToPersistent(endpoint),
		time.Now(),
		endpointDeleteIntentTTL,
	); err != nil {
		if newlyAssigned {
			if rollbackErr := service.releaseIPConfigsUntransacted(podInfo); rollbackErr != nil {
				if restoreErr := service.restorePersistentState(ctx); restoreErr != nil {
					return podIPInfo, fmt.Errorf(
						"%w: %w; rolling back newly assigned IPs: %w; restoring committed state: %w",
						ErrEndpointStateUpdate,
						err,
						rollbackErr,
						restoreErr,
					)
				}
				return podIPInfo, fmt.Errorf("%w: %w; rolling back newly assigned IPs: %w", ErrEndpointStateUpdate, err, rollbackErr)
			}
		}
		if errors.Is(err, persistentstate.ErrDeleteIntent) {
			return podIPInfo, fmt.Errorf("%w: %w", ErrEndpointDeleteIntent, err)
		}
		return podIPInfo, fmt.Errorf("%w: %w", ErrEndpointStateUpdate, err)
	}

	service.EndpointState = endpointState
	delete(service.EndpointDeleteIntents, podInfo.InfraContainerID())
	service.persistentStateGeneration++
	return podIPInfo, nil
}

func (service *HTTPRestService) releaseIPConfigsWithPersistentStateLocked(ctx context.Context, podInfo cns.PodInfo) error {
	now := time.Now()
	if err := service.persistentState.ReleaseEndpoint(
		ctx,
		podInfo.Key(),
		podInfo.InfraContainerID(),
		persistentstate.DeleteIntent{CreatedAt: now},
		endpointDeleteIntentTTL,
	); err != nil {
		return fmt.Errorf("releasing persistent endpoint state: %w", err)
	}

	service.reachFaultPoint(faultPointDeleteAfterIntentCommit, podInfo.Name(), podInfo.Namespace())
	delete(service.EndpointState, podInfo.InfraContainerID())
	for containerID, intent := range service.EndpointDeleteIntents {
		if endpointDeleteIntentExpired(intent, now) {
			delete(service.EndpointDeleteIntents, containerID)
		}
	}
	service.EndpointDeleteIntents[podInfo.InfraContainerID()] = EndpointDeleteIntent{CreatedAt: now}

	if err := service.releaseIPConfigsUntransacted(podInfo); err != nil {
		if restoreErr := service.restorePersistentState(ctx); restoreErr != nil {
			return fmt.Errorf("applying committed IP release: %w; restoring committed state: %w", err, restoreErr)
		}
		return nil
	}
	service.persistentStateGeneration++
	return nil
}

func (service *HTTPRestService) restorePersistentState(ctx context.Context) error {
	snapshot, err := service.persistentState.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("reading persistent state snapshot: %w", err)
	}
	return service.applyPersistentSnapshot(snapshot)
}

func (service *HTTPRestService) applyPersistentSnapshot(snapshot persistentstate.Snapshot) error {
	serviceState := &httpRestServiceState{
		Location:                         snapshot.Metadata.Location,
		NetworkType:                      snapshot.Metadata.NetworkType,
		OrchestratorType:                 snapshot.Metadata.OrchestratorType,
		NodeID:                           snapshot.Metadata.NodeID,
		Initialized:                      snapshot.Metadata.Initialized,
		ContainerIDByOrchestratorContext: make(map[string]*ncList, len(snapshot.OrchestratorContexts)),
		ContainerStatus:                  make(map[string]containerstatus, len(snapshot.NetworkContainers)),
		Networks:                         make(map[string]*networkInfo, len(snapshot.Networks)),
		TimeStamp:                        snapshot.Metadata.TimeStamp,
		joinedNetworks:                   make(map[string]struct{}),
		primaryInterface:                 service.state.primaryInterface,
		PnpIDByMacAddress:                make(map[string]string, len(snapshot.PnPIDByMAC)),
	}

	for key, ncIDs := range snapshot.OrchestratorContexts {
		value := ncList(strings.Join(ncIDs, ","))
		serviceState.ContainerIDByOrchestratorContext[key] = &value
	}
	for mac, pnpID := range snapshot.PnPIDByMAC {
		serviceState.PnpIDByMacAddress[mac] = pnpID
	}
	for name, network := range snapshot.Networks {
		serviceState.Networks[name] = &networkInfo{
			NetworkName: name,
			NicInfo:     network.NicInfo,
			Options:     network.Options,
		}
	}
	for ncID := range snapshot.NetworkContainers {
		record := snapshot.NetworkContainers[ncID]
		request := record.Request
		request.NetworkContainerid = ncID
		request.SecondaryIPConfigs = make(map[string]cns.SecondaryIPConfig)
		for ipID, ip := range snapshot.IPs {
			if ip.NCID == ncID {
				request.SecondaryIPConfigs[ipID] = cns.SecondaryIPConfig{
					IPAddress: ip.IPAddress,
					NCVersion: ip.NCVersion,
				}
			}
		}
		serviceState.ContainerStatus[ncID] = containerstatus{
			ID:                            record.ID,
			VMVersion:                     record.VMVersion,
			HostVersion:                   record.HostVersion,
			CreateNetworkContainerRequest: request,
			VfpUpdateComplete:             record.VFPUpdateComplete,
		}
	}

	podIPConfigState := make(map[string]cns.IPConfigurationStatus, len(snapshot.IPs))
	for ipID, ip := range snapshot.IPs {
		ncRecord, ok := snapshot.NetworkContainers[ip.NCID]
		if !ok {
			return fmt.Errorf("%w: IP %q references missing NC %q", persistentstate.ErrInconsistentState, ipID, ip.NCID)
		}
		hostVersion, err := strconv.Atoi(ncRecord.HostVersion)
		if err != nil {
			return fmt.Errorf("parsing host version for NC %q: %w", ip.NCID, err)
		}
		ipState := types.Available
		if hostVersion < ip.NCVersion {
			ipState = types.PendingProgramming
		}
		status := cns.IPConfigurationStatus{
			NCID:      ip.NCID,
			ID:        ip.ID,
			IPAddress: ip.IPAddress,
		}
		status.WithStateMiddleware(stateTransitionMiddleware)
		status.SetState(ipState)
		podIPConfigState[ipID] = status
	}

	podIPIDsByKey := make(map[string][]string, len(snapshot.Assignments))
	for podKey, assignment := range snapshot.Assignments {
		podInfo := cns.NewPodInfo(
			assignment.Pod.InfraContainerID,
			assignment.Pod.InterfaceID,
			assignment.Pod.PodName,
			assignment.Pod.PodNamespace,
		)
		podIPIDsByKey[podKey] = append([]string(nil), assignment.IPIDs...)
		for _, ipID := range assignment.IPIDs {
			status, ok := podIPConfigState[ipID]
			if !ok {
				return fmt.Errorf("%w: assignment %q references missing IP %q", persistentstate.ErrInconsistentState, podKey, ipID)
			}
			status.PodInfo = podInfo
			status.SetState(types.Assigned)
			podIPConfigState[ipID] = status
		}
	}

	endpoints := make(map[string]*EndpointInfo, len(snapshot.Endpoints))
	for containerID, endpoint := range snapshot.Endpoints {
		endpoints[containerID] = endpointFromPersistent(endpoint)
	}
	intents := make(map[string]EndpointDeleteIntent, len(snapshot.DeleteIntents))
	for containerID, intent := range snapshot.DeleteIntents {
		intents[containerID] = EndpointDeleteIntent{CreatedAt: intent.CreatedAt}
	}

	service.state = serviceState
	service.PnpIDByMacAddress = serviceState.PnpIDByMacAddress
	service.PodIPConfigState = podIPConfigState
	service.PodIPIDByPodInterfaceKey = podIPIDsByKey
	service.EndpointState = endpoints
	service.EndpointDeleteIntents = intents
	service.persistentStateGeneration = snapshot.Metadata.Generation
	return nil
}

func (service *HTTPRestService) persistentSnapshot() persistentstate.Snapshot {
	snapshot := persistentstate.NewSnapshot()
	snapshot.Metadata = persistentstate.Metadata{
		SchemaVersion:    persistentstate.SchemaVersion,
		Authority:        persistentstate.AuthorityBolt,
		Generation:       service.persistentStateGeneration,
		OrchestratorType: service.state.OrchestratorType,
		NodeID:           service.state.NodeID,
		Location:         service.state.Location,
		NetworkType:      service.state.NetworkType,
		Initialized:      service.state.Initialized,
		TimeStamp:        service.state.TimeStamp,
	}

	for ncID := range service.state.ContainerStatus {
		current := service.state.ContainerStatus[ncID]
		snapshot.NetworkContainers[ncID] = persistentstate.NewNetworkContainerRecord(
			current.ID,
			current.VMVersion,
			current.HostVersion,
			current.VfpUpdateComplete,
			current.CreateNetworkContainerRequest,
		)
		for ipID, ip := range current.CreateNetworkContainerRequest.SecondaryIPConfigs {
			snapshot.IPs[ipID] = persistentstate.IPRecord{
				ID:        ipID,
				IPAddress: ip.IPAddress,
				NCID:      ncID,
				NCVersion: ip.NCVersion,
			}
		}
	}
	for name, network := range service.state.Networks {
		if network == nil {
			continue
		}
		snapshot.Networks[name] = persistentstate.NetworkRecord{
			NetworkName: name,
			NicInfo:     network.NicInfo,
			Options:     network.Options,
		}
	}
	for key, list := range service.state.ContainerIDByOrchestratorContext {
		if list == nil || *list == "" {
			continue
		}
		snapshot.OrchestratorContexts[key] = strings.Split(string(*list), ",")
	}
	for mac, pnpID := range service.state.PnpIDByMacAddress {
		snapshot.PnPIDByMAC[mac] = pnpID
	}
	return snapshot
}

func (service *HTTPRestService) persistDurableState(ctx context.Context) error {
	snapshot := service.persistentSnapshot()
	if err := service.persistentState.ReplaceDurableState(ctx, snapshot); err != nil {
		if restoreErr := service.restorePersistentState(ctx); restoreErr != nil {
			return fmt.Errorf("persisting durable state: %w; restoring committed state: %w", err, restoreErr)
		}
		return fmt.Errorf("replacing durable persistent state: %w", err)
	}
	service.persistentStateGeneration++
	return nil
}

func (service *HTTPRestService) ReplacePersistentEndpoints(ctx context.Context, endpoints map[string]*EndpointInfo) error {
	records := make(map[string]persistentstate.EndpointRecord, len(endpoints))
	for containerID, endpoint := range endpoints {
		records[containerID] = endpointToPersistent(endpoint)
	}
	if err := service.persistentState.ReplaceManagedEndpoints(ctx, records); err != nil {
		return fmt.Errorf("replacing managed endpoint state: %w", err)
	}
	return service.restorePersistentState(ctx)
}

func (service *HTTPRestService) HandleDebugPersistentState(w http.ResponseWriter, r *http.Request) {
	if service.persistentState == nil {
		http.Error(w, "persistent state store is not enabled", http.StatusNotFound)
		return
	}
	snapshot, err := service.persistentState.Snapshot(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	storage, err := service.persistentState.StorageMetadata()
	if err != nil {
		http.Error(w, "failed to inspect persistent state storage", http.StatusInternalServerError)
		return
	}
	response := persistentstate.DebugResponse{
		Snapshot: snapshot,
		Storage:  storage,
	}
	if err := common.Encode(w, &response); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func endpointToPersistent(endpoint *EndpointInfo) persistentstate.EndpointRecord {
	if endpoint == nil {
		return persistentstate.EndpointRecord{}
	}
	record := persistentstate.EndpointRecord{
		PodName:       endpoint.PodName,
		PodNamespace:  endpoint.PodNamespace,
		IfnameToIPMap: make(map[string]*persistentstate.IPInfoRecord, len(endpoint.IfnameToIPMap)),
	}
	for ifName, ipInfo := range endpoint.IfnameToIPMap {
		if ipInfo == nil {
			continue
		}
		record.IfnameToIPMap[ifName] = &persistentstate.IPInfoRecord{
			IPv4:               append([]net.IPNet(nil), ipInfo.IPv4...),
			IPv6:               append([]net.IPNet(nil), ipInfo.IPv6...),
			HNSEndpointID:      ipInfo.HnsEndpointID,
			HNSNetworkID:       ipInfo.HnsNetworkID,
			HostVethName:       ipInfo.HostVethName,
			MACAddress:         ipInfo.MacAddress,
			NetworkContainerID: ipInfo.NetworkContainerID,
			NICType:            ipInfo.NICType,
		}
	}
	return record
}

func endpointFromPersistent(endpoint persistentstate.EndpointRecord) *EndpointInfo {
	record := &EndpointInfo{
		PodName:       endpoint.PodName,
		PodNamespace:  endpoint.PodNamespace,
		IfnameToIPMap: make(map[string]*IPInfo, len(endpoint.IfnameToIPMap)),
	}
	for ifName, ipInfo := range endpoint.IfnameToIPMap {
		if ipInfo == nil {
			continue
		}
		record.IfnameToIPMap[ifName] = &IPInfo{
			IPv4:               append([]net.IPNet(nil), ipInfo.IPv4...),
			IPv6:               append([]net.IPNet(nil), ipInfo.IPv6...),
			HnsEndpointID:      ipInfo.HNSEndpointID,
			HnsNetworkID:       ipInfo.HNSNetworkID,
			HostVethName:       ipInfo.HostVethName,
			MacAddress:         ipInfo.MACAddress,
			NetworkContainerID: ipInfo.NetworkContainerID,
			NICType:            ipInfo.NICType,
		}
	}
	return record
}
