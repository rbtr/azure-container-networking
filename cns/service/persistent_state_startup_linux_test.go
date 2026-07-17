// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/azure-container-networking/cns/configuration"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitializePersistentStateRejectsReadOnlyDatabaseDirectory(t *testing.T) {
	config := testPersistentStateStartupConfig(t.TempDir(), configuration.StateStoreBackendBolt)
	config.manageEndpointState = false
	databaseDir := filepath.Dir(config.paths.database)
	require.NoError(t, os.MkdirAll(databaseDir, 0o700))
	require.NoError(t, os.Chmod(databaseDir, 0o500))
	t.Cleanup(func() {
		require.NoError(t, os.Chmod(databaseDir, 0o700))
	})

	probePath := filepath.Join(databaseDir, "permission-probe")
	if err := os.WriteFile(probePath, []byte("probe"), 0o600); err == nil {
		require.NoError(t, os.Remove(probePath))
		require.NoError(t, os.Chmod(databaseDir, 0o700))
		t.Skip("filesystem does not enforce directory write permissions for this user")
	}

	result, err := initializePersistentState(t.Context(), config, startupPersistentStateProviders("boot-1"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "opening Bolt state store")
	assert.Nil(t, result.database)
}
