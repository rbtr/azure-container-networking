// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//nolint:goconst // Repeated values make state fixtures easier to read.
package state_test

import (
	"context"
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/state"
	nncv1alpha "github.com/Azure/azure-container-networking/crd/nodenetworkconfig/api/v1alpha"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

var errStopTransaction = errors.New("stop transaction")

const (
	testNCID      = "nc-1"
	testIPID      = "ip-uuid-1"
	testIPAddress = "10.0.0.4"
)

func openTestDB(t *testing.T) (db *state.DB, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "azure-cns.db")
	db, err := state.Open(path, state.Options{})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	return db, path
}

func sampleNCRecord() state.NetworkContainerRecord {
	return state.NewNetworkContainerRecord(
		testNCID,
		"2",
		"1",
		true,
		cns.CreateNetworkContainerRequest{
			NetworkContainerid: testNCID,
			Version:            "2",
			AuthorizationToken: "must-not-persist",
			IPv6Configuration: cns.IPConfiguration{
				IPSubnet: cns.IPSubnet{IPAddress: "fd00::", PrefixLength: 120},
			},
			EndpointPolicies: []cns.NetworkContainerRequestPolicies{{Type: "acl"}},
			NCStatus:         nncv1alpha.NCUpdateSuccess,
		},
	)
}

func sampleIPs() map[string]state.IPRecord {
	return map[string]state.IPRecord{
		testIPID: {
			ID:        testIPID,
			IPAddress: testIPAddress,
			NCID:      testNCID,
			NCVersion: 2,
		},
	}
}

func TestDBPersistsCompleteNCState(t *testing.T) {
	ctx := context.Background()
	db, path := openTestDB(t)

	require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))
	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)

	record := snapshot.NetworkContainers[testNCID]
	assert.Empty(t, record.Request.AuthorizationToken)
	assert.Equal(t, "fd00::", record.Request.IPv6Configuration.IPSubnet.IPAddress)
	require.Len(t, record.Request.EndpointPolicies, 1)
	assert.Equal(t, nncv1alpha.NCUpdateSuccess, record.Request.NCStatus)
	assert.Equal(t, testIPAddress, snapshot.IPs[testIPID].IPAddress)

	require.NoError(t, db.Close())
	reopened, err := state.Open(path, state.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })

	reopenedSnapshot, err := reopened.Snapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, snapshot.NetworkContainers, reopenedSnapshot.NetworkContainers)
	assert.Equal(t, snapshot.IPs, reopenedSnapshot.IPs)
}

func TestDBUpdateAbortsAtomically(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	err := db.Update(ctx, func(tx *state.WriteTx) error {
		require.NoError(t, tx.PutNetworkContainer(sampleNCRecord()))
		return errStopTransaction
	})
	require.ErrorIs(t, err, errStopTransaction)

	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Empty(t, snapshot.NetworkContainers)
}

func TestDBRejectsSchemaMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.db")
	db, err := state.Open(path, state.Options{})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	raw, err := bolt.Open(path, 0o600, nil)
	require.NoError(t, err)
	require.NoError(t, raw.Update(func(tx *bolt.Tx) error {
		version := make([]byte, 4)
		binary.LittleEndian.PutUint32(version, state.SchemaVersion+1)
		return tx.Bucket([]byte("metadata")).Put([]byte("schema_version"), version)
	}))
	require.NoError(t, raw.Close())

	_, err = state.Open(path, state.Options{})
	require.ErrorIs(t, err, state.ErrSchemaMismatch)
}

func TestSnapshotRejectsDanglingIP(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.Update(ctx, func(tx *state.WriteTx) error {
		return tx.PutIP(state.IPRecord{ID: "ip-1", IPAddress: "10.0.0.4", NCID: "missing"})
	}))

	_, err := db.Snapshot(ctx)
	require.ErrorIs(t, err, state.ErrInconsistentState)
}

func TestSnapshotRejectsCorruptRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	db, err := state.Open(path, state.Options{})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	raw, err := bolt.Open(path, 0o600, nil)
	require.NoError(t, err)
	require.NoError(t, raw.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("endpoints")).Put([]byte("container-1"), []byte("{"))
	}))
	require.NoError(t, raw.Close())

	db, err = state.Open(path, state.Options{})
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Snapshot(context.Background())
	require.Error(t, err)
}

func TestApplyNetworkContainerRejectsRemovingAssignedIP(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))

	endpoint := sampleEndpoint("10.0.0.4")
	assignment := state.AssignmentRecord{
		Pod: state.PodIdentity{
			PodKey:           "container-1",
			InfraContainerID: "container-1",
		},
		IPIDs: []string{"ip-uuid-1"},
	}
	require.NoError(t, db.AssignEndpoint(ctx, assignment, endpoint, testNow, testIntentTTL))

	err := db.ApplyNetworkContainer(ctx, sampleNCRecord(), map[string]state.IPRecord{})
	require.ErrorIs(t, err, state.ErrInconsistentState)

	snapshot, snapshotErr := db.Snapshot(ctx)
	require.NoError(t, snapshotErr)
	assert.Contains(t, snapshot.IPs, testIPID)
}
