// Copyright 2025 Microsoft. All rights reserved.
// MIT License

//go:build linux

package restserver

import (
	"net"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/logger"
	"github.com/Azure/azure-container-networking/netlink"
)

// assignVethPairs attempts to assign pre-created veth pairs from the pool to
// each InfraNIC PodIpInfo entry. If successful, it also adds host-side routes
// for the assigned IP using the pre-created veth's interface index.
func (service *HTTPRestService) assignVethPairs(podIPInfo []cns.PodIpInfo) {
	nl := netlink.NewNetlink()
	for i := range podIPInfo {
		if podIPInfo[i].NICType != cns.InfraNIC && podIPInfo[i].NICType != "" {
			continue
		}
		vp, err := service.vethPool.Acquire()
		if err != nil {
			return // pool empty, CNI will handle creation
		}

		podIP := net.ParseIP(podIPInfo[i].PodIPConfig.IPAddress)
		if podIP == nil {
			continue
		}

		var mask net.IPMask
		if podIP.To4() != nil {
			mask = net.CIDRMask(32, 32) //nolint:gomnd // IPv4 host mask
		} else {
			mask = net.CIDRMask(128, 128) //nolint:gomnd // IPv6 host mask
		}
		dst := &net.IPNet{IP: podIP, Mask: mask}
		family := netlink.GetIPAddressFamily(podIP)

		if err := nl.AddIPRoute(&netlink.Route{
			Family:    family,
			Dst:       dst,
			LinkIndex: vp.HostIndex,
		}); err != nil {
			logger.Errorf("Failed to add host route for pre-created veth %s -> %s: %v",
				vp.HostName, podIP.String(), err)
			continue // don't mark as pre-created
		}

		podIPInfo[i].HostVethName = vp.HostName
		podIPInfo[i].ContainerVethName = vp.ContainerName
		podIPInfo[i].HostRoutesPreCreated = true
	}
}
