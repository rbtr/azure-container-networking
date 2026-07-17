//go:build windows

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/configuration"
	"github.com/Azure/azure-container-networking/cns/restserver"
	persistentstate "github.com/Azure/azure-container-networking/cns/state"
	"github.com/stretchr/testify/require"
)

const (
	windowsStateBootID1          = "11111111-1111-1111-1111-111111111111"
	windowsStateBootID2          = "22222222-2222-2222-2222-222222222222"
	windowsStateContainerID      = "container-windows"
	windowsStatePodKey           = "pod-key-windows"
	windowsStateNCID             = "nc-windows"
	windowsStateIPID             = "ip-windows"
	windowsStateIPAddress        = "10.240.0.4"
	windowsStateHNSEndpointID    = "hns-endpoint-old"
	windowsStateHNSNetworkID     = "hns-network-old"
	windowsStateAPIPAEndpointID  = "hns-apipa-old"
	windowsStateReimportEndpoint = "hns-endpoint-json"
)

func TestInitializePersistentStateWindowsSameBootRestart(t *testing.T) {
	config := windowsPersistentStateConfig(t, configuration.StateStoreBackendBolt)
	first, err := initializePersistentState(
		t.Context(),
		config,
		windowsPersistentStateProviders(t, windowsStateBootID1),
	)
	require.NoError(t, err)
	seedWindowsPersistentState(t, first.database)
	require.NoError(t, first.Close())

	restarted, err := initializePersistentState(
		t.Context(),
		config,
		windowsPersistentStateProviders(t, windowsStateBootID1),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, restarted.Close())
	})
	require.False(t, restarted.rebooted)

	snapshot, err := restarted.database.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, windowsStateBootID1, snapshot.Metadata.BootID)
	require.Equal(t, "1", snapshot.NetworkContainers[windowsStateNCID].HostVersion)
	require.True(t, snapshot.NetworkContainers[windowsStateNCID].VFPUpdateComplete)
	requireWindowsEndpointState(t, snapshot, windowsStateHNSEndpointID, windowsStatePodKey)
}

func TestInitializePersistentStateWindowsNewBootRetainsEndpoints(t *testing.T) {
	config := windowsPersistentStateConfig(t, configuration.StateStoreBackendBolt)
	first, err := initializePersistentState(
		t.Context(),
		config,
		windowsPersistentStateProviders(t, windowsStateBootID1),
	)
	require.NoError(t, err)
	seedWindowsPersistentState(t, first.database)
	require.NoError(t, first.Close())

	restarted, err := initializePersistentState(
		t.Context(),
		config,
		windowsPersistentStateProviders(t, windowsStateBootID2),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, restarted.Close())
	})
	require.True(t, restarted.rebooted)

	snapshot, err := restarted.database.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, windowsStateBootID2, snapshot.Metadata.BootID)
	require.Equal(t, "-1", snapshot.NetworkContainers[windowsStateNCID].HostVersion)
	require.False(t, snapshot.NetworkContainers[windowsStateNCID].VFPUpdateComplete)
	requireWindowsEndpointState(t, snapshot, windowsStateHNSEndpointID, windowsStateContainerID)
	require.NotContains(t, snapshot.Assignments, windowsStatePodKey)
}

func TestInitializePersistentStateWindowsRollbackAndReupgrade(t *testing.T) {
	config := windowsPersistentStateConfig(t, configuration.StateStoreBackendBolt)
	require.True(t, filepath.IsAbs(config.paths.database))
	require.NotEmpty(t, filepath.VolumeName(config.paths.database))

	first, err := initializePersistentState(
		t.Context(),
		config,
		windowsPersistentStateProviders(t, windowsStateBootID1),
	)
	require.NoError(t, err)
	seedWindowsPersistentState(t, first.database)
	require.NoError(t, first.Close())

	rollbackConfig := config
	rollbackConfig.backend = configuration.StateStoreBackendJSON
	rollbackConfig.mode = configuration.StateStoreModeRollbackToJSON
	rollback, err := initializePersistentState(
		t.Context(),
		rollbackConfig,
		windowsPersistentStateProviders(t, windowsStateBootID1),
	)
	require.NoError(t, err)
	require.FileExists(t, config.paths.legacyCNS)
	require.FileExists(t, config.paths.legacyEndpoint)

	endpoints := map[string]*restserver.EndpointInfo{}
	require.NoError(t, rollback.legacyEndpointStore.Read(restserver.EndpointStoreKey, &endpoints))
	endpoint := endpoints[windowsStateContainerID]
	require.NotNil(t, endpoint)
	delegated := endpoint.IfnameToIPMap["Ethernet 4"]
	require.NotNil(t, delegated)
	delegated.HnsEndpointID = windowsStateReimportEndpoint
	require.NoError(t, rollback.legacyEndpointStore.Write(restserver.EndpointStoreKey, endpoints))
	require.NoError(t, rollback.Close())

	reupgraded, err := initializePersistentState(
		t.Context(),
		config,
		windowsPersistentStateProviders(t, windowsStateBootID1),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, reupgraded.Close())
	})
	require.False(t, reupgraded.rebooted)

	snapshot, err := reupgraded.database.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, persistentstate.AuthorityBolt, snapshot.Metadata.Authority)
	requireWindowsEndpointState(t, snapshot, windowsStateReimportEndpoint, windowsStateContainerID)
}

func windowsPersistentStateConfig(
	t *testing.T,
	backend configuration.StateStoreBackend,
) persistentStateStartupConfig {
	t.Helper()
	config := testPersistentStateStartupConfig(t.TempDir(), backend)
	config.bootPolicy = platformPersistentStateBootPolicy(true)
	return config
}

func windowsPersistentStateProviders(t *testing.T, bootID string) persistentStateProviders {
	t.Helper()
	return persistentStateProviders{
		bootID: func() (string, error) {
			return bootID, nil
		},
		lastRebootTime: func() (time.Time, error) {
			t.Fatal("last reboot time fallback must not run when Bolt boot metadata is available")
			return time.Time{}, nil
		},
		openDatabase: persistentstate.Open,
	}
}

func seedWindowsPersistentState(t *testing.T, database *persistentstate.DB) {
	t.Helper()
	record := persistentstate.NewNetworkContainerRecord(
		windowsStateNCID,
		"2",
		"1",
		true,
		cns.CreateNetworkContainerRequest{NetworkContainerid: windowsStateNCID},
	)
	require.NoError(t, database.ApplyNetworkContainer(t.Context(), record, map[string]persistentstate.IPRecord{
		windowsStateIPID: {
			ID:        windowsStateIPID,
			IPAddress: windowsStateIPAddress,
			NCID:      windowsStateNCID,
			NCVersion: 2,
		},
	}))

	assignment := persistentstate.AssignmentRecord{
		Pod: persistentstate.PodIdentity{
			PodKey:           windowsStatePodKey,
			InfraContainerID: windowsStateContainerID,
			InterfaceID:      windowsStateContainerID,
			PodName:          "pod-windows",
			PodNamespace:     "default",
		},
		IPIDs: []string{windowsStateIPID},
	}
	require.NoError(t, database.AssignEndpoint(
		t.Context(),
		assignment,
		windowsPersistentEndpoint(),
		time.Now(),
		time.Hour,
	))
}

func windowsPersistentEndpoint() persistentstate.EndpointRecord {
	return persistentstate.EndpointRecord{
		PodName:      "pod-windows",
		PodNamespace: "default",
		IfnameToIPMap: map[string]*persistentstate.IPInfoRecord{
			"eth0": {
				IPv4: []net.IPNet{{
					IP:   net.ParseIP(windowsStateIPAddress).To4(),
					Mask: net.CIDRMask(24, 32),
				}},
				NICType: cns.InfraNIC,
			},
			"Ethernet 4": {
				HNSEndpointID: windowsStateHNSEndpointID,
				HNSNetworkID:  windowsStateHNSNetworkID,
				MACAddress:    "00:11:22:33:44:55",
				NICType:       cns.DelegatedVMNIC,
			},
			"HostNCApipaEndpoint-Swift_old-nc": {
				IPv4: []net.IPNet{{
					IP:   net.ParseIP("169.254.128.4").To4(),
					Mask: net.CIDRMask(16, 32),
				}},
				HNSEndpointID:      windowsStateAPIPAEndpointID,
				NetworkContainerID: "Swift_old-nc",
				NICType:            cns.ApipaNIC,
			},
		},
	}
}

func requireWindowsEndpointState(
	t *testing.T,
	snapshot persistentstate.Snapshot,
	hnsEndpointID string,
	assignmentKey string,
) {
	t.Helper()
	endpoint, ok := snapshot.Endpoints[windowsStateContainerID]
	require.True(t, ok)
	delegated := endpoint.IfnameToIPMap["Ethernet 4"]
	require.NotNil(t, delegated)
	require.Equal(t, hnsEndpointID, delegated.HNSEndpointID)
	require.Equal(t, windowsStateHNSNetworkID, delegated.HNSNetworkID)
	apipa := endpoint.IfnameToIPMap["HostNCApipaEndpoint-Swift_old-nc"]
	require.NotNil(t, apipa)
	require.Equal(t, windowsStateAPIPAEndpointID, apipa.HNSEndpointID)
	assignment, ok := snapshot.Assignments[assignmentKey]
	require.True(t, ok)
	require.Equal(t, []string{windowsStateIPID}, assignment.IPIDs)
	require.Equal(t, assignmentKey, snapshot.IPOwners[windowsStateIPID])
}
