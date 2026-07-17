//go:build windows && integration

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package restserver

import (
	"os"
	"testing"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/stretchr/testify/require"
)

func TestPersistentStateHNSCleanupNativeIntegration(t *testing.T) {
	endpointID := os.Getenv("ACN_HNS_INTEGRATION_ENDPOINT_ID")
	networkID := os.Getenv("ACN_HNS_INTEGRATION_NETWORK_ID")
	if endpointID == "" || networkID == "" {
		t.Skip("set ACN_HNS_INTEGRATION_ENDPOINT_ID and ACN_HNS_INTEGRATION_NETWORK_ID to disposable HNS resources")
	}

	const (
		staleContainerID = "acn-state-integration-stale"
		macAddress       = "02:00:00:00:00:01"
	)
	service := getTestService(cns.AzureContainerInstance)
	service.EndpointState = map[string]*EndpointInfo{
		staleContainerID: {
			PodName:      "acn-state-integration",
			PodNamespace: "default",
			IfnameToIPMap: map[string]*IPInfo{
				"Ethernet": {
					HnsEndpointID: endpointID,
					HnsNetworkID:  networkID,
					MacAddress:    macAddress,
					NICType:       cns.DelegatedVMNIC,
				},
			},
		},
	}
	database := attachPersistentState(t, service)
	t.Cleanup(func() {
		require.NoError(t, database.Close())
	})
	require.NoError(t, service.ReplacePersistentEndpoints(t.Context(), service.EndpointState))

	originalClient := defaultHNSClient
	t.Cleanup(func() {
		defaultHNSClient = originalClient
	})
	defaultHNSClient = hnsClientImpl{}

	require.NoError(t, service.cleanupStaleHNSResources("Swift_integration-new", macAddress, ""))

	snapshot, err := database.Snapshot(t.Context())
	require.NoError(t, err)
	require.NotContains(t, snapshot.Endpoints, staleContainerID)
}
