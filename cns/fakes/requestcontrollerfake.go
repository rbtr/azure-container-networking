//go:build !ignore_uncovered
// +build !ignore_uncovered

package fakes

import (
	"context"
	"net"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/types"
	"github.com/Azure/azure-container-networking/crd/nodenetworkconfig/api/v1alpha"
	"github.com/google/uuid"
)

type poolMonitor interface {
	Update(nnc *v1alpha.NodeNetworkConfig)
}

type ipStatusManager interface {
	AddIPConfigs([]cns.IPConfigurationStatus)
	RemovePendingReleaseIPConfigs([]string)
	GetPodIPConfigState() map[string]cns.IPConfigurationStatus
}

type RequestControllerFake struct {
	ipm     ipStatusManager
	poolMon poolMonitor
	nnc     *v1alpha.NodeNetworkConfig
	ip      net.IP
}

func NewRequestControllerFake(ipm ipStatusManager, poolMon poolMonitor, nnc *v1alpha.NodeNetworkConfig, subnetAddressSpace string, numberOfIPConfigs int64) *RequestControllerFake {
	rc := &RequestControllerFake{
		ipm:     ipm,
		poolMon: poolMon,
		nnc:     nnc,
	}
	rc.ip, _, _ = net.ParseCIDR(subnetAddressSpace)
	rc.CarveIPConfigsAndAddToStatusAndCNS(numberOfIPConfigs)
	rc.nnc.Spec.RequestedIPCount = numberOfIPConfigs
	return rc
}

func (rc *RequestControllerFake) CarveIPConfigsAndAddToStatusAndCNS(numberOfIPConfigs int64) []cns.IPConfigurationStatus {
	var cnsIPConfigs []cns.IPConfigurationStatus
	for i := int64(0); i < numberOfIPConfigs; i++ {

		ipconfigCRD := v1alpha.IPAssignment{
			Name: uuid.New().String(),
			IP:   rc.ip.String(),
		}
		rc.nnc.Status.NetworkContainers[0].IPAssignments = append(rc.nnc.Status.NetworkContainers[0].IPAssignments, ipconfigCRD)

		ipconfigCNS := cns.IPConfigurationStatus{
			ID:        ipconfigCRD.Name,
			IPAddress: ipconfigCRD.IP,
		}
		ipconfigCNS.SetState(types.Available)
		cnsIPConfigs = append(cnsIPConfigs, ipconfigCNS)

		incrementIP(rc.ip)
	}

	rc.ipm.AddIPConfigs(cnsIPConfigs)

	return cnsIPConfigs
}

func (rc *RequestControllerFake) Init(context.Context) error {
	return nil
}

func (rc *RequestControllerFake) Start(context.Context) error {
	return nil
}

func (rc *RequestControllerFake) IsStarted() bool {
	return true
}

func remove(slice []v1alpha.IPAssignment, s int) []v1alpha.IPAssignment {
	return append(slice[:s], slice[s+1:]...)
}

func (rc *RequestControllerFake) Reconcile(removePendingReleaseIPs bool) error {
	diff := rc.nnc.Spec.RequestedIPCount - int64(len(rc.ipm.GetPodIPConfigState()))

	if diff > 0 {
		// carve the difference of test IPs and add them to CNS, assume dnc has populated the CRD status
		rc.CarveIPConfigsAndAddToStatusAndCNS(diff)
	} else if diff < 0 {
		// Assume DNC has removed the IPConfigs from the status

		// mimic DNC removing IPConfigs from the CRD
		for _, notInUseIPConfigName := range rc.nnc.Spec.IPsNotInUse {

			// remove ipconfig from status
			index := 0
			for _, ipconfig := range rc.nnc.Status.NetworkContainers[0].IPAssignments {
				if notInUseIPConfigName == ipconfig.Name {
					break
				}
				index++
			}
			rc.nnc.Status.NetworkContainers[0].IPAssignments = remove(rc.nnc.Status.NetworkContainers[0].IPAssignments, index)

		}
	}

	// remove ipconfig from CNS
	if removePendingReleaseIPs {
		rc.ipm.RemovePendingReleaseIPConfigs(rc.nnc.Spec.IPsNotInUse)
	}

	// update
	rc.poolMon.Update(rc.nnc)
	return nil
}

func incrementIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}
