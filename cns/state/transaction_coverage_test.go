// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//nolint:goconst,wrapcheck // Repeated fixtures and direct callback propagation keep tests readable.
package state_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/Azure/azure-container-networking/cns/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

func putAllTransactionRecords(tx *state.WriteTx) error {
	meta, err := tx.Metadata()
	if err != nil {
		return err
	}
	meta.BootID = testBootID
	meta.NodeID = "node-1"
	if err := tx.PutMetadata(meta); err != nil {
		return err
	}
	if err := tx.PutNetworkContainer(sampleNCRecord()); err != nil {
		return err
	}
	if err := tx.PutIP(sampleIPs()[testIPID]); err != nil {
		return err
	}
	if err := tx.PutNetwork(state.NetworkRecord{NetworkName: "network-1"}); err != nil {
		return err
	}
	if err := tx.PutOrchestratorContext("orchestrator-1", []string{testNCID}); err != nil {
		return err
	}
	if err := tx.PutPnPIDByMAC("00:11:22:33:44:55", "pnp-1"); err != nil {
		return err
	}
	if err := tx.PutAssignment(testAssignment(testIPID)); err != nil {
		return err
	}
	if err := tx.PutIPOwner(testIPID, testContainerID); err != nil {
		return err
	}
	if err := tx.PutEndpoint(testContainerID, sampleEndpoint(testIPAddress)); err != nil {
		return err
	}
	if err := tx.PutDeleteIntent("deleted", state.DeleteIntent{CreatedAt: testNow}); err != nil {
		return err
	}
	if err := tx.SetMigrationComplete(); err != nil {
		return err
	}
	return tx.SetRollbackComplete()
}

func TestTransactionGettersCollectionsAndDeletes(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.Update(ctx, putAllTransactionRecords))

	require.NoError(t, db.View(ctx, func(tx *state.ReadTx) error {
		meta, err := tx.Metadata()
		require.NoError(t, err)
		assert.Equal(t, testBootID, meta.BootID)
		assert.Equal(t, "node-1", meta.NodeID)

		nc, err := tx.NetworkContainer(testNCID)
		require.NoError(t, err)
		assert.Equal(t, testNCID, nc.ID)
		ncs, err := tx.NetworkContainers()
		require.NoError(t, err)
		assert.Equal(t, nc, ncs[testNCID])

		ip, err := tx.IP(testIPID)
		require.NoError(t, err)
		assert.Equal(t, testIPAddress, ip.IPAddress)
		ips, err := tx.IPs()
		require.NoError(t, err)
		assert.Equal(t, ip, ips[testIPID])

		network, err := tx.Network("network-1")
		require.NoError(t, err)
		assert.Equal(t, "network-1", network.NetworkName)
		networks, err := tx.Networks()
		require.NoError(t, err)
		assert.Equal(t, network, networks["network-1"])

		contexts, err := tx.OrchestratorContexts()
		require.NoError(t, err)
		assert.Equal(t, []string{testNCID}, contexts["orchestrator-1"])
		pnpIDs, err := tx.PnPIDByMAC()
		require.NoError(t, err)
		assert.Equal(t, "pnp-1", pnpIDs["00:11:22:33:44:55"])

		assignment, err := tx.Assignment(testContainerID)
		require.NoError(t, err)
		assert.Equal(t, []string{testIPID}, assignment.IPIDs)
		assignments, err := tx.Assignments()
		require.NoError(t, err)
		assert.Equal(t, assignment, assignments[testContainerID])

		owner, err := tx.IPOwner(testIPID)
		require.NoError(t, err)
		assert.Equal(t, testContainerID, owner)
		owners, err := tx.IPOwners()
		require.NoError(t, err)
		assert.Equal(t, owner, owners[testIPID])

		endpoint, err := tx.Endpoint(testContainerID)
		require.NoError(t, err)
		endpoints, err := tx.Endpoints()
		require.NoError(t, err)
		assert.Equal(t, endpoint, endpoints[testContainerID])

		intent, err := tx.DeleteIntent("deleted")
		require.NoError(t, err)
		intents, err := tx.DeleteIntents()
		require.NoError(t, err)
		assert.Equal(t, intent, intents["deleted"])
		assert.True(t, tx.MigrationComplete())
		assert.True(t, tx.RollbackComplete())
		return nil
	}))

	require.NoError(t, db.Update(ctx, func(tx *state.WriteTx) error {
		if err := tx.DeleteNetworkContainer(testNCID); err != nil {
			return err
		}
		if err := tx.DeleteIP(testIPID); err != nil {
			return err
		}
		if err := tx.DeleteNetwork("network-1"); err != nil {
			return err
		}
		if err := tx.DeleteOrchestratorContext("orchestrator-1"); err != nil {
			return err
		}
		if err := tx.DeletePnPIDByMAC("00:11:22:33:44:55"); err != nil {
			return err
		}
		if err := tx.DeleteAssignment(testContainerID); err != nil {
			return err
		}
		if err := tx.DeleteIPOwner(testIPID); err != nil {
			return err
		}
		if err := tx.DeleteEndpoint(testContainerID); err != nil {
			return err
		}
		if err := tx.DeleteDeleteIntent("deleted"); err != nil {
			return err
		}
		return tx.ClearRollbackComplete()
	}))

	require.NoError(t, db.View(ctx, func(tx *state.ReadTx) error {
		_, err := tx.NetworkContainer(testNCID)
		require.ErrorIs(t, err, state.ErrNotFound)
		_, err = tx.IP(testIPID)
		require.ErrorIs(t, err, state.ErrNotFound)
		_, err = tx.Network("network-1")
		require.ErrorIs(t, err, state.ErrNotFound)
		_, err = tx.Assignment(testContainerID)
		require.ErrorIs(t, err, state.ErrNotFound)
		_, err = tx.IPOwner(testIPID)
		require.ErrorIs(t, err, state.ErrNotFound)
		_, err = tx.Endpoint(testContainerID)
		require.ErrorIs(t, err, state.ErrNotFound)
		_, err = tx.DeleteIntent("deleted")
		require.ErrorIs(t, err, state.ErrNotFound)

		ncs, err := tx.NetworkContainers()
		require.NoError(t, err)
		assert.Empty(t, ncs)
		ips, err := tx.IPs()
		require.NoError(t, err)
		assert.Empty(t, ips)
		networks, err := tx.Networks()
		require.NoError(t, err)
		assert.Empty(t, networks)
		contexts, err := tx.OrchestratorContexts()
		require.NoError(t, err)
		assert.Empty(t, contexts)
		pnpIDs, err := tx.PnPIDByMAC()
		require.NoError(t, err)
		assert.Empty(t, pnpIDs)
		assignments, err := tx.Assignments()
		require.NoError(t, err)
		assert.Empty(t, assignments)
		owners, err := tx.IPOwners()
		require.NoError(t, err)
		assert.Empty(t, owners)
		endpoints, err := tx.Endpoints()
		require.NoError(t, err)
		assert.Empty(t, endpoints)
		intents, err := tx.DeleteIntents()
		require.NoError(t, err)
		assert.Empty(t, intents)
		assert.True(t, tx.MigrationComplete())
		assert.False(t, tx.RollbackComplete())
		return nil
	}))
}

func TestClearDurableAndCompleteState(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.Update(ctx, putAllTransactionRecords))

	require.NoError(t, db.Update(ctx, func(tx *state.WriteTx) error {
		return tx.ClearDurableState()
	}))
	require.NoError(t, db.View(ctx, func(tx *state.ReadTx) error {
		ncs, err := tx.NetworkContainers()
		require.NoError(t, err)
		assert.Empty(t, ncs)
		ips, err := tx.IPs()
		require.NoError(t, err)
		assert.Empty(t, ips)
		networks, err := tx.Networks()
		require.NoError(t, err)
		assert.Empty(t, networks)
		contexts, err := tx.OrchestratorContexts()
		require.NoError(t, err)
		assert.Empty(t, contexts)
		pnpIDs, err := tx.PnPIDByMAC()
		require.NoError(t, err)
		assert.Empty(t, pnpIDs)

		assignments, err := tx.Assignments()
		require.NoError(t, err)
		assert.NotEmpty(t, assignments)
		owners, err := tx.IPOwners()
		require.NoError(t, err)
		assert.NotEmpty(t, owners)
		endpoints, err := tx.Endpoints()
		require.NoError(t, err)
		assert.NotEmpty(t, endpoints)
		intents, err := tx.DeleteIntents()
		require.NoError(t, err)
		assert.NotEmpty(t, intents)
		return nil
	}))

	require.NoError(t, db.Update(ctx, func(tx *state.WriteTx) error {
		return tx.ClearState()
	}))
	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Empty(t, snapshot.NetworkContainers)
	assert.Empty(t, snapshot.IPs)
	assert.Empty(t, snapshot.Networks)
	assert.Empty(t, snapshot.OrchestratorContexts)
	assert.Empty(t, snapshot.PnPIDByMAC)
	assert.Empty(t, snapshot.Assignments)
	assert.Empty(t, snapshot.IPOwners)
	assert.Empty(t, snapshot.Endpoints)
	assert.Empty(t, snapshot.DeleteIntents)
}

func TestSnapshotCorruptionAttributesBucketAndKey(t *testing.T) {
	tests := []struct {
		name   string
		bucket string
		key    string
	}{
		{name: "metadata", bucket: "metadata", key: "service"},
		{name: "network containers", bucket: "network_containers", key: testNCID},
		{name: "IPs", bucket: "ips", key: testIPID},
		{name: "networks", bucket: "networks", key: "network-1"},
		{name: "orchestrator contexts", bucket: "orchestrator_contexts", key: "orchestrator-1"},
		{name: "assignments", bucket: "assignments", key: testContainerID},
		{name: "endpoints", bucket: "endpoints", key: testContainerID},
		{name: "delete intents", bucket: "delete_intents", key: testContainerID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "corrupt.db")
			db, err := state.Open(path, state.Options{})
			require.NoError(t, err)
			require.NoError(t, db.Close())

			raw, err := bolt.Open(path, 0o600, nil)
			require.NoError(t, err)
			require.NoError(t, raw.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(tt.bucket)).Put([]byte(tt.key), []byte("{"))
			}))
			require.NoError(t, raw.Close())

			db, err = state.Open(path, state.Options{})
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, db.Close())
			})
			_, err = db.Snapshot(context.Background())
			require.Error(t, err)
			require.ErrorContains(t, err, fmt.Sprintf("bucket %q", tt.bucket))
			require.ErrorContains(t, err, fmt.Sprintf("%q", tt.key))
		})
	}
}

func TestDBBoundaryFailures(t *testing.T) {
	var nilDB *state.DB
	require.NoError(t, nilDB.Close())

	t.Run("open directory", func(t *testing.T) {
		_, err := state.Open(t.TempDir(), state.Options{})
		require.Error(t, err)
	})

	t.Run("canceled context", func(t *testing.T) {
		db, _ := openTestDB(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := db.View(ctx, func(*state.ReadTx) error {
			return nil
		})
		require.ErrorIs(t, err, context.Canceled)
		err = db.Update(ctx, func(*state.WriteTx) error {
			return nil
		})
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("closed database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "closed.db")
		db, err := state.Open(path, state.Options{})
		require.NoError(t, err)
		require.NoError(t, db.Close())

		err = db.View(context.Background(), func(*state.ReadTx) error {
			return nil
		})
		require.ErrorIs(t, err, bolterrors.ErrDatabaseNotOpen)
		err = db.Update(context.Background(), func(*state.WriteTx) error {
			return nil
		})
		require.ErrorIs(t, err, bolterrors.ErrDatabaseNotOpen)
		_, err = db.Snapshot(context.Background())
		require.ErrorIs(t, err, bolterrors.ErrDatabaseNotOpen)
	})

	t.Run("callback error", func(t *testing.T) {
		db, _ := openTestDB(t)
		err := db.View(context.Background(), func(*state.ReadTx) error {
			return errStopTransaction
		})
		require.ErrorIs(t, err, errStopTransaction)
	})
}
