// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/configuration"
	persistentstate "github.com/Azure/azure-container-networking/cns/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

const (
	startupTestNCID        = "nc-1"
	startupTestIPID        = "ip-uuid-1"
	startupTestIP          = "10.0.0.4"
	startupTestContainerID = "container-1"
)

var (
	errStartupBootIDUnavailable    = errors.New("boot ID unavailable")
	errStartupMetadataBucketAbsent = errors.New("metadata bucket is missing")
)

func TestPersistentStateBootPolicyByChannelMode(t *testing.T) {
	tests := []struct {
		name           string
		channelMode    string
		resetReadiness bool
	}{
		{name: "CRD", channelMode: cns.CRD, resetReadiness: true},
		{name: "MultiTenantCRD", channelMode: cns.MultiTenantCRD, resetReadiness: true},
		{name: "AzureHost", channelMode: cns.AzureHost, resetReadiness: true},
		{name: "Managed", channelMode: cns.Managed},
		{name: "Direct", channelMode: cns.Direct},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := persistentStateBootPolicy(test.channelMode)
			assert.Equal(t, test.resetReadiness, policy.ResetNetworkContainerReadiness)
		})
	}
}

func TestInitializePersistentStateMigratesLegacyJSON(t *testing.T) {
	tests := []struct {
		name                string
		manageEndpointState bool
		wantEndpointState   bool
	}{
		{
			name:                "managed endpoint state",
			manageEndpointState: true,
			wantEndpointState:   true,
		},
		{
			name: "external endpoint state",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testPersistentStateStartupConfig(t.TempDir(), configuration.StateStoreBackendBolt)
			config.manageEndpointState = test.manageEndpointState
			writeStartupLegacyState(t, config, "node-before-migration")

			result, err := initializePersistentState(t.Context(), config, startupPersistentStateProviders("boot-1"))
			require.NoError(t, err)
			defer func() {
				require.NoError(t, result.Close())
			}()

			require.False(t, result.rebooted)
			snapshot := startupSnapshot(t, result.database)
			assert.Equal(t, persistentstate.AuthorityBolt, snapshot.Metadata.Authority)
			assert.Equal(t, "boot-1", snapshot.Metadata.BootID)
			assert.Equal(t, "node-before-migration", snapshot.Metadata.NodeID)
			assert.Contains(t, snapshot.NetworkContainers, startupTestNCID)
			assert.Contains(t, snapshot.IPs, startupTestIPID)
			if test.wantEndpointState {
				assert.Contains(t, snapshot.Endpoints, startupTestContainerID)
				assert.Contains(t, snapshot.Assignments, startupTestContainerID)
				assert.Equal(t, startupTestContainerID, snapshot.IPOwners[startupTestIPID])
				return
			}
			assert.Empty(t, snapshot.Endpoints)
			assert.Empty(t, snapshot.Assignments)
			assert.Empty(t, snapshot.IPOwners)
		})
	}
}

func TestInitializePersistentStateSameBootRestartPreservesState(t *testing.T) {
	config := testPersistentStateStartupConfig(t.TempDir(), configuration.StateStoreBackendBolt)
	writeStartupLegacyState(t, config, "node-1")

	first, err := initializePersistentState(t.Context(), config, startupPersistentStateProviders("boot-1"))
	require.NoError(t, err)
	before := startupSnapshot(t, first.database)
	require.NoError(t, first.Close())

	second, err := initializePersistentState(t.Context(), config, startupPersistentStateProviders("boot-1"))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, second.Close())
	}()

	assert.False(t, second.rebooted)
	after := startupSnapshot(t, second.database)
	assert.Equal(t, before, after)
}

func TestInitializePersistentStateNewBootAppliesPolicy(t *testing.T) {
	config := testPersistentStateStartupConfig(t.TempDir(), configuration.StateStoreBackendBolt)
	writeStartupLegacyState(t, config, "node-1")

	first, err := initializePersistentState(t.Context(), config, startupPersistentStateProviders("boot-1"))
	require.NoError(t, err)
	require.NoError(t, first.Close())

	second, err := initializePersistentState(t.Context(), config, startupPersistentStateProviders("boot-2"))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, second.Close())
	}()

	assert.True(t, second.rebooted)
	snapshot := startupSnapshot(t, second.database)
	assert.Equal(t, "boot-2", snapshot.Metadata.BootID)
	assert.Contains(t, snapshot.NetworkContainers, startupTestNCID)
	assert.Contains(t, snapshot.IPs, startupTestIPID)
	assert.Empty(t, snapshot.Endpoints)
	assert.Empty(t, snapshot.Assignments)
	assert.Empty(t, snapshot.IPOwners)
	record := snapshot.NetworkContainers[startupTestNCID]
	assert.Equal(t, "-1", record.HostVersion)
	assert.False(t, record.VFPUpdateComplete)
}

func TestInitializePersistentStateBoltRestartIgnoresCorruptLegacyJSON(t *testing.T) {
	config := testPersistentStateStartupConfig(t.TempDir(), configuration.StateStoreBackendBolt)
	writeStartupLegacyState(t, config, "node-1")

	first, err := initializePersistentState(t.Context(), config, startupPersistentStateProviders("boot-1"))
	require.NoError(t, err)
	before := startupSnapshot(t, first.database)
	require.NoError(t, first.Close())

	require.NoError(t, os.WriteFile(config.paths.legacyCNS, []byte("{"), 0o600))
	require.NoError(t, os.WriteFile(config.paths.legacyEndpoint, []byte("{"), 0o600))

	second, err := initializePersistentState(t.Context(), config, startupPersistentStateProviders("boot-1"))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, second.Close())
	}()

	assert.False(t, second.rebooted)
	assert.Equal(t, before, startupSnapshot(t, second.database))
}

func TestInitializePersistentStateRollbackStartsJSONBackend(t *testing.T) {
	config := testPersistentStateStartupConfig(t.TempDir(), configuration.StateStoreBackendBolt)
	writeStartupLegacyState(t, config, "node-1")

	boltResult, err := initializePersistentState(t.Context(), config, startupPersistentStateProviders("boot-1"))
	require.NoError(t, err)
	require.NoError(t, boltResult.Close())

	config.backend = configuration.StateStoreBackendJSON
	config.mode = configuration.StateStoreModeRollbackToJSON
	jsonResult, err := initializePersistentState(t.Context(), config, startupPersistentStateProviders("boot-1"))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, jsonResult.Close())
	}()

	assert.Nil(t, jsonResult.database)
	require.NotNil(t, jsonResult.legacyCNSStore)
	require.NotNil(t, jsonResult.legacyEndpointStore)

	legacyCNS := map[string]any{}
	require.NoError(t, jsonResult.legacyCNSStore.Read("ContainerNetworkService", &legacyCNS))
	assert.Equal(t, "node-1", legacyCNS["NodeID"])
	legacyEndpoints := map[string]any{}
	require.NoError(t, jsonResult.legacyEndpointStore.Read("Endpoints", &legacyEndpoints))
	assert.Contains(t, legacyEndpoints, startupTestContainerID)

	reopened, err := persistentstate.Open(config.paths.database, persistentstate.Options{Timeout: 100 * time.Millisecond})
	require.NoError(t, err)
	snapshot := startupSnapshot(t, reopened)
	assert.Equal(t, persistentstate.AuthorityJSON, snapshot.Metadata.Authority)
	require.NoError(t, reopened.Close())
}

func TestInitializePersistentStateReimportsMutatedRollbackJSON(t *testing.T) {
	config := testPersistentStateStartupConfig(t.TempDir(), configuration.StateStoreBackendBolt)
	writeStartupLegacyState(t, config, "node-before-rollback")

	boltResult, err := initializePersistentState(t.Context(), config, startupPersistentStateProviders("boot-1"))
	require.NoError(t, err)
	require.NoError(t, boltResult.Close())

	config.backend = configuration.StateStoreBackendJSON
	config.mode = configuration.StateStoreModeRollbackToJSON
	jsonResult, err := initializePersistentState(t.Context(), config, startupPersistentStateProviders("boot-1"))
	require.NoError(t, err)

	legacyCNS := map[string]any{}
	require.NoError(t, jsonResult.legacyCNSStore.Read("ContainerNetworkService", &legacyCNS))
	legacyCNS["NodeID"] = "node-after-rollback"
	require.NoError(t, jsonResult.legacyCNSStore.Write("ContainerNetworkService", legacyCNS))
	require.NoError(t, jsonResult.Close())

	config.backend = configuration.StateStoreBackendBolt
	config.mode = configuration.StateStoreModeNormal
	upgraded, err := initializePersistentState(t.Context(), config, startupPersistentStateProviders("boot-1"))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, upgraded.Close())
	}()

	assert.False(t, upgraded.rebooted)
	snapshot := startupSnapshot(t, upgraded.database)
	assert.Equal(t, persistentstate.AuthorityBolt, snapshot.Metadata.Authority)
	assert.Equal(t, "node-after-rollback", snapshot.Metadata.NodeID)
	assert.Contains(t, snapshot.Endpoints, startupTestContainerID)
	assert.Equal(t, startupTestContainerID, snapshot.IPOwners[startupTestIPID])
}

func TestInitializePersistentStateRollbackRequiresDatabase(t *testing.T) {
	config := testPersistentStateStartupConfig(t.TempDir(), configuration.StateStoreBackendJSON)
	config.mode = configuration.StateStoreModeRollbackToJSON

	result, err := initializePersistentState(t.Context(), config, startupPersistentStateProviders("boot-1"))
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Nil(t, result.database)
}

func TestInitializePersistentStateRejectsCorruptDatabase(t *testing.T) {
	config := testPersistentStateStartupConfig(t.TempDir(), configuration.StateStoreBackendBolt)
	require.NoError(t, os.MkdirAll(filepath.Dir(config.paths.database), 0o755))
	require.NoError(t, os.WriteFile(config.paths.database, []byte("not a Bolt database"), 0o600))

	result, err := initializePersistentState(t.Context(), config, startupPersistentStateProviders("boot-1"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "opening Bolt state store")
	assert.Nil(t, result.database)
}

func TestInitializePersistentStateRejectsSchemaMismatch(t *testing.T) {
	config := testPersistentStateStartupConfig(t.TempDir(), configuration.StateStoreBackendBolt)
	require.NoError(t, os.MkdirAll(filepath.Dir(config.paths.database), 0o755))
	database, err := persistentstate.Open(config.paths.database, persistentstate.Options{})
	require.NoError(t, err)
	require.NoError(t, database.Close())
	setStartupSchemaVersion(t, config.paths.database, persistentstate.SchemaVersion+1)
	result, err := initializePersistentState(t.Context(), config, startupPersistentStateProviders("boot-1"))
	require.ErrorIs(t, err, persistentstate.ErrSchemaMismatch)
	assert.Nil(t, result.database)
}

func TestInitializePersistentStateRejectsDatabaseLockContention(t *testing.T) {
	config := testPersistentStateStartupConfig(t.TempDir(), configuration.StateStoreBackendBolt)
	require.NoError(t, os.MkdirAll(filepath.Dir(config.paths.database), 0o755))
	held, err := persistentstate.Open(config.paths.database, persistentstate.Options{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = held.Close()
	})

	providers := startupPersistentStateProviders("boot-1")
	providers.openDatabase = func(path string, _ persistentstate.Options) (*persistentstate.DB, error) {
		return persistentstate.Open(path, persistentstate.Options{Timeout: 25 * time.Millisecond})
	}
	result, err := initializePersistentState(t.Context(), config, providers)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "opening Bolt state store")
	assert.Nil(t, result.database)

	require.NoError(t, held.Close())
	retry, err := initializePersistentState(t.Context(), config, startupPersistentStateProviders("boot-1"))
	require.NoError(t, err)
	require.NoError(t, retry.Close())
}

func TestInitializePersistentStateHonorsContextCancellation(t *testing.T) {
	config := testPersistentStateStartupConfig(t.TempDir(), configuration.StateStoreBackendBolt)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result, err := initializePersistentState(ctx, config, startupPersistentStateProviders("boot-1"))
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, result.database)
	assertStartupDatabaseReopens(t, config.paths.database)
}

func TestInitializePersistentStateCleansUpPartialStartup(t *testing.T) {
	config := testPersistentStateStartupConfig(t.TempDir(), configuration.StateStoreBackendBolt)
	providers := startupPersistentStateProviders("boot-1")
	providers.bootID = func() (string, error) {
		return "", errStartupBootIDUnavailable
	}

	result, err := initializePersistentState(t.Context(), config, providers)
	require.ErrorIs(t, err, errStartupBootIDUnavailable)
	assert.Nil(t, result.database)
	assertStartupDatabaseReopens(t, config.paths.database)
}

func startupPersistentStateProviders(bootID string) persistentStateProviders {
	return persistentStateProviders{
		bootID: func() (string, error) {
			return bootID, nil
		},
		lastRebootTime: func() (time.Time, error) {
			return time.Time{}, nil
		},
		openDatabase: persistentstate.Open,
	}
}

func startupSnapshot(t *testing.T, database *persistentstate.DB) persistentstate.Snapshot {
	t.Helper()
	require.NotNil(t, database)
	snapshot, err := database.Snapshot(t.Context())
	require.NoError(t, err)
	return snapshot
}

func writeStartupLegacyState(t *testing.T, config persistentStateStartupConfig, nodeID string) {
	t.Helper()
	request := cns.CreateNetworkContainerRequest{
		NetworkContainerid: startupTestNCID,
		Version:            "4",
		SecondaryIPConfigs: map[string]cns.SecondaryIPConfig{
			startupTestIPID: {
				IPAddress: startupTestIP,
				NCVersion: 4,
			},
		},
	}
	writeStartupJSON(t, config.paths.legacyCNS, map[string]any{
		"ContainerNetworkService": map[string]any{
			"OrchestratorType": "KubernetesCRD",
			"NodeID":           nodeID,
			"Initialized":      true,
			"ContainerStatus": map[string]any{
				startupTestNCID: map[string]any{
					"ID":                            startupTestNCID,
					"VMVersion":                     "4",
					"HostVersion":                   "3",
					"VfpUpdateComplete":             true,
					"CreateNetworkContainerRequest": request,
				},
			},
		},
	})
	writeStartupJSON(t, config.paths.legacyEndpoint, map[string]any{
		"Endpoints": map[string]any{
			startupTestContainerID: map[string]any{
				"PodName":      "pod-1",
				"PodNamespace": "default",
				"IfnameToIPMap": map[string]any{
					"eth0": map[string]any{
						"IPv4": []net.IPNet{{
							IP:   net.ParseIP(startupTestIP),
							Mask: net.CIDRMask(24, 32),
						}},
					},
				},
			},
		},
	})
}

func writeStartupJSON(t *testing.T, path string, value any) {
	t.Helper()
	require.NotEmpty(t, path)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	data, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func setStartupSchemaVersion(t *testing.T, path string, version uint32) {
	t.Helper()
	database, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 100 * time.Millisecond})
	require.NoError(t, err)
	require.NoError(t, database.Update(func(tx *bolt.Tx) error {
		metadata := tx.Bucket([]byte("metadata"))
		if metadata == nil {
			return errStartupMetadataBucketAbsent
		}
		value := make([]byte, 4)
		binary.LittleEndian.PutUint32(value, version)
		return metadata.Put([]byte("schema_version"), value)
	}))
	require.NoError(t, database.Close())
}

func assertStartupDatabaseReopens(t *testing.T, path string) {
	t.Helper()
	database, err := persistentstate.Open(path, persistentstate.Options{Timeout: 100 * time.Millisecond})
	require.NoError(t, err)
	require.NoError(t, database.Close())
}
