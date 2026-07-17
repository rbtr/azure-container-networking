// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//nolint:goconst,wrapcheck // Repeated fixtures and direct callback propagation keep tests readable.
package state_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

func TestImportLegacyValidationFailures(t *testing.T) {
	tests := []struct {
		name      string
		writeData func(*testing.T, string, string)
		wantErr   error
	}{
		{
			name: "invalid CNS envelope",
			writeData: func(t *testing.T, cnsPath, _ string) {
				t.Helper()
				require.NoError(t, os.WriteFile(cnsPath, []byte("{"), 0o600))
			},
		},
		{
			name: "invalid nested CNS state",
			writeData: func(t *testing.T, cnsPath, _ string) {
				t.Helper()
				require.NoError(t, os.WriteFile(cnsPath, []byte(`{"ContainerNetworkService":"invalid"}`), 0o600))
			},
		},
		{
			name: "invalid nested endpoint state",
			writeData: func(t *testing.T, _, endpointPath string) {
				t.Helper()
				require.NoError(t, os.WriteFile(endpointPath, []byte(`{"Endpoints":"invalid"}`), 0o600))
			},
		},
		{
			name: "invalid nested delete intents",
			writeData: func(t *testing.T, _, endpointPath string) {
				t.Helper()
				require.NoError(t, os.WriteFile(endpointPath, []byte(`{"EndpointDeleteIntents":"invalid"}`), 0o600))
			},
		},
		{
			name: "empty IP ID",
			writeData: func(t *testing.T, cnsPath, _ string) {
				t.Helper()
				writeEnvelope(t, cnsPath, map[string]any{
					"ContainerNetworkService": map[string]any{
						"ContainerStatus": map[string]any{
							testNCID: map[string]any{
								"ID": testNCID,
								"CreateNetworkContainerRequest": cns.CreateNetworkContainerRequest{
									SecondaryIPConfigs: map[string]cns.SecondaryIPConfig{
										"": {IPAddress: testIPAddress},
									},
								},
							},
						},
					},
				})
			},
			wantErr: state.ErrInconsistentState,
		},
		{
			name: "duplicate IP ID",
			writeData: func(t *testing.T, cnsPath, _ string) {
				t.Helper()
				writeEnvelope(t, cnsPath, map[string]any{
					"ContainerNetworkService": map[string]any{
						"ContainerStatus": map[string]any{
							"nc-1": map[string]any{
								"ID": "nc-1",
								"CreateNetworkContainerRequest": cns.CreateNetworkContainerRequest{
									SecondaryIPConfigs: map[string]cns.SecondaryIPConfig{
										testIPID: {IPAddress: "10.0.0.4"},
									},
								},
							},
							"nc-2": map[string]any{
								"ID": "nc-2",
								"CreateNetworkContainerRequest": cns.CreateNetworkContainerRequest{
									SecondaryIPConfigs: map[string]cns.SecondaryIPConfig{
										testIPID: {IPAddress: "10.0.1.4"},
									},
								},
							},
						},
					},
				})
			},
			wantErr: state.ErrInconsistentState,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cnsPath := filepath.Join(dir, "azure-cns.json")
			endpointPath := filepath.Join(dir, "azure-endpoints.json")
			tt.writeData(t, cnsPath, endpointPath)
			db, _ := openTestDB(t)

			err := db.ImportLegacy(context.Background(), state.ImportOptions{
				CNSJSONPath:         cnsPath,
				EndpointJSONPath:    endpointPath,
				ManageEndpointState: true,
				BootID:              testBootID,
				Now:                 testNow,
				DeleteIntentTTL:     testIntentTTL,
			})
			require.Error(t, err)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			}
		})
	}
}

func TestImportLegacyRejectsNonEmptyBoltState(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))

	err := db.ImportLegacy(ctx, state.ImportOptions{BootID: testBootID, Now: testNow})
	require.ErrorIs(t, err, state.ErrInconsistentState)

	snapshot, snapshotErr := db.Snapshot(ctx)
	require.NoError(t, snapshotErr)
	assert.Contains(t, snapshot.NetworkContainers, testNCID)
}

func TestImportLegacyClearsJSONAuthoritativeState(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))
	require.NoError(t, db.Update(ctx, func(tx *state.WriteTx) error {
		meta, err := tx.Metadata()
		if err != nil {
			return err
		}
		meta.Authority = state.AuthorityJSON
		return tx.PutMetadata(meta)
	}))

	require.NoError(t, db.ImportLegacy(ctx, state.ImportOptions{BootID: testBootID, Now: testNow}))

	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, state.AuthorityBolt, snapshot.Metadata.Authority)
	assert.Equal(t, testBootID, snapshot.Metadata.BootID)
	assert.Empty(t, snapshot.NetworkContainers)
	require.NoError(t, db.View(ctx, func(tx *state.ReadTx) error {
		assert.True(t, tx.MigrationComplete())
		assert.False(t, tx.RollbackComplete())
		return nil
	}))
}

func TestImportLegacyPrunesExpiredDeleteIntents(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	endpointPath := filepath.Join(dir, "azure-endpoints.json")
	writeEnvelope(t, endpointPath, map[string]any{
		"EndpointDeleteIntents": map[string]any{
			"expired": map[string]any{
				"createdAt": testNow.Add(-testIntentTTL),
			},
			"active": map[string]any{
				"createdAt": testNow.Add(-testIntentTTL + 1),
			},
			"zero": map[string]any{},
		},
	})
	db, _ := openTestDB(t)

	require.NoError(t, db.ImportLegacy(ctx, state.ImportOptions{
		EndpointJSONPath: endpointPath,
		BootID:           testBootID,
		Now:              testNow,
		DeleteIntentTTL:  testIntentTTL,
	}))

	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Contains(t, snapshot.DeleteIntents, "active")
	assert.NotContains(t, snapshot.DeleteIntents, "expired")
	assert.NotContains(t, snapshot.DeleteIntents, "zero")
}

func TestMigrationPersistenceFailures(t *testing.T) {
	t.Run("ImportLegacy read-only database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "read-only.db")
		db, err := state.Open(path, state.Options{})
		require.NoError(t, err)
		require.NoError(t, db.Close())
		db, err = state.Open(path, state.Options{ReadOnly: true})
		require.NoError(t, err)
		t.Cleanup(func() {
			require.NoError(t, db.Close())
		})

		err = db.ImportLegacy(context.Background(), state.ImportOptions{BootID: testBootID, Now: testNow})
		require.ErrorIs(t, err, bolterrors.ErrDatabaseReadOnly)
	})

	t.Run("ExportLegacy read-only database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "read-only.db")
		db, err := state.Open(path, state.Options{})
		require.NoError(t, err)
		require.NoError(t, db.ApplyNetworkContainer(context.Background(), sampleNCRecord(), sampleIPs()))
		require.NoError(t, db.Close())
		db, err = state.Open(path, state.Options{ReadOnly: true})
		require.NoError(t, err)
		t.Cleanup(func() {
			require.NoError(t, db.Close())
		})
		dir := t.TempDir()

		err = db.ExportLegacy(
			context.Background(),
			filepath.Join(dir, "azure-cns.json"),
			filepath.Join(dir, "azure-endpoints.json"),
		)
		require.ErrorIs(t, err, bolterrors.ErrDatabaseReadOnly)
		snapshot, snapshotErr := db.Snapshot(context.Background())
		require.NoError(t, snapshotErr)
		assert.Equal(t, state.AuthorityBolt, snapshot.Metadata.Authority)
	})
}

func TestExportLegacyFailurePaths(t *testing.T) {
	t.Run("closed database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "closed.db")
		db, err := state.Open(path, state.Options{})
		require.NoError(t, err)
		require.NoError(t, db.Close())

		err = db.ExportLegacy(context.Background(), "", "")
		require.ErrorIs(t, err, bolterrors.ErrDatabaseNotOpen)
	})

	t.Run("corrupt snapshot", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "corrupt.db")
		db, err := state.Open(path, state.Options{})
		require.NoError(t, err)
		require.NoError(t, db.Close())
		raw, err := bolt.Open(path, 0o600, nil)
		require.NoError(t, err)
		require.NoError(t, raw.Update(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte("network_containers")).Put([]byte(testNCID), []byte("{"))
		}))
		require.NoError(t, raw.Close())
		db, err = state.Open(path, state.Options{})
		require.NoError(t, err)
		t.Cleanup(func() {
			require.NoError(t, db.Close())
		})

		err = db.ExportLegacy(context.Background(), "", "")
		require.ErrorContains(t, err, `bucket "network_containers"`)
	})

	t.Run("CNS target is a directory", func(t *testing.T) {
		ctx := context.Background()
		db, _ := openTestDB(t)
		require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))
		dir := t.TempDir()
		cnsPath := filepath.Join(dir, "azure-cns.json")
		require.NoError(t, os.Mkdir(cnsPath, 0o755))

		err := db.ExportLegacy(ctx, cnsPath, filepath.Join(dir, "azure-endpoints.json"))
		require.Error(t, err)
	})
}

func TestMigrationOperationsRespectCanceledContext(t *testing.T) {
	db, _ := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := db.ImportLegacy(ctx, state.ImportOptions{})
	require.ErrorIs(t, err, context.Canceled)
	err = db.ExportLegacy(ctx, "", "")
	require.ErrorIs(t, err, context.Canceled)
}
