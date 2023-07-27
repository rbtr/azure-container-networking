package cns

import (
	"fmt"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/restserver"
	"github.com/Azure/azure-container-networking/store"
	"github.com/pkg/errors"
)

type stateReader interface {
	Read(key string, value interface{}) error
}

// NewCNSPodInfoProvider returns an implementation of cns.PodInfoByIPProvider
// that builds the PodInfo map from the CNS managed endpoint store.
func NewCNSPodInfoProvider(reader stateReader) (cns.PodInfoByIPProvider, error) {
	return newCNSPodInfoProvider(reader)
}

func newCNSPodInfoProvider(reader stateReader) (cns.PodInfoByIPProvider, error) {
	var state map[string]*restserver.EndpointInfo
	err := reader.Read(restserver.EndpointStoreKey, &state)
	if err != nil {
		if errors.Is(err, store.ErrKeyNotFound) {
			// Nothing to restore.
			return cns.PodInfoByIPProviderFunc(func() (map[string]cns.PodInfo, error) {
				return endpointStateToPodInfoByIP(state)
			}), err
		}
		return nil, fmt.Errorf("failed to read endpoints state from store : %w", err)
	}
	return cns.PodInfoByIPProviderFunc(func() (map[string]cns.PodInfo, error) {
		return endpointStateToPodInfoByIP(state)
	}), nil
}

func endpointStateToPodInfoByIP(state map[string]*restserver.EndpointInfo) (map[string]cns.PodInfo, error) {
	podInfoByIP := map[string]cns.PodInfo{}
	for containerID, endpointInfo := range state { // for each endpoint
		for _, ipinfo := range endpointInfo.IfnameToIPMap { // for each IP info object of the endpoint's interfaces
			for _, ipv4conf := range ipinfo.IPv4 { // for each IPv4 config of the endpoint's interfaces
				if _, ok := podInfoByIP[ipv4conf.IP.String()]; ok {
					return nil, errors.Wrap(cns.ErrDuplicateIP, ipv4conf.IP.String())
				}
				podInfoByIP[ipv4conf.IP.String()] = cns.NewPodInfo(
					containerID,
					containerID,
					endpointInfo.PodName,
					endpointInfo.PodNamespace,
				)
			}
			for _, ipv6conf := range ipinfo.IPv6 { // for each IPv6 config of the endpoint's interfaces
				if _, ok := podInfoByIP[ipv6conf.IP.String()]; ok {
					return nil, errors.Wrap(cns.ErrDuplicateIP, ipv6conf.IP.String())
				}
				podInfoByIP[ipv6conf.IP.String()] = cns.NewPodInfo(
					containerID,
					containerID,
					endpointInfo.PodName,
					endpointInfo.PodNamespace,
				)
			}
		}
	}
	return podInfoByIP, nil
}
