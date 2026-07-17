// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//nolint:goconst // Repeated values make state-machine fixtures easier to read.
package state_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-container-networking/cns/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testNow       = time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	testIntentTTL = 24 * time.Hour
)

const (
	testContainerID = "container-1"
	testPodName     = "pod-1"
	testNamespace   = "default"
	testIfName      = "eth0"
	testBootID      = "boot-1"
)

func sampleEndpoint(ip string) state.EndpointRecord {
	return state.EndpointRecord{
		PodName:      testPodName,
		PodNamespace: testNamespace,
		IfnameToIPMap: map[string]*state.IPInfoRecord{
			testIfName: {
				IPv4: []net.IPNet{{
					IP:   net.ParseIP(ip),
					Mask: net.CIDRMask(24, 32),
				}},
			},
		},
	}
}

func TestAssignAndReleaseEndpointAreAtomic(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))

	assignment := state.AssignmentRecord{
		Pod: state.PodIdentity{
			PodKey:           testContainerID,
			InfraContainerID: testContainerID,
			PodName:          testPodName,
			PodNamespace:     testNamespace,
		},
		IPIDs: []string{testIPID},
	}
	require.NoError(t, db.AssignEndpoint(ctx, assignment, sampleEndpoint(testIPAddress), testNow, testIntentTTL))

	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, testContainerID, snapshot.IPOwners[testIPID])
	assert.Contains(t, snapshot.Assignments, testContainerID)
	assert.Contains(t, snapshot.Endpoints, testContainerID)

	require.NoError(t, db.ReleaseEndpoint(ctx, testContainerID, testContainerID, state.DeleteIntent{CreatedAt: testNow}, testIntentTTL))
	snapshot, err = db.Snapshot(ctx)
	require.NoError(t, err)
	assert.NotContains(t, snapshot.IPOwners, testIPID)
	assert.NotContains(t, snapshot.Assignments, testContainerID)
	assert.NotContains(t, snapshot.Endpoints, testContainerID)
	assert.Contains(t, snapshot.DeleteIntents, testContainerID)
}

func TestDeleteIntentBlocksLateAddAndPatch(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))
	require.NoError(t, db.ReleaseEndpoint(ctx, "container-1", "container-1", state.DeleteIntent{CreatedAt: testNow}, testIntentTTL))

	assignment := state.AssignmentRecord{
		Pod:   state.PodIdentity{PodKey: "container-1", InfraContainerID: "container-1"},
		IPIDs: []string{"ip-uuid-1"},
	}
	err := db.AssignEndpoint(ctx, assignment, sampleEndpoint("10.0.0.4"), testNow.Add(time.Minute), testIntentTTL)
	require.ErrorIs(t, err, state.ErrDeleteIntent)

	err = db.PatchEndpoint(ctx, "container-1", sampleEndpoint("10.0.0.4"), testNow.Add(time.Minute), testIntentTTL)
	require.ErrorIs(t, err, state.ErrDeleteIntent)
}

func TestExpiredDeleteIntentDoesNotBlockAdd(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))
	require.NoError(t, db.ReleaseEndpoint(ctx, "container-1", "container-1", state.DeleteIntent{CreatedAt: testNow}, testIntentTTL))

	assignment := state.AssignmentRecord{
		Pod:   state.PodIdentity{PodKey: "container-1", InfraContainerID: "container-1"},
		IPIDs: []string{"ip-uuid-1"},
	}
	require.NoError(t, db.AssignEndpoint(ctx, assignment, sampleEndpoint("10.0.0.4"), testNow.Add(testIntentTTL), testIntentTTL))

	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.NotContains(t, snapshot.DeleteIntents, "container-1")
	assert.Equal(t, "container-1", snapshot.IPOwners["ip-uuid-1"])
}

func TestBootChangeClearsSessionStateOnly(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))

	assignment := state.AssignmentRecord{
		Pod:   state.PodIdentity{PodKey: "container-1", InfraContainerID: "container-1"},
		IPIDs: []string{"ip-uuid-1"},
	}
	require.NoError(t, db.AssignEndpoint(ctx, assignment, sampleEndpoint("10.0.0.4"), testNow, testIntentTTL))
	require.NoError(t, db.Update(ctx, func(tx *state.WriteTx) error {
		meta, err := tx.Metadata()
		if err != nil {
			return fmt.Errorf("reading metadata: %w", err)
		}
		meta.BootID = "boot-1"
		return tx.PutMetadata(meta)
	}))

	changed, err := db.ApplyBoot(ctx, "boot-2", state.BootPolicy{
		ClearEndpoints:                 true,
		ResetNetworkContainerReadiness: true,
	})
	require.NoError(t, err)
	assert.True(t, changed)

	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Contains(t, snapshot.NetworkContainers, "nc-1")
	assert.Contains(t, snapshot.IPs, "ip-uuid-1")
	assert.Empty(t, snapshot.Assignments)
	assert.Empty(t, snapshot.IPOwners)
	assert.Empty(t, snapshot.Endpoints)
	assert.Empty(t, snapshot.DeleteIntents)
	assert.Equal(t, "-1", snapshot.NetworkContainers["nc-1"].HostVersion)
	assert.False(t, snapshot.NetworkContainers["nc-1"].VFPUpdateComplete)
}

func TestBootChangeCanRetainEndpointOwnershipForPlatformReconciliation(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))
	assignment := state.AssignmentRecord{
		Pod:   state.PodIdentity{PodKey: "container-1", InfraContainerID: "container-1"},
		IPIDs: []string{"ip-uuid-1"},
	}
	require.NoError(t, db.AssignEndpoint(ctx, assignment, sampleEndpoint("10.0.0.4"), testNow, testIntentTTL))
	require.NoError(t, db.Update(ctx, func(tx *state.WriteTx) error {
		meta, err := tx.Metadata()
		if err != nil {
			return fmt.Errorf("reading metadata: %w", err)
		}
		meta.BootID = "boot-1"
		return tx.PutMetadata(meta)
	}))

	changed, err := db.ApplyBoot(ctx, "boot-2", state.BootPolicy{ClearEndpoints: false})
	require.NoError(t, err)
	assert.True(t, changed)

	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Contains(t, snapshot.Endpoints, "container-1")
	assert.Equal(t, "container-1", snapshot.IPOwners["ip-uuid-1"])
	assert.Equal(t, []string{"ip-uuid-1"}, snapshot.Assignments["container-1"].IPIDs)
	assert.Empty(t, snapshot.DeleteIntents)
}

func TestEndpointMigrationBeforeNCReconcileBuildsAssignmentsLater(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.ReplaceManagedEndpoints(ctx, map[string]state.EndpointRecord{
		"container-1": sampleEndpoint("10.0.0.4"),
	}))

	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Contains(t, snapshot.Endpoints, "container-1")
	assert.Empty(t, snapshot.Assignments)

	durable := state.NewSnapshot()
	durable.Metadata = snapshot.Metadata
	durable.NetworkContainers["nc-1"] = sampleNCRecord()
	durable.IPs["ip-uuid-1"] = sampleIPs()["ip-uuid-1"]
	require.NoError(t, db.ReplaceDurableState(ctx, durable))

	snapshot, err = db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"ip-uuid-1"}, snapshot.Assignments["container-1"].IPIDs)
	assert.Equal(t, "container-1", snapshot.IPOwners["ip-uuid-1"])
}

func TestDisablingManagedEndpointStateClearsCompetingAuthority(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))
	require.NoError(t, db.AssignEndpoint(ctx, state.AssignmentRecord{
		Pod:   state.PodIdentity{PodKey: "container-1", InfraContainerID: "container-1"},
		IPIDs: []string{"ip-uuid-1"},
	}, sampleEndpoint("10.0.0.4"), testNow, testIntentTTL))

	require.NoError(t, db.SetManagedEndpointState(ctx, false))
	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Empty(t, snapshot.Assignments)
	assert.Empty(t, snapshot.IPOwners)
	assert.Empty(t, snapshot.Endpoints)
	assert.Empty(t, snapshot.DeleteIntents)
	assert.Contains(t, snapshot.IPs, "ip-uuid-1")
}

func TestConcurrentAssignmentsCannotOwnSameIP(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))

	var (
		wg   sync.WaitGroup
		errs = make(chan error, 2)
	)
	for i := 0; i < 2; i++ {
		containerID := fmt.Sprintf("container-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- db.AssignEndpoint(ctx, state.AssignmentRecord{
				Pod:   state.PodIdentity{PodKey: containerID, InfraContainerID: containerID},
				IPIDs: []string{"ip-uuid-1"},
			}, sampleEndpoint("10.0.0.4"), testNow, testIntentTTL)
		}()
	}
	wg.Wait()
	close(errs)

	successes := 0
	conflicts := 0
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, state.ErrIPAlreadyAssigned):
			conflicts++
		default:
			t.Fatalf("unexpected assignment error: %v", err)
		}
	}
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, conflicts)
	_, err := db.Snapshot(ctx)
	require.NoError(t, err)
}
