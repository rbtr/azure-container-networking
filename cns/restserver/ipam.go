// Copyright 2017 Microsoft. All rights reserved.
// MIT License

package restserver

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/filter"
	"github.com/Azure/azure-container-networking/cns/logger"
)

// used to request an IPConfig from the CNS state
func (service *HTTPRestService) requestIPConfigHandler(w http.ResponseWriter, r *http.Request) {
	var (
		err             error
		ipconfigRequest cns.IPConfigRequest
		podIpInfo       cns.PodIpInfo
		returnCode      int
		returnMessage   string
	)

	err = service.Listener.Decode(w, r, &ipconfigRequest)
	operationName := "requestIPConfigHandler"
	logger.Request(service.Name+operationName, ipconfigRequest, err)
	if err != nil {
		return
	}

	// retrieve ipconfig from nc
	_, returnCode, returnMessage = service.validateIpConfigRequest(ipconfigRequest)
	if returnCode == Success {
		if podIpInfo, err = requestIPConfigHelper(service, ipconfigRequest); err != nil {
			returnCode = FailedToAllocateIpConfig
			returnMessage = fmt.Sprintf("AllocateIPConfig failed: %v, IP config request is %s", err, ipconfigRequest)
		}
	}

	resp := cns.Response{
		ReturnCode: returnCode,
		Message:    returnMessage,
	}

	reserveResp := &cns.IPConfigResponse{
		Response: resp,
	}
	reserveResp.PodIpInfo = podIpInfo

	err = service.Listener.Encode(w, &reserveResp)
	logger.ResponseEx(service.Name+operationName, ipconfigRequest, reserveResp, resp.ReturnCode, ReturnCodeToString(resp.ReturnCode), err)
}

func (service *HTTPRestService) releaseIPConfigHandler(w http.ResponseWriter, r *http.Request) {
	req := cns.IPConfigRequest{}
	resp := cns.Response{}
	var err error

	defer func() {
		err = service.Listener.Encode(w, &resp)
		logger.ResponseEx(service.Name, req, resp, resp.ReturnCode, ReturnCodeToString(resp.ReturnCode), err)
	}()

	err = service.Listener.Decode(w, r, &req)
	logger.Request(service.Name+"releaseIPConfigHandler", req, err)
	if err != nil {
		resp.ReturnCode = UnexpectedError
		resp.Message = err.Error()
		logger.Errorf("releaseIPConfigHandler decode failed becase %v, release IP config info %s", resp.Message, req)
		return
	}

	var podInfo cns.PodInfo
	podInfo, resp.ReturnCode, resp.Message = service.validateIpConfigRequest(req)

	if err = service.releaseIPConfig(podInfo); err != nil {
		resp.ReturnCode = UnexpectedError
		resp.Message = err.Error()
		logger.Errorf("releaseIPConfigHandler releaseIPConfig failed because %v, release IP config info %s", resp.Message, req)
		return
	}
}

// MarkIPAsPendingRelease will set the IPs which are in PendingProgramming or Available to PendingRelease state
// It will try to update [totalIpsToRelease] number of ips.
func (service *HTTPRestService) MarkIPAsPendingRelease(totalIPsToRelease int) (map[string]cns.IPConfigurationStatus, error) {
	service.Lock()
	defer service.Unlock()

	var releasedIPs = map[string]cns.IPConfigurationStatus{}
	var availableIPs, pendingProgrammingIPs []string

	for uuid, existingIPConfig := range service.PodIPConfigState {
		if existingIPConfig.State == cns.PendingProgramming {
			pendingProgrammingIPs = append(pendingProgrammingIPs, uuid)
		}
		if existingIPConfig.State == cns.Available {
			availableIPs = append(availableIPs, uuid)
		}
	}

	// we want to release the PendingProgrammingIPs first
	releaseableIPs := append(pendingProgrammingIPs, availableIPs...)

	// to release efficiently, we want to iterate through all of the releaseableIPs,
	// releasing up to the number of totalIPsToRelease.
	for i := 0; i < len(releaseableIPs) && len(releasedIPs) < totalIPsToRelease; i++ {
		uuid := releaseableIPs[i]
		releasedIPConfig, err := service.updateIPConfigStateUnsafe(uuid, cns.PendingRelease, service.PodIPConfigState[uuid].OrchestratorContext)
		if err != nil {
			logger.Errorf("error releasing IP %s: %v", uuid, err)
			continue
		}
		releasedIPs[uuid] = releasedIPConfig
	}

	var err error
	if len(releasedIPs) < totalIPsToRelease {
		err = fmt.Errorf("released %d/%d releaseable IPs, needed to release %d", len(releasedIPs), len(releaseableIPs), totalIPsToRelease)
	}
	logger.Printf("[MarkIPAsPendingRelease] Set total IPs to PendingRelease %d, expected %d", len(releasedIPs), totalIPsToRelease)
	return releasedIPs, err
}

func (service *HTTPRestService) updateIPConfigStateUnsafe(ipId string, updatedState cns.IPConfigState, podInfo cns.PodInfo) (cns.IPConfigurationStatus, error) {
	ipConfig, ok := service.PodIPConfigState[ipId]
	if !ok {
		return ipConfig, fmt.Errorf("[updateIPConfigState] Failed to update state %s for the IPConfig. ID %s not found PodIPConfigState", updatedState, ipId)
	}
	logger.Printf("[updateIPConfigState] Changing IpId [%s] state to [%s], orchestratorContext [%s]. Current config [%+v]", ipId, updatedState, podInfo, ipConfig)
	ipConfig.State = updatedState
	ipConfig.PodInfo = podInfo
	service.PodIPConfigState[ipId] = ipConfig
	return ipConfig, nil
}

// MarkIpsAsAvailableUntransacted will update pending programming IPs to available if NMAgent side's programmed nc version keep up with nc version.
// Note: this func is an untransacted API as the caller will take a Service lock
func (service *HTTPRestService) MarkIpsAsAvailableUnsafe(ncID string, newHostNCVersion int) {
	// Check whether it exist in service state and get the related nc info
	if ncInfo, exist := service.state.ContainerStatus[ncID]; !exist {
		logger.Errorf("Can't find NC with ID %s in service state, stop updating its pending programming IP status", ncID)
	} else {
		previousHostNCVersion, err := strconv.Atoi(ncInfo.HostVersion)
		if err != nil {
			logger.Printf("[MarkIpsAsAvailableUntransacted] Get int value from ncInfo.HostVersion %s failed: %v, can't proceed", ncInfo.HostVersion, err)
			return
		}
		// We only need to handle the situation when dnc nc version is larger than programmed nc version
		if previousHostNCVersion < newHostNCVersion {
			for uuid, secondaryIPConfigs := range ncInfo.CreateNetworkContainerRequest.SecondaryIPConfigs {
				if ipConfigStatus, exist := service.PodIPConfigState[uuid]; !exist {
					logger.Errorf("IP %s with uuid as %s exist in service state Secondary IP list but can't find in PodIPConfigState", ipConfigStatus.IPAddress, uuid)
				} else if ipConfigStatus.State == cns.PendingProgramming && secondaryIPConfigs.NCVersion <= newHostNCVersion {
					_, err := service.updateIPConfigStateUnsafe(uuid, cns.Available, nil)
					if err != nil {
						logger.Errorf("Error updating IPConfig [%+v] state to Available, err: %+v", ipConfigStatus, err)
					}

					// Following 2 sentence assign new host version to secondary ip config.
					secondaryIPConfigs.NCVersion = newHostNCVersion
					ncInfo.CreateNetworkContainerRequest.SecondaryIPConfigs[uuid] = secondaryIPConfigs
					logger.Printf("Change ip %s with uuid %s from pending programming to %s, current secondary ip configs is %+v", ipConfigStatus.IPAddress, uuid, cns.Available,
						ncInfo.CreateNetworkContainerRequest.SecondaryIPConfigs[uuid])
				}
			}
		}
	}
}

func (service *HTTPRestService) GetPodIPConfigState() map[string]cns.IPConfigurationStatus {
	service.RLock()
	defer service.RUnlock()
	return service.PodIPConfigState
}

func (service *HTTPRestService) getPodIPIDByOrchestratorContexthandler(w http.ResponseWriter, r *http.Request) {
	service.RLock()
	defer service.RUnlock()
	resp := cns.GetPodContextResponse{
		PodContext: service.PodIPIDByPodInterfaceKey,
	}
	err := service.Listener.Encode(w, &resp)
	logger.Response(service.Name, resp, resp.Response.ReturnCode, ReturnCodeToString(resp.Response.ReturnCode), err)
}

func (service *HTTPRestService) getHTTPRestDataHandler(w http.ResponseWriter, r *http.Request) {
	service.RLock()
	defer service.RUnlock()
	resp := GetHTTPServiceDataResponse{
		HttpRestServiceData: HttpRestServiceData{
			PodIPIDByPodInterfaceKey: service.PodIPIDByPodInterfaceKey,
			PodIPConfigState:         service.PodIPConfigState,
			IPAMPoolMonitor:          service.IPAMPoolMonitor.GetStateSnapshot(),
		},
	}
	err := service.Listener.Encode(w, &resp)
	logger.Response(service.Name, resp, resp.Response.ReturnCode, ReturnCodeToString(resp.Response.ReturnCode), err)
}

func (service *HTTPRestService) getIPAddressesHandler(w http.ResponseWriter, r *http.Request) {
	service.RLock()
	defer service.RUnlock()

	var req cns.GetIPAddressesRequest
	var resp cns.GetIPAddressStatusResponse

	defer func() {
		err := service.Listener.Encode(w, &resp)
		logger.ResponseEx(service.Name, req, resp, resp.Response.ReturnCode, ReturnCodeToString(resp.Response.ReturnCode), err)
	}()

	err := service.Listener.Decode(w, r, &req)
	if err != nil {
		resp.Response.Message = err.Error()
		resp.Response.ReturnCode = UnexpectedError
		logger.Errorf("getIPAddressesHandler decode failed because %v, GetIPAddressesRequest is %v", resp.Response.Message, req)
		return
	}

	// Get all IPConfigs matching a state, and append to a slice of IPAddressState
	resp.IPConfigurationStatus = filter.MatchAnyIPConfigState(service.PodIPConfigState, filter.PredicatesForStates(req.IPConfigStateFilter...)...)
}

// GetAllocatedIPConfigs returns a filtered list of IPs which are in
// Allocated State
func (service *HTTPRestService) GetAllocatedIPConfigs() []cns.IPConfigurationStatus {
	service.RLock()
	defer service.RUnlock()
	return filter.MatchAnyIPConfigState(service.PodIPConfigState, filter.StateAllocated)
}

// GetAvailableIPConfigs returns a filtered list of IPs which are in
// Available State
func (service *HTTPRestService) GetAvailableIPConfigs() []cns.IPConfigurationStatus {
	service.RLock()
	defer service.RUnlock()
	return filter.MatchAnyIPConfigState(service.PodIPConfigState, filter.StateAvailable)
}

// GetPendingProgramIPConfigs returns a filtered list of IPs which are in
// PendingProgramming State.
func (service *HTTPRestService) GetPendingProgramIPConfigs() []cns.IPConfigurationStatus {
	service.RLock()
	defer service.RUnlock()
	return filter.MatchAnyIPConfigState(service.PodIPConfigState, filter.StatePendingProgramming)
}

// GetPendingReleaseIPConfigs returns a filtered list of IPs which are in
// PendingRelease State.
func (service *HTTPRestService) GetPendingReleaseIPConfigs() []cns.IPConfigurationStatus {
	service.RLock()
	defer service.RUnlock()
	return filter.MatchAnyIPConfigState(service.PodIPConfigState, filter.StatePendingRelease)
}

//SetIPConfigAsAllocated takes a lock of the service, and sets the ipconfig in the CNS state as allocated, does not take a lock
func (service *HTTPRestService) setIPConfigAsAllocatedUnsafe(ipconfig cns.IPConfigurationStatus, podInfo cns.PodInfo) (cns.IPConfigurationStatus, error) {
	ipconfig, err := service.updateIPConfigStateUnsafe(ipconfig.ID, cns.Allocated, podInfo)
	if err != nil {
		return cns.IPConfigurationStatus{}, err
	}

	service.PodIPIDByPodInterfaceKey[podInfo.Key()] = ipconfig.ID
	return ipconfig, nil
}

//SetIPConfigAsAllocated and sets the ipconfig in the CNS state as allocated, does not take a lock
func (service *HTTPRestService) setIPConfigAsAvailableUnsafe(ipconfig cns.IPConfigurationStatus, podInfo cns.PodInfo) (cns.IPConfigurationStatus, error) {
	ipconfig, err := service.updateIPConfigStateUnsafe(ipconfig.ID, cns.Available, nil)
	if err != nil {
		return cns.IPConfigurationStatus{}, err
	}

	delete(service.PodIPIDByPodInterfaceKey, podInfo.Key())
	logger.Printf("[setIPConfigAsAvailable] Deleted outdated pod info %s from PodIPIDByOrchestratorContext since IP %s with ID %s will be released and set as Available", podInfo.Key(), ipconfig.IPAddress, ipconfig.ID)
	return ipconfig, nil
}

////SetIPConfigAsAllocated takes a lock of the service, and sets the ipconfig in the CNS stateas Available
// Todo - CNI should also pass the IPAddress which needs to be released to validate if that is the right IP allcoated
// in the first place.
func (service *HTTPRestService) releaseIPConfig(podInfo cns.PodInfo) error {
	service.Lock()
	defer service.Unlock()

	ipID, ok := service.PodIPIDByPodInterfaceKey[podInfo.Key()]
	if !ok {
		logger.Errorf("[releaseIPConfig] SetIPConfigAsAvailable failed to release, no allocation found for pod [%+v]", podInfo)
		return nil
	}

	ipconfig, ok := service.PodIPConfigState[ipID]
	if !ok {
		logger.Errorf("[releaseIPConfig] Failed to get release ipconfig %+v and pod info is %+v. Pod to IPID exists, but IPID to IPConfig doesn't exist, CNS State potentially corrupt", ipconfig.IPAddress, podInfo)
		return fmt.Errorf("[releaseIPConfig] releaseIPConfig failed. IPconfig %+v and pod info is %+v. Pod to IPID exists, but IPID to IPConfig doesn't exist, CNS State potentially corrupt", ipconfig.IPAddress, podInfo)
	}

	logger.Printf("[releaseIPConfig] Releasing IP %+v for pod %+v", ipconfig.IPAddress, podInfo)
	_, err := service.setIPConfigAsAvailableUnsafe(ipconfig, podInfo)
	if err != nil {
		return fmt.Errorf("[releaseIPConfig] failed to mark IPConfig [%+v] as Available. err: %w", ipconfig, err)
	}
	logger.Printf("[releaseIPConfig] Released IP %+v for pod %+v", ipconfig.IPAddress, podInfo)
	return nil
}

// called when CNS is starting up and there are existing ipconfigs in the CRD that are marked as pending
func (service *HTTPRestService) MarkExistingIPsAsPending(pendingIPIDs []string) error {
	service.Lock()
	defer service.Unlock()

	for _, id := range pendingIPIDs {
		ipconfig, ok := service.PodIPConfigState[id]
		if !ok {
			logger.Errorf("Inconsistent state, ipconfig with ID [%v] marked as pending release, but does not exist in state", id)
			continue
		}

		if ipconfig.State == cns.Allocated { // TODO(rbtr): this early return aborts processing the rest of the input
			return fmt.Errorf("failed to mark IP [%v] as pending, currently allocated", id)
		}

		logger.Printf("[MarkExistingIPsAsPending]: Marking IP [%+v] to PendingRelease", ipconfig)
		ipconfig.State = cns.PendingRelease
		service.PodIPConfigState[id] = ipconfig
	}
	return nil
}

// GetExistingIPConfig returns a tuple of an IP config, a bool that it exists, and an error if encountered.
func (service *HTTPRestService) GetExistingIPConfig(podInfo cns.PodInfo) (cns.PodIpInfo, bool, error) {
	service.RLock()
	defer service.RUnlock()

	var podIpInfo cns.PodIpInfo
	ipID, ok := service.PodIPIDByPodInterfaceKey[podInfo.Key()]
	if !ok || ipID == "" {
		return podIpInfo, false, nil
	}

	if ipState, ok := service.PodIPConfigState[ipID]; ok {
		err := service.populateIpConfigInfoUntransacted(ipState, &podIpInfo)
		return podIpInfo, true, err
	}

	logger.Errorf("Failed to get existing ipconfig. Pod to IPID exists, but IPID to IPConfig doesn't exist, CNS State potentially corrupt")
	return podIpInfo, false, fmt.Errorf("failed to get existing ipconfig. Pod to IPID exists, but IPID to IPConfig doesn't exist, CNS State potentially corrupt")
}

func (service *HTTPRestService) AllocateDesiredIPConfig(podInfo cns.PodInfo, desiredIpAddress string) (cns.PodIpInfo, error) {
	var podIpInfo cns.PodIpInfo
	service.Lock()
	defer service.Unlock()

	found := false
	for _, ipConfig := range service.PodIPConfigState {
		if ipConfig.IPAddress == desiredIpAddress {
			if ipConfig.State == cns.Allocated {
				// This IP has already been allocated, if it is allocated to same pod, then return the same
				// IPconfiguration
				if ipConfig.PodInfo.Key() == podInfo.Key() {
					logger.Printf("[AllocateDesiredIPConfig]: IP Config [%+v] is already allocated to this Pod [%+v]", ipConfig, podInfo)
					found = true
				} else {
					return podIpInfo, fmt.Errorf("[AllocateDesiredIPConfig] Desired IP is already allocated %+v, requested for pod %+v", ipConfig, podInfo)
				}
			} else if ipConfig.State == cns.Available || ipConfig.State == cns.PendingProgramming {
				// This race can happen during restart, where CNS state is lost and thus we have lost the NC programmed version
				// As part of reconcile, we mark IPs as Allocated which are already allocated to PODs (listed from APIServer)
				_, err := service.setIPConfigAsAllocatedUnsafe(ipConfig, podInfo)
				if err != nil {
					return podIpInfo, err
				}
				found = true
			} else {
				return podIpInfo, fmt.Errorf("[AllocateDesiredIPConfig] Desired IP is not available %+v", ipConfig)
			}

			if found {
				err := service.populateIpConfigInfoUntransacted(ipConfig, &podIpInfo)
				return podIpInfo, err
			}
		}
	}
	return podIpInfo, fmt.Errorf("Requested IP not found in pool")
}

func (service *HTTPRestService) AllocateAnyAvailableIPConfig(podInfo cns.PodInfo) (cns.PodIpInfo, error) {

	service.Lock()
	defer service.Unlock()

	var podIpInfo cns.PodIpInfo
	for _, ipState := range service.PodIPConfigState {
		if ipState.State != cns.Available {
			continue
		}
		_, err := service.setIPConfigAsAllocatedUnsafe(ipState, podInfo)
		if err != nil {
			return podIpInfo, err
		}

		err = service.populateIpConfigInfoUntransacted(ipState, &podIpInfo)
		if err != nil {
			return podIpInfo, err
		}

		return podIpInfo, err
	}

	return podIpInfo, fmt.Errorf("no more free IPs available, waiting on Azure CNS to allocated more IP's...")
}

// If IPConfig is already allocated for pod, it returns that else it returns one of the available ipconfigs.
func requestIPConfigHelper(service *HTTPRestService, req cns.IPConfigRequest) (cns.PodIpInfo, error) {
	var (
		podIpInfo cns.PodIpInfo
		isExist   bool
	)

	// check if ipconfig already allocated for this pod and return if exists or error
	// if error, ipstate is nil, if exists, ipstate is not nil and error is nil
	podInfo, err := cns.NewPodInfoFromIPConfigRequest(req)
	if err != nil {
		return podIpInfo, err
	}

	if podIpInfo, isExist, err = service.GetExistingIPConfig(podInfo); err != nil || isExist {
		return podIpInfo, err
	}

	// return desired IPConfig
	if req.DesiredIPAddress != "" {
		return service.AllocateDesiredIPConfig(podInfo, req.DesiredIPAddress)
	}

	// return any free IPConfig
	return service.AllocateAnyAvailableIPConfig(podInfo)
}
