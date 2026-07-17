// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//nolint:goconst,wrapcheck // Repeated fixtures and direct callback propagation keep tests readable.
package state_test

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-container-networking/cns/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

func testAssignment(ipIDs ...string) state.AssignmentRecord {
	return state.AssignmentRecord{
		Pod: state.PodIdentity{
			PodKey:           testContainerID,
			InfraContainerID: testContainerID,
			PodName:          testPodName,
			PodNamespace:     testNamespace,
		},
		IPIDs: ipIDs,
	}
}

func invalidPrefixEndpoint() state.EndpointRecord {
	endpoint := sampleEndpoint(testIPAddress)
	endpoint.IfnameToIPMap[testIfName].IPv4[0].Mask = net.IPMask{0xff, 0, 0xff, 0}
	return endpoint
}

func openReadOnlyOperationDB(t *testing.T) (*state.DB, state.Snapshot) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "read-only.db")
	writable, err := state.Open(path, state.Options{})
	require.NoError(t, err)
	require.NoError(t, writable.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))
	snapshot, err := writable.Snapshot(ctx)
	require.NoError(t, err)
	require.NoError(t, writable.Close())

	readOnly, err := state.Open(path, state.Options{ReadOnly: true})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, readOnly.Close())
	})
	return readOnly, snapshot
}

func openCorruptedOperationDB(
	t *testing.T,
	seed func(context.Context, *state.DB) error,
	bucket, key string,
) *state.DB {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "corrupt-operation.db")
	db, err := state.Open(path, state.Options{})
	require.NoError(t, err)
	if seed != nil {
		require.NoError(t, seed(ctx, db))
	}
	require.NoError(t, db.Close())

	raw, err := bolt.Open(path, 0o600, nil)
	require.NoError(t, err)
	require.NoError(t, raw.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucket)).Put([]byte(key), []byte("{"))
	}))
	require.NoError(t, raw.Close())

	db, err = state.Open(path, state.Options{})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	return db
}

func TestDeleteNetworkContainerBehavior(t *testing.T) {
	t.Run("rejects owned IPs without mutation", func(t *testing.T) {
		ctx := context.Background()
		db, _ := openTestDB(t)
		require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))
		require.NoError(t, db.AssignEndpoint(
			ctx,
			testAssignment(testIPID),
			sampleEndpoint(testIPAddress),
			testNow,
			testIntentTTL,
		))
		before, err := db.Snapshot(ctx)
		require.NoError(t, err)

		err = db.DeleteNetworkContainer(ctx, testNCID)
		require.ErrorIs(t, err, state.ErrInconsistentState)

		after, snapshotErr := db.Snapshot(ctx)
		require.NoError(t, snapshotErr)
		assert.Equal(t, before, after)
	})

	t.Run("deletes an unowned NC and its IPs", func(t *testing.T) {
		ctx := context.Background()
		db, _ := openTestDB(t)
		require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))

		require.NoError(t, db.DeleteNetworkContainer(ctx, testNCID))

		snapshot, err := db.Snapshot(ctx)
		require.NoError(t, err)
		assert.NotContains(t, snapshot.NetworkContainers, testNCID)
		assert.NotContains(t, snapshot.IPs, testIPID)
	})

	t.Run("rejects an empty NC ID", func(t *testing.T) {
		db, _ := openTestDB(t)
		err := db.DeleteNetworkContainer(context.Background(), "")
		require.ErrorIs(t, err, state.ErrInvalidInput)
	})
}

func TestDeleteEndpointRecordBehavior(t *testing.T) {
	t.Run("rejects a referenced endpoint without mutation", func(t *testing.T) {
		ctx := context.Background()
		db, _ := openTestDB(t)
		require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))
		require.NoError(t, db.AssignEndpoint(
			ctx,
			testAssignment(testIPID),
			sampleEndpoint(testIPAddress),
			testNow,
			testIntentTTL,
		))
		before, err := db.Snapshot(ctx)
		require.NoError(t, err)

		err = db.DeleteEndpointRecord(ctx, testContainerID)
		require.ErrorIs(t, err, state.ErrInconsistentState)

		after, snapshotErr := db.Snapshot(ctx)
		require.NoError(t, snapshotErr)
		assert.Equal(t, before, after)
	})

	t.Run("deletes an unreferenced endpoint", func(t *testing.T) {
		ctx := context.Background()
		db, _ := openTestDB(t)
		require.NoError(t, db.ReplaceManagedEndpoints(ctx, map[string]state.EndpointRecord{
			testContainerID: sampleEndpoint(testIPAddress),
		}))

		require.NoError(t, db.DeleteEndpointRecord(ctx, testContainerID))

		snapshot, err := db.Snapshot(ctx)
		require.NoError(t, err)
		assert.NotContains(t, snapshot.Endpoints, testContainerID)
	})

	t.Run("rejects an empty endpoint ID", func(t *testing.T) {
		db, _ := openTestDB(t)
		err := db.DeleteEndpointRecord(context.Background(), "")
		require.ErrorIs(t, err, state.ErrInvalidInput)
	})
}

func TestPruneDeleteIntentsBehavior(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.Update(ctx, func(tx *state.WriteTx) error {
		if err := tx.PutDeleteIntent("expired", state.DeleteIntent{CreatedAt: testNow.Add(-testIntentTTL)}); err != nil {
			return err
		}
		if err := tx.PutDeleteIntent("active", state.DeleteIntent{CreatedAt: testNow.Add(-testIntentTTL + time.Nanosecond)}); err != nil {
			return err
		}
		return tx.PutDeleteIntent("zero", state.DeleteIntent{})
	}))

	require.NoError(t, db.PruneDeleteIntents(ctx, testNow, testIntentTTL))

	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]state.DeleteIntent{
		"active": {CreatedAt: testNow.Add(-testIntentTTL + time.Nanosecond)},
	}, snapshot.DeleteIntents)
}

func TestReplaceDurableStateFailureIsAtomic(t *testing.T) {
	t.Run("stale generation is unchanged", func(t *testing.T) {
		ctx := context.Background()
		db, _ := openTestDB(t)
		stale, err := db.Snapshot(ctx)
		require.NoError(t, err)
		stale.Networks["stale"] = state.NetworkRecord{NetworkName: "stale"}

		require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))
		current, err := db.Snapshot(ctx)
		require.NoError(t, err)

		err = db.ReplaceDurableState(ctx, stale)
		require.ErrorIs(t, err, state.ErrStaleGeneration)

		after, snapshotErr := db.Snapshot(ctx)
		require.NoError(t, snapshotErr)
		assert.Equal(t, current, after)
	})

	t.Run("retained endpoint cannot reference a removed IP", func(t *testing.T) {
		ctx := context.Background()
		db, _ := openTestDB(t)
		require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))
		require.NoError(t, db.AssignEndpoint(
			ctx,
			testAssignment(testIPID),
			sampleEndpoint(testIPAddress),
			testNow,
			testIntentTTL,
		))
		before, err := db.Snapshot(ctx)
		require.NoError(t, err)

		next := state.NewSnapshot()
		next.Metadata = before.Metadata
		next.NetworkContainers[testNCID] = before.NetworkContainers[testNCID]
		err = db.ReplaceDurableState(ctx, next)
		require.ErrorIs(t, err, state.ErrInconsistentState)

		after, snapshotErr := db.Snapshot(ctx)
		require.NoError(t, snapshotErr)
		assert.Equal(t, before, after)
	})
}

func TestReplaceDurableStatePersistsEveryDurableCollection(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.ReplaceManagedEndpoints(ctx, map[string]state.EndpointRecord{
		testContainerID: sampleEndpoint(testIPAddress),
	}))
	before, err := db.Snapshot(ctx)
	require.NoError(t, err)

	next := state.NewSnapshot()
	next.Metadata = before.Metadata
	next.NetworkContainers[testNCID] = sampleNCRecord()
	next.IPs[testIPID] = sampleIPs()[testIPID]
	next.Networks["network-1"] = state.NetworkRecord{NetworkName: "network-1"}
	next.OrchestratorContexts["orchestrator-1"] = []string{testNCID}
	next.PnPIDByMAC["00:11:22:33:44:55"] = "pnp-1"
	require.NoError(t, db.ReplaceDurableState(ctx, next))

	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, testNCID, snapshot.NetworkContainers[testNCID].ID)
	assert.Empty(t, snapshot.NetworkContainers[testNCID].Request.AuthorizationToken)
	assert.Equal(t, next.IPs, snapshot.IPs)
	assert.Equal(t, next.Networks, snapshot.Networks)
	assert.Equal(t, next.OrchestratorContexts, snapshot.OrchestratorContexts)
	assert.Equal(t, next.PnPIDByMAC, snapshot.PnPIDByMAC)
	assert.Equal(t, []string{testIPID}, snapshot.Assignments[testContainerID].IPIDs)
	assert.Equal(t, testContainerID, snapshot.IPOwners[testIPID])
	assert.Contains(t, snapshot.Endpoints, testContainerID)
}

func TestReplaceDurableStateValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*state.Snapshot)
		wantErr error
	}{
		{
			name: "schema mismatch",
			mutate: func(snapshot *state.Snapshot) {
				snapshot.Metadata.SchemaVersion++
			},
			wantErr: state.ErrSchemaMismatch,
		},
		{
			name: "NC key mismatch",
			mutate: func(snapshot *state.Snapshot) {
				record := snapshot.NetworkContainers[testNCID]
				delete(snapshot.NetworkContainers, testNCID)
				snapshot.NetworkContainers["different"] = record
			},
			wantErr: state.ErrInconsistentState,
		},
		{
			name: "invalid IP",
			mutate: func(snapshot *state.Snapshot) {
				record := snapshot.IPs[testIPID]
				record.IPAddress = "not-an-ip"
				snapshot.IPs[testIPID] = record
			},
			wantErr: state.ErrInconsistentState,
		},
		{
			name: "missing NC reference",
			mutate: func(snapshot *state.Snapshot) {
				delete(snapshot.NetworkContainers, testNCID)
			},
			wantErr: state.ErrInconsistentState,
		},
		{
			name: "invalid NC prefix",
			mutate: func(snapshot *state.Snapshot) {
				record := snapshot.NetworkContainers[testNCID]
				record.Request.IPConfiguration.IPSubnet.IPAddress = "10.0.0.0"
				record.Request.IPConfiguration.IPSubnet.PrefixLength = 64
				snapshot.NetworkContainers[testNCID] = record
			},
			wantErr: state.ErrInconsistentState,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			db, _ := openTestDB(t)
			before, err := db.Snapshot(ctx)
			require.NoError(t, err)
			next := state.NewSnapshot()
			next.Metadata = before.Metadata
			next.NetworkContainers[testNCID] = sampleNCRecord()
			next.IPs[testIPID] = sampleIPs()[testIPID]
			tt.mutate(&next)

			err = db.ReplaceDurableState(ctx, next)
			require.ErrorIs(t, err, tt.wantErr)

			after, snapshotErr := db.Snapshot(ctx)
			require.NoError(t, snapshotErr)
			assert.Equal(t, before, after)
		})
	}
}

func TestApplyNetworkContainerReconcilesUnownedInventory(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	record := sampleNCRecord()
	initialIPs := map[string]state.IPRecord{
		testIPID: sampleIPs()[testIPID],
		"ip-old": {
			ID:        "ip-old",
			IPAddress: "10.0.0.5",
			NCID:      testNCID,
			NCVersion: 2,
		},
	}
	require.NoError(t, db.ApplyNetworkContainer(ctx, record, initialIPs))

	otherRecord := record
	otherRecord.ID = "nc-2"
	otherIP := state.IPRecord{
		ID:        "ip-other",
		IPAddress: "10.0.1.4",
		NCID:      otherRecord.ID,
		NCVersion: 2,
	}
	require.NoError(t, db.ApplyNetworkContainer(ctx, otherRecord, map[string]state.IPRecord{otherIP.ID: otherIP}))

	nextIPs := map[string]state.IPRecord{
		testIPID: sampleIPs()[testIPID],
		"ip-new": {
			ID:        "ip-new",
			IPAddress: "10.0.0.6",
			NCID:      testNCID,
			NCVersion: 3,
		},
	}
	require.NoError(t, db.ApplyNetworkContainer(ctx, record, nextIPs))

	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Contains(t, snapshot.IPs, testIPID)
	assert.Contains(t, snapshot.IPs, "ip-new")
	assert.NotContains(t, snapshot.IPs, "ip-old")
	assert.Equal(t, otherIP, snapshot.IPs[otherIP.ID])
	assert.Contains(t, snapshot.NetworkContainers, otherRecord.ID)
}

func TestAssignEndpointReplacesExistingOwnership(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	ips := map[string]state.IPRecord{
		testIPID: sampleIPs()[testIPID],
		"ip-2": {
			ID:        "ip-2",
			IPAddress: "10.0.0.5",
			NCID:      testNCID,
			NCVersion: 2,
		},
	}
	require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), ips))
	endpoint := sampleEndpoint(testIPAddress)
	endpoint.IfnameToIPMap[testIfName].IPv4 = append(
		endpoint.IfnameToIPMap[testIfName].IPv4,
		net.IPNet{IP: net.ParseIP("10.0.0.5"), Mask: net.CIDRMask(24, 32)},
	)
	require.NoError(t, db.AssignEndpoint(
		ctx,
		testAssignment(testIPID, "ip-2"),
		endpoint,
		testNow,
		testIntentTTL,
	))

	require.NoError(t, db.AssignEndpoint(
		ctx,
		testAssignment("ip-2"),
		sampleEndpoint("10.0.0.5"),
		testNow.Add(time.Minute),
		testIntentTTL,
	))

	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.NotContains(t, snapshot.IPOwners, testIPID)
	assert.Equal(t, testContainerID, snapshot.IPOwners["ip-2"])
	assert.Equal(t, []string{"ip-2"}, snapshot.Assignments[testContainerID].IPIDs)
}

func TestAssignEndpointRejectsInvalidReferences(t *testing.T) {
	tests := []struct {
		name       string
		assignment state.AssignmentRecord
		endpoint   state.EndpointRecord
		wantErr    error
	}{
		{
			name:       "missing IP",
			assignment: testAssignment("missing"),
			endpoint:   sampleEndpoint(testIPAddress),
			wantErr:    state.ErrNotFound,
		},
		{
			name:       "endpoint omits assigned IP",
			assignment: testAssignment(testIPID),
			endpoint:   sampleEndpoint("10.0.0.5"),
			wantErr:    state.ErrInconsistentState,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			db, _ := openTestDB(t)
			require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))

			err := db.AssignEndpoint(ctx, tt.assignment, tt.endpoint, testNow, testIntentTTL)
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestReleaseEndpointWithoutAssignmentPrunesExpiredIntents(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.Update(ctx, func(tx *state.WriteTx) error {
		if err := tx.PutDeleteIntent("expired", state.DeleteIntent{CreatedAt: testNow.Add(-testIntentTTL)}); err != nil {
			return err
		}
		return tx.PutDeleteIntent("active", state.DeleteIntent{CreatedAt: testNow})
	}))

	require.NoError(t, db.ReleaseEndpoint(
		ctx,
		"missing-pod",
		testContainerID,
		state.DeleteIntent{CreatedAt: testNow.Add(time.Minute)},
		testIntentTTL,
	))

	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.NotContains(t, snapshot.DeleteIntents, "expired")
	assert.Contains(t, snapshot.DeleteIntents, "active")
	assert.Contains(t, snapshot.DeleteIntents, testContainerID)
}

func TestPatchEndpointSuccess(t *testing.T) {
	tests := []struct {
		name  string
		setup func(context.Context, *state.DB)
		now   time.Time
	}{
		{
			name:  "without delete intent",
			setup: func(context.Context, *state.DB) {},
			now:   testNow,
		},
		{
			name: "after delete intent expires",
			setup: func(ctx context.Context, db *state.DB) {
				require.NoError(t, db.Update(ctx, func(tx *state.WriteTx) error {
					return tx.PutDeleteIntent(testContainerID, state.DeleteIntent{CreatedAt: testNow})
				}))
			},
			now: testNow.Add(testIntentTTL),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			db, _ := openTestDB(t)
			tt.setup(ctx, db)

			endpoint := sampleEndpoint(testIPAddress)
			require.NoError(t, db.PatchEndpoint(ctx, testContainerID, endpoint, tt.now, testIntentTTL))

			snapshot, err := db.Snapshot(ctx)
			require.NoError(t, err)
			assert.Equal(t, endpoint, snapshot.Endpoints[testContainerID])
			assert.NotContains(t, snapshot.DeleteIntents, testContainerID)
		})
	}
}

func TestConcurrentApplyBootHasOneEffectiveTransition(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.Update(ctx, func(tx *state.WriteTx) error {
		meta, err := tx.Metadata()
		if err != nil {
			return err
		}
		meta.BootID = "boot-old"
		return tx.PutMetadata(meta)
	}))
	before, err := db.Snapshot(ctx)
	require.NoError(t, err)

	const workers = 32
	type result struct {
		changed bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			changed, applyErr := db.ApplyBoot(ctx, "boot-new", state.BootPolicy{})
			results <- result{changed: changed, err: applyErr}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	changedCount := 0
	for result := range results {
		require.NoError(t, result.err)
		if result.changed {
			changedCount++
		}
	}
	assert.Equal(t, 1, changedCount)

	after, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, "boot-new", after.Metadata.BootID)
	assert.Equal(t, before.Metadata.Generation+1, after.Metadata.Generation)

	changed, err := db.ApplyBoot(ctx, "boot-new", state.BootPolicy{})
	require.NoError(t, err)
	assert.False(t, changed)
	unchanged, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, after.Metadata.Generation, unchanged.Metadata.Generation)
}

func TestSetManagedEndpointStateEnabledIsNoOp(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))
	require.NoError(t, db.AssignEndpoint(
		ctx,
		testAssignment(testIPID),
		sampleEndpoint(testIPAddress),
		testNow,
		testIntentTTL,
	))
	require.NoError(t, db.Update(ctx, func(tx *state.WriteTx) error {
		return tx.PutDeleteIntent("other", state.DeleteIntent{CreatedAt: testNow})
	}))
	before, err := db.Snapshot(ctx)
	require.NoError(t, err)

	require.NoError(t, db.SetManagedEndpointState(ctx, true))

	after, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestOperationValidation(t *testing.T) {
	tests := []struct {
		name    string
		run     func(context.Context, *state.DB) error
		wantErr error
	}{
		{
			name: "ApplyBoot empty boot ID",
			run: func(ctx context.Context, db *state.DB) error {
				_, err := db.ApplyBoot(ctx, "", state.BootPolicy{})
				return err
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "ApplyNetworkContainer empty NC ID",
			run: func(ctx context.Context, db *state.DB) error {
				record := sampleNCRecord()
				record.ID = ""
				return db.ApplyNetworkContainer(ctx, record, nil)
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "ApplyNetworkContainer empty IP ID",
			run: func(ctx context.Context, db *state.DB) error {
				return db.ApplyNetworkContainer(ctx, sampleNCRecord(), map[string]state.IPRecord{
					"": {ID: "", IPAddress: testIPAddress, NCID: testNCID},
				})
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "ApplyNetworkContainer mismatched IP ID",
			run: func(ctx context.Context, db *state.DB) error {
				return db.ApplyNetworkContainer(ctx, sampleNCRecord(), map[string]state.IPRecord{
					testIPID: {ID: "different", IPAddress: testIPAddress, NCID: testNCID},
				})
			},
			wantErr: state.ErrInconsistentState,
		},
		{
			name: "ApplyNetworkContainer wrong NC reference",
			run: func(ctx context.Context, db *state.DB) error {
				return db.ApplyNetworkContainer(ctx, sampleNCRecord(), map[string]state.IPRecord{
					testIPID: {ID: testIPID, IPAddress: testIPAddress, NCID: "different"},
				})
			},
			wantErr: state.ErrInconsistentState,
		},
		{
			name: "ApplyNetworkContainer invalid IP",
			run: func(ctx context.Context, db *state.DB) error {
				return db.ApplyNetworkContainer(ctx, sampleNCRecord(), map[string]state.IPRecord{
					testIPID: {ID: testIPID, IPAddress: "not-an-ip", NCID: testNCID},
				})
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "ApplyNetworkContainer invalid prefix",
			run: func(ctx context.Context, db *state.DB) error {
				record := sampleNCRecord()
				record.Request.IPConfiguration.IPSubnet.IPAddress = "10.0.0.0"
				record.Request.IPConfiguration.IPSubnet.PrefixLength = 64
				return db.ApplyNetworkContainer(ctx, record, sampleIPs())
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "ReplaceManagedEndpoints empty endpoint ID",
			run: func(ctx context.Context, db *state.DB) error {
				return db.ReplaceManagedEndpoints(ctx, map[string]state.EndpointRecord{
					"": sampleEndpoint(testIPAddress),
				})
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "ReplaceManagedEndpoints invalid prefix",
			run: func(ctx context.Context, db *state.DB) error {
				return db.ReplaceManagedEndpoints(ctx, map[string]state.EndpointRecord{
					testContainerID: invalidPrefixEndpoint(),
				})
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "ReplaceManagedEndpoints unknown IP",
			run: func(ctx context.Context, db *state.DB) error {
				return db.ReplaceManagedEndpoints(ctx, map[string]state.EndpointRecord{
					testContainerID: sampleEndpoint("10.0.0.99"),
				})
			},
			wantErr: state.ErrInconsistentState,
		},
		{
			name: "AssignEndpoint empty pod key",
			run: func(ctx context.Context, db *state.DB) error {
				assignment := testAssignment(testIPID)
				assignment.Pod.PodKey = ""
				return db.AssignEndpoint(ctx, assignment, sampleEndpoint(testIPAddress), testNow, testIntentTTL)
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "AssignEndpoint empty infra container ID",
			run: func(ctx context.Context, db *state.DB) error {
				assignment := testAssignment(testIPID)
				assignment.Pod.InfraContainerID = ""
				return db.AssignEndpoint(ctx, assignment, sampleEndpoint(testIPAddress), testNow, testIntentTTL)
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "AssignEndpoint no IP IDs",
			run: func(ctx context.Context, db *state.DB) error {
				return db.AssignEndpoint(ctx, testAssignment(), sampleEndpoint(testIPAddress), testNow, testIntentTTL)
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "AssignEndpoint empty IP ID",
			run: func(ctx context.Context, db *state.DB) error {
				return db.AssignEndpoint(ctx, testAssignment(""), sampleEndpoint(testIPAddress), testNow, testIntentTTL)
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "AssignEndpoint duplicate IP ID",
			run: func(ctx context.Context, db *state.DB) error {
				return db.AssignEndpoint(
					ctx,
					testAssignment(testIPID, testIPID),
					sampleEndpoint(testIPAddress),
					testNow,
					testIntentTTL,
				)
			},
			wantErr: state.ErrInconsistentState,
		},
		{
			name: "AssignEndpoint invalid prefix",
			run: func(ctx context.Context, db *state.DB) error {
				return db.AssignEndpoint(ctx, testAssignment(testIPID), invalidPrefixEndpoint(), testNow, testIntentTTL)
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "AssignEndpoint zero time",
			run: func(ctx context.Context, db *state.DB) error {
				return db.AssignEndpoint(ctx, testAssignment(testIPID), sampleEndpoint(testIPAddress), time.Time{}, testIntentTTL)
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "AssignEndpoint non-positive intent TTL",
			run: func(ctx context.Context, db *state.DB) error {
				return db.AssignEndpoint(ctx, testAssignment(testIPID), sampleEndpoint(testIPAddress), testNow, 0)
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "ReleaseEndpoint empty pod key",
			run: func(ctx context.Context, db *state.DB) error {
				return db.ReleaseEndpoint(ctx, "", testContainerID, state.DeleteIntent{CreatedAt: testNow}, testIntentTTL)
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "ReleaseEndpoint empty infra container ID",
			run: func(ctx context.Context, db *state.DB) error {
				return db.ReleaseEndpoint(ctx, testContainerID, "", state.DeleteIntent{CreatedAt: testNow}, testIntentTTL)
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "ReleaseEndpoint zero intent timestamp",
			run: func(ctx context.Context, db *state.DB) error {
				return db.ReleaseEndpoint(ctx, testContainerID, testContainerID, state.DeleteIntent{}, testIntentTTL)
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "ReleaseEndpoint non-positive intent TTL",
			run: func(ctx context.Context, db *state.DB) error {
				return db.ReleaseEndpoint(ctx, testContainerID, testContainerID, state.DeleteIntent{CreatedAt: testNow}, 0)
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "PatchEndpoint empty endpoint ID",
			run: func(ctx context.Context, db *state.DB) error {
				return db.PatchEndpoint(ctx, "", sampleEndpoint(testIPAddress), testNow, testIntentTTL)
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "PatchEndpoint invalid prefix",
			run: func(ctx context.Context, db *state.DB) error {
				return db.PatchEndpoint(ctx, testContainerID, invalidPrefixEndpoint(), testNow, testIntentTTL)
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "PatchEndpoint zero time",
			run: func(ctx context.Context, db *state.DB) error {
				return db.PatchEndpoint(ctx, testContainerID, sampleEndpoint(testIPAddress), time.Time{}, testIntentTTL)
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "PatchEndpoint non-positive intent TTL",
			run: func(ctx context.Context, db *state.DB) error {
				return db.PatchEndpoint(ctx, testContainerID, sampleEndpoint(testIPAddress), testNow, 0)
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "PruneDeleteIntents zero time",
			run: func(ctx context.Context, db *state.DB) error {
				return db.PruneDeleteIntents(ctx, time.Time{}, testIntentTTL)
			},
			wantErr: state.ErrInvalidInput,
		},
		{
			name: "PruneDeleteIntents non-positive TTL",
			run: func(ctx context.Context, db *state.DB) error {
				return db.PruneDeleteIntents(ctx, testNow, 0)
			},
			wantErr: state.ErrInvalidInput,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			db, _ := openTestDB(t)
			require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))

			err := tt.run(ctx, db)
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestOperationPersistenceFailures(t *testing.T) {
	ctx := context.Background()
	db, snapshot := openReadOnlyOperationDB(t)
	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "SetManagedEndpointState",
			run: func() error {
				return db.SetManagedEndpointState(ctx, false)
			},
		},
		{
			name: "ReplaceDurableState",
			run: func() error {
				return db.ReplaceDurableState(ctx, snapshot)
			},
		},
		{
			name: "ReplaceManagedEndpoints",
			run: func() error {
				return db.ReplaceManagedEndpoints(ctx, map[string]state.EndpointRecord{})
			},
		},
		{
			name: "ApplyBoot",
			run: func() error {
				_, err := db.ApplyBoot(ctx, "new-boot", state.BootPolicy{})
				return err
			},
		},
		{
			name: "ApplyNetworkContainer",
			run: func() error {
				return db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs())
			},
		},
		{
			name: "DeleteNetworkContainer",
			run: func() error {
				return db.DeleteNetworkContainer(ctx, testNCID)
			},
		},
		{
			name: "AssignEndpoint",
			run: func() error {
				return db.AssignEndpoint(
					ctx,
					testAssignment(testIPID),
					sampleEndpoint(testIPAddress),
					testNow,
					testIntentTTL,
				)
			},
		},
		{
			name: "ReleaseEndpoint",
			run: func() error {
				return db.ReleaseEndpoint(
					ctx,
					testContainerID,
					testContainerID,
					state.DeleteIntent{CreatedAt: testNow},
					testIntentTTL,
				)
			},
		},
		{
			name: "DeleteEndpointRecord",
			run: func() error {
				return db.DeleteEndpointRecord(ctx, testContainerID)
			},
		},
		{
			name: "PatchEndpoint",
			run: func() error {
				return db.PatchEndpoint(ctx, testContainerID, sampleEndpoint(testIPAddress), testNow, testIntentTTL)
			},
		},
		{
			name: "PruneDeleteIntents",
			run: func() error {
				return db.PruneDeleteIntents(ctx, testNow, testIntentTTL)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run()
			require.ErrorIs(t, err, bolterrors.ErrDatabaseReadOnly)
		})
	}
}

func TestOperationCorruptionAttribution(t *testing.T) {
	seedNC := func(ctx context.Context, db *state.DB) error {
		return db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs())
	}
	tests := []struct {
		name   string
		seed   func(context.Context, *state.DB) error
		bucket string
		key    string
		run    func(context.Context, *state.DB) error
	}{
		{
			name:   "ReplaceManagedEndpoints IP inventory",
			seed:   seedNC,
			bucket: "ips",
			key:    testIPID,
			run: func(ctx context.Context, db *state.DB) error {
				return db.ReplaceManagedEndpoints(ctx, nil)
			},
		},
		{
			name:   "ApplyBoot metadata",
			bucket: "metadata",
			key:    "service",
			run: func(ctx context.Context, db *state.DB) error {
				_, err := db.ApplyBoot(ctx, "boot-new", state.BootPolicy{})
				return err
			},
		},
		{
			name:   "ApplyBoot IP inventory",
			seed:   seedNC,
			bucket: "ips",
			key:    testIPID,
			run: func(ctx context.Context, db *state.DB) error {
				_, err := db.ApplyBoot(ctx, "boot-new", state.BootPolicy{})
				return err
			},
		},
		{
			name:   "ApplyBoot endpoints",
			seed:   seedNC,
			bucket: "endpoints",
			key:    testContainerID,
			run: func(ctx context.Context, db *state.DB) error {
				_, err := db.ApplyBoot(ctx, "boot-new", state.BootPolicy{})
				return err
			},
		},
		{
			name:   "ApplyBoot NC readiness",
			seed:   seedNC,
			bucket: "network_containers",
			key:    testNCID,
			run: func(ctx context.Context, db *state.DB) error {
				_, err := db.ApplyBoot(ctx, "boot-new", state.BootPolicy{ResetNetworkContainerReadiness: true})
				return err
			},
		},
		{
			name:   "ApplyNetworkContainer IP inventory",
			seed:   seedNC,
			bucket: "ips",
			key:    testIPID,
			run: func(ctx context.Context, db *state.DB) error {
				return db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs())
			},
		},
		{
			name:   "DeleteNetworkContainer IP inventory",
			seed:   seedNC,
			bucket: "ips",
			key:    testIPID,
			run: func(ctx context.Context, db *state.DB) error {
				return db.DeleteNetworkContainer(ctx, testNCID)
			},
		},
		{
			name:   "AssignEndpoint delete intent",
			seed:   seedNC,
			bucket: "delete_intents",
			key:    testContainerID,
			run: func(ctx context.Context, db *state.DB) error {
				return db.AssignEndpoint(
					ctx,
					testAssignment(testIPID),
					sampleEndpoint(testIPAddress),
					testNow,
					testIntentTTL,
				)
			},
		},
		{
			name:   "AssignEndpoint existing assignment",
			seed:   seedNC,
			bucket: "assignments",
			key:    testContainerID,
			run: func(ctx context.Context, db *state.DB) error {
				return db.AssignEndpoint(
					ctx,
					testAssignment(testIPID),
					sampleEndpoint(testIPAddress),
					testNow,
					testIntentTTL,
				)
			},
		},
		{
			name:   "AssignEndpoint IP",
			seed:   seedNC,
			bucket: "ips",
			key:    testIPID,
			run: func(ctx context.Context, db *state.DB) error {
				return db.AssignEndpoint(
					ctx,
					testAssignment(testIPID),
					sampleEndpoint(testIPAddress),
					testNow,
					testIntentTTL,
				)
			},
		},
		{
			name:   "ReleaseEndpoint delete intents",
			bucket: "delete_intents",
			key:    testContainerID,
			run: func(ctx context.Context, db *state.DB) error {
				return db.ReleaseEndpoint(
					ctx,
					testContainerID,
					testContainerID,
					state.DeleteIntent{CreatedAt: testNow},
					testIntentTTL,
				)
			},
		},
		{
			name:   "ReleaseEndpoint assignment",
			bucket: "assignments",
			key:    testContainerID,
			run: func(ctx context.Context, db *state.DB) error {
				return db.ReleaseEndpoint(
					ctx,
					testContainerID,
					testContainerID,
					state.DeleteIntent{CreatedAt: testNow},
					testIntentTTL,
				)
			},
		},
		{
			name:   "DeleteEndpointRecord assignments",
			bucket: "assignments",
			key:    testContainerID,
			run: func(ctx context.Context, db *state.DB) error {
				return db.DeleteEndpointRecord(ctx, testContainerID)
			},
		},
		{
			name:   "PatchEndpoint delete intent",
			bucket: "delete_intents",
			key:    testContainerID,
			run: func(ctx context.Context, db *state.DB) error {
				return db.PatchEndpoint(ctx, testContainerID, sampleEndpoint(testIPAddress), testNow, testIntentTTL)
			},
		},
		{
			name:   "PruneDeleteIntents records",
			bucket: "delete_intents",
			key:    testContainerID,
			run: func(ctx context.Context, db *state.DB) error {
				return db.PruneDeleteIntents(ctx, testNow, testIntentTTL)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openCorruptedOperationDB(t, tt.seed, tt.bucket, tt.key)
			err := tt.run(context.Background(), db)
			require.ErrorContains(t, err, fmt.Sprintf("bucket %q", tt.bucket))
			require.ErrorContains(t, err, tt.key)
		})
	}
}
