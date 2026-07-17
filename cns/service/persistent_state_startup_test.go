// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Azure/azure-container-networking/cns/configuration"
	persistentstate "github.com/Azure/azure-container-networking/cns/state"
	"github.com/stretchr/testify/require"
)

func TestInitializePersistentStateFreshJSON(t *testing.T) {
	config := testPersistentStateStartupConfig(t.TempDir(), configuration.StateStoreBackendJSON)

	result, err := initializePersistentState(t.Context(), config, persistentStateProviders{})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, result.Close())
	})

	require.Nil(t, result.database)
	require.NotNil(t, result.legacyCNSStore)
	require.NotNil(t, result.legacyEndpointStore)
	require.False(t, result.rebooted)

	want := map[string]string{"state": "fresh"}
	require.NoError(t, result.legacyCNSStore.Write("test", want))
	got := map[string]string{}
	require.NoError(t, result.legacyCNSStore.Read("test", &got))
	require.Equal(t, want, got)

	_, err = os.Stat(config.paths.database)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestInitializePersistentStateFreshBolt(t *testing.T) {
	config := testPersistentStateStartupConfig(t.TempDir(), configuration.StateStoreBackendBolt)
	bootIDCalls := 0
	rebootTimeCalls := 0
	openDatabaseCalls := 0
	providers := persistentStateProviders{
		bootID: func() (string, error) {
			bootIDCalls++
			return "boot-1", nil
		},
		lastRebootTime: func() (time.Time, error) {
			rebootTimeCalls++
			return time.Time{}, nil
		},
		openDatabase: func(path string, options persistentstate.Options) (*persistentstate.DB, error) {
			openDatabaseCalls++
			return persistentstate.Open(path, options)
		},
	}

	result, err := initializePersistentState(t.Context(), config, providers)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, result.Close())
	})

	require.NotNil(t, result.database)
	require.Nil(t, result.legacyCNSStore)
	require.Nil(t, result.legacyEndpointStore)
	require.False(t, result.rebooted)
	require.Equal(t, 1, bootIDCalls)
	require.Equal(t, 0, rebootTimeCalls)
	require.Equal(t, 1, openDatabaseCalls)
	require.FileExists(t, config.paths.database)

	snapshot, err := result.database.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, "boot-1", snapshot.Metadata.BootID)

	require.NoError(t, result.Close())
	reopened, err := persistentstate.Open(config.paths.database, persistentstate.Options{Timeout: 100 * time.Millisecond})
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
}

func testPersistentStateStartupConfig(
	root string,
	backend configuration.StateStoreBackend,
) persistentStateStartupConfig {
	return persistentStateStartupConfig{
		backend:             backend,
		mode:                configuration.StateStoreModeNormal,
		manageEndpointState: true,
		bootPolicy: persistentstate.BootPolicy{
			ClearEndpoints:                 true,
			ResetNetworkContainerReadiness: true,
		},
		paths: persistentStatePaths{
			legacyCNS:      filepath.Join(root, "cns", "azure-cns.json"),
			legacyEndpoint: filepath.Join(root, "endpoint", "azure-endpoints.json"),
			database:       filepath.Join(root, "database", "azure-cns.db"),
			cnsLock:        filepath.Join(root, "locks", "azure-cns.lock"),
			endpointLock:   filepath.Join(root, "locks", "azure-endpoints.lock"),
		},
	}
}
