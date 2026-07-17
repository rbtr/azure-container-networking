//go:build windows

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package restserver

import (
	"net"
	"testing"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/stretchr/testify/require"
)

func TestPersistentEndpointRoundTripPreservesHNSCleanupMetadata(t *testing.T) {
	endpoint := windowsHNSCleanupEndpoint()

	got := endpointFromPersistent(endpointToPersistent(endpoint))

	require.Equal(t, endpoint, got)
}

func TestCleanupStaleHNSResourcesPrunesBoltEndpointRecord(t *testing.T) {
	const staleContainerID = "stale-container"

	service := getTestService(cns.AzureContainerInstance)
	service.EndpointState = map[string]*EndpointInfo{
		staleContainerID: windowsHNSCleanupEndpoint(),
	}
	database := attachPersistentState(t, service)
	t.Cleanup(func() {
		require.NoError(t, database.Close())
	})
	require.NoError(t, service.ReplacePersistentEndpoints(t.Context(), service.EndpointState))

	client := &mockHNSClient{}
	originalClient := defaultHNSClient
	t.Cleanup(func() {
		defaultHNSClient = originalClient
	})
	defaultHNSClient = client

	require.NoError(t, service.cleanupStaleHNSResources(
		"Swift_new-nc",
		"00:11:22:33:44:55",
		"169.254.128.4",
	))
	require.NotContains(t, service.EndpointState, staleContainerID)
	require.ElementsMatch(t, []string{"hns-apipa", "hns-endpoint"}, client.deletedEndpointIDs)
	require.Equal(t, []string{"hns-network"}, client.deletedNetworkIDs)

	snapshot, err := database.Snapshot(t.Context())
	require.NoError(t, err)
	require.NotContains(t, snapshot.Endpoints, staleContainerID)
}

func windowsHNSCleanupEndpoint() *EndpointInfo {
	return &EndpointInfo{
		PodName:      "pod-windows",
		PodNamespace: "default",
		IfnameToIPMap: map[string]*IPInfo{
			"Ethernet 4": {
				HnsEndpointID: "hns-endpoint",
				HnsNetworkID:  "hns-network",
				MacAddress:    "00:11:22:33:44:55",
				NICType:       cns.DelegatedVMNIC,
			},
			"HostNCApipaEndpoint-Swift_old-nc": {
				IPv4: []net.IPNet{{
					IP:   net.ParseIP("169.254.128.4").To4(),
					Mask: net.CIDRMask(16, 32),
				}},
				HnsEndpointID:      "hns-apipa",
				NetworkContainerID: "Swift_old-nc",
				NICType:            cns.ApipaNIC,
			},
		},
	}
}
