// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExportLegacyFilePermissions(t *testing.T) {
	tests := []struct {
		name     string
		existing bool
	}{
		{name: "new destinations"},
		{name: "restrictive existing destinations", existing: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db := openRollbackTestDB(t)
			seedRollbackTestDB(ctx, t, db)

			outputDir := t.TempDir()
			cnsPath := filepath.Join(outputDir, "azure-cns.json")
			endpointPath := filepath.Join(outputDir, "azure-endpoints.json")
			if test.existing {
				writeRollbackDestination(t, cnsPath, []byte(`{"old":"cns"}`))
				writeRollbackDestination(t, endpointPath, []byte(`{"old":"endpoint"}`))
				require.NoError(t, os.Chmod(cnsPath, 0o400))
				require.NoError(t, os.Chmod(endpointPath, 0o400))
			}

			require.NoError(t, db.ExportLegacy(ctx, cnsPath, endpointPath))
			assertLegacyEnvelopeKeys(t, cnsPath, legacyCNSStoreKey)
			assertLegacyEnvelopeKeys(
				t,
				endpointPath,
				legacyEndpointStoreKey,
				legacyDeleteIntentStoreKey,
			)

			for _, path := range []string{cnsPath, endpointPath} {
				info, err := os.Stat(path)
				require.NoError(t, err)
				assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			}
		})
	}
}

func TestExportLegacyReadOnlyAncestorPreventsParentCreation(t *testing.T) {
	ctx := context.Background()
	db := openRollbackTestDB(t)
	seedRollbackTestDB(ctx, t, db)

	readOnlyParent := filepath.Join(t.TempDir(), "read-only")
	require.NoError(t, os.Mkdir(readOnlyParent, 0o700))
	require.NoError(t, os.Chmod(readOnlyParent, 0o500))
	t.Cleanup(func() {
		require.NoError(t, os.Chmod(readOnlyParent, 0o700))
	})

	probePath := filepath.Join(readOnlyParent, "permission-probe")
	if err := os.WriteFile(probePath, []byte("probe"), 0o600); err == nil {
		require.NoError(t, os.Remove(probePath))
		require.NoError(t, os.Chmod(readOnlyParent, 0o700))
		t.Skip("filesystem does not enforce directory write permissions for this user")
	}

	cnsPath := filepath.Join(readOnlyParent, "missing", "azure-cns.json")
	endpointPath := filepath.Join(t.TempDir(), "azure-endpoints.json")
	before := readRollbackTransition(ctx, t, db)

	require.Error(t, db.ExportLegacy(ctx, cnsPath, endpointPath))

	after := readRollbackTransition(ctx, t, db)
	assert.Equal(t, AuthorityBolt, after.authority)
	assert.False(t, after.rollbackComplete)
	assert.Equal(t, before.generation, after.generation)
	assertRollbackPathMissing(t, endpointPath)
	assertNoRollbackTemporaryFiles(t, cnsPath, endpointPath)
}
