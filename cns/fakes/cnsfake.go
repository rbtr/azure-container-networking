//go:build !ignore_uncovered
// +build !ignore_uncovered

package fakes

import (
	"errors"
	"sync"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/types"
)

type StringStack struct {
	sync.Mutex
	items []string
}

func NewStack() *StringStack {
	return &StringStack{items: make([]string, 0)}
}

func (stack *StringStack) Push(v string) {
	stack.Lock()
	defer stack.Unlock()

	stack.items = append(stack.items, v)
}

func (stack *StringStack) Pop() (string, error) {
	stack.Lock()
	defer stack.Unlock()

	length := len(stack.items)
	if length == 0 {
		return "", errors.New("Empty Stack")
	}

	res := stack.items[length-1]
	stack.items = stack.items[:length-1]
	return res, nil
}

type IPStatusManager struct {
	PendingProgramIPConfigStatus map[string]cns.IPConfigurationStatus
	AvailableIPConfigStatus      map[string]cns.IPConfigurationStatus
	AssignedIPConfigStatus       map[string]cns.IPConfigurationStatus
	PendingReleaseIPConfigStatus map[string]cns.IPConfigurationStatus
	AvailableIPIDStack           StringStack
	rwmutex                      sync.RWMutex
}

func NewIPStatusManager() *IPStatusManager {
	return &IPStatusManager{
		PendingProgramIPConfigStatus: make(map[string]cns.IPConfigurationStatus),
		AvailableIPConfigStatus:      make(map[string]cns.IPConfigurationStatus),
		AssignedIPConfigStatus:       make(map[string]cns.IPConfigurationStatus),
		PendingReleaseIPConfigStatus: make(map[string]cns.IPConfigurationStatus),
		AvailableIPIDStack:           StringStack{},
	}
}

func (ipm *IPStatusManager) AddIPConfigs(ipconfigs []cns.IPConfigurationStatus) {
	ipm.rwmutex.Lock()
	defer ipm.rwmutex.Unlock()
	for i := range ipconfigs {
		switch ipconfigs[i].GetState() {
		case types.PendingProgramming:
			ipm.PendingProgramIPConfigStatus[ipconfigs[i].ID] = ipconfigs[i]
		case types.Available:
			ipm.AvailableIPConfigStatus[ipconfigs[i].ID] = ipconfigs[i]
			ipm.AvailableIPIDStack.Push(ipconfigs[i].ID)
		case types.Assigned:
			ipm.AssignedIPConfigStatus[ipconfigs[i].ID] = ipconfigs[i]
		case types.PendingRelease:
			ipm.PendingReleaseIPConfigStatus[ipconfigs[i].ID] = ipconfigs[i]
		}
	}
}

func (ipm *IPStatusManager) RemovePendingReleaseIPConfigs(ipconfigNames []string) {
	ipm.rwmutex.Lock()
	defer ipm.rwmutex.Unlock()
	for _, name := range ipconfigNames {
		delete(ipm.PendingReleaseIPConfigStatus, name)
	}
}

func (ipm *IPStatusManager) ReserveIPConfig() (cns.IPConfigurationStatus, error) {
	ipm.rwmutex.Lock()
	defer ipm.rwmutex.Unlock()
	id, err := ipm.AvailableIPIDStack.Pop()
	if err != nil {
		return cns.IPConfigurationStatus{}, err
	}
	ipc := ipm.AvailableIPConfigStatus[id]
	ipc.SetState(types.Assigned)
	ipm.AssignedIPConfigStatus[id] = ipc
	delete(ipm.AvailableIPConfigStatus, id)
	return ipm.AssignedIPConfigStatus[id], nil
}

func (ipm *IPStatusManager) ReleaseIPConfig(ipconfigID string) (cns.IPConfigurationStatus, error) {
	ipm.rwmutex.Lock()
	defer ipm.rwmutex.Unlock()
	ipc := ipm.AssignedIPConfigStatus[ipconfigID]
	ipc.SetState(types.Available)
	ipm.AvailableIPConfigStatus[ipconfigID] = ipc
	ipm.AvailableIPIDStack.Push(ipconfigID)
	delete(ipm.AssignedIPConfigStatus, ipconfigID)
	return ipm.AvailableIPConfigStatus[ipconfigID], nil
}

func (ipm *IPStatusManager) MarkIPAsPendingRelease(numberOfIPsToMark int) (map[string]cns.IPConfigurationStatus, error) {
	ipm.rwmutex.Lock()
	defer ipm.rwmutex.Unlock()

	var err error

	pendingReleaseIPs := make(map[string]cns.IPConfigurationStatus)

	defer func() {
		// if there was an error, and not all IPs have been freed, restore state
		if err != nil && len(pendingReleaseIPs) != numberOfIPsToMark {
			for uuid, ipState := range pendingReleaseIPs {
				ipState.SetState(types.Available)
				ipm.AvailableIPIDStack.Push(pendingReleaseIPs[uuid].ID)
				ipm.AvailableIPConfigStatus[pendingReleaseIPs[uuid].ID] = ipState
				delete(ipm.PendingReleaseIPConfigStatus, pendingReleaseIPs[uuid].ID)
			}
		}
	}()

	for i := 0; i < numberOfIPsToMark; i++ {
		id, err := ipm.AvailableIPIDStack.Pop()
		if err != nil {
			return ipm.PendingReleaseIPConfigStatus, err
		}

		// add all pending release to a slice
		ipConfig := ipm.AvailableIPConfigStatus[id]
		ipConfig.SetState(types.PendingRelease)
		pendingReleaseIPs[id] = ipConfig

		delete(ipm.AvailableIPConfigStatus, id)
	}

	// if no errors at this point, add the pending IPs to the Pending state
	for _, pendingReleaseIP := range pendingReleaseIPs {
		ipm.PendingReleaseIPConfigStatus[pendingReleaseIP.ID] = pendingReleaseIP
	}

	return pendingReleaseIPs, nil
}

func (ipm *IPStatusManager) SetNumberOfAssignedIPs(assign int) error {
	currentAssigned := len(ipm.AssignedIPConfigStatus)
	delta := (assign - currentAssigned)

	if delta > 0 {
		// assign IPs
		for i := 0; i < delta; i++ {
			if _, err := ipm.ReserveIPConfig(); err != nil {
				return err
			}
		}
		return nil
	}
	// unassign IPs
	delta *= -1
	i := 0
	for id := range ipm.AssignedIPConfigStatus {
		if i >= delta {
			break
		}
		if _, err := ipm.ReleaseIPConfig(id); err != nil {
			return err
		}
		i++
	}
	return nil
}

func (ipm *IPStatusManager) GetPendingReleaseIPConfigs() []cns.IPConfigurationStatus {
	ipconfigs := []cns.IPConfigurationStatus{}
	for key := range ipm.PendingReleaseIPConfigStatus {
		ipconfigs = append(ipconfigs, ipm.PendingReleaseIPConfigStatus[key])
	}
	return ipconfigs
}

// Return union of all state maps
func (ipm *IPStatusManager) GetPodIPConfigState() map[string]cns.IPConfigurationStatus {
	ipconfigs := make(map[string]cns.IPConfigurationStatus)
	for key := range ipm.AssignedIPConfigStatus {
		ipconfigs[key] = ipm.AssignedIPConfigStatus[key]
	}
	for key := range ipm.AvailableIPConfigStatus {
		ipconfigs[key] = ipm.AvailableIPConfigStatus[key]
	}
	for key := range ipm.PendingReleaseIPConfigStatus {
		ipconfigs[key] = ipm.PendingReleaseIPConfigStatus[key]
	}
	for key := range ipm.PendingProgramIPConfigStatus {
		ipconfigs[key] = ipm.PendingProgramIPConfigStatus[key]
	}
	return ipconfigs
}
