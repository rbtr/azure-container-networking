package v2

import (
	"math"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/types"
)

// pool is the current actual state of the CNS IP pool.
type pool struct {
	// assigned are the IPs that CNS has assigned to Pods.
	assigned int64
	// available are the allocated IPs in state "Available".
	available int64
	// pendingProgramming are the allocated IPs in state "PendingProgramming".
	pendingProgramming int64
	// pendingRelease are the allocated IPs in state "PendingRelease".
	pendingRelease int64
	// primaryIPs counts the number of NC primary IPs present in the NNC.
	primaryIPs int
	// requested are the target allocation of IPs that CNS has requested from DNC.
	requested int64
	// totalIPs are all the IPs totalIPs to CNS by DNC.
	totalIPs int64
}

func (p pool) repopulatePoolState(ips map[string]cns.IPConfigurationStatus) pool {
	state := pool{
		totalIPs:  int64(len(ips)),
		requested: p.requested,
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
	return state
}

func (p pool) scalePool(s scaler) pool {
	targetRequest := calculateTargetIPCount(p.requested, s.batch, s.buffer)
	if targetRequest > s.max {
		// clamp request at the max IPs
		targetRequest = s.max
	}
	p.requested = targetRequest
	return p
}

func calculateTargetIPCount(demand, batch int64, buffer float64) int64 {
	return batch * int64(math.Ceil(buffer+float64(demand)/float64(batch)))
}
