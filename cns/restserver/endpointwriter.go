// Copyright Microsoft. All rights reserved.
// MIT License

package restserver

import (
	"context"
	"net"

	"github.com/Azure/azure-container-networking/cns/logger"
	cnsstore "github.com/Azure/azure-container-networking/cns/store"
)

// persistEndpoint synchronously writes a single endpoint record to bolt.
func (service *HTTPRestService) persistEndpoint(containerID string, info *EndpointInfo) error {
	if service.endpointStore == nil {
		return nil
	}
	rec := endpointInfoToRecord(info)
	if err := service.endpointStore.PutEndpoint(context.Background(), containerID, rec); err != nil {
		logger.Errorf("[persistEndpoint] sync put for %s failed: %v", containerID, err)
		endpointWriteFailures.Inc()
		return err
	}
	return nil
}

// deletePersistedEndpoint synchronously removes a single endpoint record from bolt.
func (service *HTTPRestService) deletePersistedEndpoint(containerID string) error {
	if service.endpointStore == nil {
		return nil
	}
	if err := service.endpointStore.DeleteEndpoint(context.Background(), containerID); err != nil {
		logger.Errorf("[deletePersistedEndpoint] sync delete for %s failed: %v", containerID, err)
		endpointWriteFailures.Inc()
		return err
	}
	return nil
}

// endpointInfoToRecord converts the restserver EndpointInfo to the
// cns/store EndpointRecord, deep-copying net.IPNet byte slices.
func endpointInfoToRecord(info *EndpointInfo) cnsstore.EndpointRecord {
	rec := cnsstore.EndpointRecord{
		PodName:       info.PodName,
		PodNamespace:  info.PodNamespace,
		IfnameToIPMap: make(map[string]*cnsstore.IPInfoRecord, len(info.IfnameToIPMap)),
	}
	for ifn, ipInfo := range info.IfnameToIPMap {
		rec.IfnameToIPMap[ifn] = ipInfoToRecord(ipInfo)
	}
	return rec
}

func ipInfoToRecord(info *IPInfo) *cnsstore.IPInfoRecord {
	r := &cnsstore.IPInfoRecord{
		HnsEndpointID:      info.HnsEndpointID,
		HnsNetworkID:       info.HnsNetworkID,
		HostVethName:       info.HostVethName,
		MacAddress:         info.MacAddress,
		NetworkContainerID: info.NetworkContainerID,
		NICType:            info.NICType,
	}
	r.IPv4 = deepCopyIPNets(info.IPv4)
	r.IPv6 = deepCopyIPNets(info.IPv6)
	return r
}

func deepCopyIPNets(nets []net.IPNet) []net.IPNet {
	if len(nets) == 0 {
		return nil
	}
	out := make([]net.IPNet, len(nets))
	for i := range nets {
		out[i] = net.IPNet{
			IP:   append(net.IP(nil), nets[i].IP...),
			Mask: append(net.IPMask(nil), nets[i].Mask...),
		}
	}
	return out
}

// EndpointRecordToInfo converts a cns/store EndpointRecord back to the
// restserver EndpointInfo. Used when restoring state from bolt at startup.
func EndpointRecordToInfo(rec cnsstore.EndpointRecord) *EndpointInfo {
	info := &EndpointInfo{
		PodName:       rec.PodName,
		PodNamespace:  rec.PodNamespace,
		IfnameToIPMap: make(map[string]*IPInfo, len(rec.IfnameToIPMap)),
	}
	for ifn, ipRec := range rec.IfnameToIPMap {
		info.IfnameToIPMap[ifn] = ipInfoRecordToIPInfo(ipRec)
	}
	return info
}

func ipInfoRecordToIPInfo(rec *cnsstore.IPInfoRecord) *IPInfo {
	return &IPInfo{
		IPv4:               rec.IPv4,
		IPv6:               rec.IPv6,
		HnsEndpointID:      rec.HnsEndpointID,
		HnsNetworkID:       rec.HnsNetworkID,
		HostVethName:       rec.HostVethName,
		MacAddress:         rec.MacAddress,
		NetworkContainerID: rec.NetworkContainerID,
		NICType:            rec.NICType,
	}
}
