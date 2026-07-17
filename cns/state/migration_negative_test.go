// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//nolint:goconst // Repeated legacy keys make migration fixtures easier to read.
package state_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errHeldWrite = errors.New("held write released")

type observedImportState struct {
	metadata             state.Metadata
	migrationComplete    bool
	rollbackComplete     bool
	networkContainers    map[string]state.NetworkContainerRecord
	ips                  map[string]state.IPRecord
	networks             map[string]state.NetworkRecord
	orchestratorContexts map[string][]string
	pnpIDByMAC           map[string]string
	assignments          map[string]state.AssignmentRecord
	ipOwners             map[string]string
	endpoints            map[string]state.EndpointRecord
	deleteIntents        map[string]state.DeleteIntent
}

func TestImportLegacyMissingAndEmptyFiles(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, cnsPath, endpointPath string)
	}{
		{
			name: "missing CNS file",
			setup: func(t *testing.T, _, endpointPath string) {
				writeEnvelope(t, endpointPath, emptyLegacyEndpointEnvelope())
			},
		},
		{
			name: "empty CNS file",
			setup: func(t *testing.T, cnsPath, endpointPath string) {
				writeBytes(t, cnsPath, nil)
				writeEnvelope(t, endpointPath, emptyLegacyEndpointEnvelope())
			},
		},
		{
			name: "missing endpoint file",
			setup: func(t *testing.T, cnsPath, _ string) {
				writeEnvelope(t, cnsPath, emptyLegacyCNSEnvelope())
			},
		},
		{
			name: "empty endpoint file",
			setup: func(t *testing.T, cnsPath, endpointPath string) {
				writeEnvelope(t, cnsPath, emptyLegacyCNSEnvelope())
				writeBytes(t, endpointPath, nil)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cnsPath := filepath.Join(dir, "azure-cns.json")
			endpointPath := filepath.Join(dir, "azure-endpoints.json")
			tt.setup(t, cnsPath, endpointPath)

			db, _ := openTestDB(t)
			require.NoError(t, db.ImportLegacy(context.Background(), state.ImportOptions{
				CNSJSONPath:         cnsPath,
				EndpointJSONPath:    endpointPath,
				ManageEndpointState: true,
				BootID:              testBootID,
				Now:                 testNow,
			}))

			assertEmptyImportComplete(t, db)
		})
	}
}

func TestImportLegacyRejectsInvalidJSONWithoutMutation(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		contents string
	}{
		{name: "truncated CNS envelope", target: "cns", contents: `{"ContainerNetworkService":`},
		{name: "malformed CNS envelope", target: "cns", contents: `{"ContainerNetworkService":]}`},
		{name: "null CNS envelope", target: "cns", contents: `null`},
		{name: "wrong-type CNS envelope", target: "cns", contents: `[]`},
		{name: "null CNS state", target: "cns", contents: `{"ContainerNetworkService":null}`},
		{name: "wrong-type CNS state", target: "cns", contents: `{"ContainerNetworkService":[]}`},
		{name: "truncated endpoint envelope", target: "endpoint", contents: `{"Endpoints":`},
		{name: "malformed endpoint envelope", target: "endpoint", contents: `{"Endpoints":]}`},
		{name: "null endpoint envelope", target: "endpoint", contents: `null`},
		{name: "wrong-type endpoint envelope", target: "endpoint", contents: `[]`},
		{name: "null endpoint state", target: "endpoint", contents: `{"Endpoints":null}`},
		{name: "wrong-type endpoint state", target: "endpoint", contents: `{"Endpoints":[]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cnsPath := filepath.Join(dir, "azure-cns.json")
			endpointPath := filepath.Join(dir, "azure-endpoints.json")
			switch tt.target {
			case "cns":
				writeBytes(t, cnsPath, []byte(tt.contents))
				writeEnvelope(t, endpointPath, emptyLegacyEndpointEnvelope())
			case "endpoint":
				writeEnvelope(t, cnsPath, emptyLegacyCNSEnvelope())
				writeBytes(t, endpointPath, []byte(tt.contents))
			default:
				t.Fatalf("unknown target %q", tt.target)
			}

			db, _ := openTestDB(t)
			before := readImportState(t, db)
			err := db.ImportLegacy(context.Background(), state.ImportOptions{
				CNSJSONPath:         cnsPath,
				EndpointJSONPath:    endpointPath,
				ManageEndpointState: true,
				BootID:              testBootID,
				Now:                 testNow,
			})

			require.Error(t, err)
			assertImportStateUnchanged(t, db, before)
		})
	}
}

func TestImportLegacyReadPermissionFailuresDoNotMutateState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file permissions are required")
	}

	tests := []struct {
		name   string
		target string
	}{
		{name: "CNS file", target: "cns"},
		{name: "endpoint file", target: "endpoint"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cnsPath := filepath.Join(dir, "azure-cns.json")
			endpointPath := filepath.Join(dir, "azure-endpoints.json")
			writeEnvelope(t, cnsPath, emptyLegacyCNSEnvelope())
			writeEnvelope(t, endpointPath, emptyLegacyEndpointEnvelope())

			targetPath := cnsPath
			if tt.target == "endpoint" {
				targetPath = endpointPath
			}
			require.NoError(t, os.Chmod(targetPath, 0))
			t.Cleanup(func() {
				require.NoError(t, os.Chmod(targetPath, 0o600))
			})
			_, readErr := os.ReadFile(targetPath)
			if readErr == nil {
				t.Skip("test process can read mode 000 files")
			}
			require.ErrorIs(t, readErr, fs.ErrPermission)

			db, _ := openTestDB(t)
			before := readImportState(t, db)
			err := db.ImportLegacy(context.Background(), state.ImportOptions{
				CNSJSONPath:         cnsPath,
				EndpointJSONPath:    endpointPath,
				ManageEndpointState: true,
				BootID:              testBootID,
				Now:                 testNow,
			})

			require.ErrorIs(t, err, fs.ErrPermission)
			assertImportStateUnchanged(t, db, before)
		})
	}
}

func TestImportLegacySkipsNullEntries(t *testing.T) {
	dir := t.TempDir()
	cnsPath := filepath.Join(dir, "azure-cns.json")
	endpointPath := filepath.Join(dir, "azure-endpoints.json")
	cnsEnvelope := legacyCNSWithInventory(map[string]map[string]string{
		"nc-1": {"ip-1": "10.0.0.4"},
	})
	cnsState := cnsEnvelope["ContainerNetworkService"].(map[string]any)
	cnsState["Networks"] = map[string]any{
		"null-network": nil,
		"network-1": map[string]any{
			"NetworkName": "network-1",
		},
	}
	writeEnvelope(t, cnsPath, cnsEnvelope)
	writeEnvelope(t, endpointPath, map[string]any{
		"Endpoints": map[string]any{
			"null-endpoint": nil,
			"container-1": map[string]any{
				"PodName":      "pod-1",
				"PodNamespace": "default",
				"IfnameToIPMap": map[string]any{
					"null-ifname": nil,
					"eth0":        legacyIPInfo("10.0.0.4"),
				},
			},
		},
	})

	db, _ := openTestDB(t)
	require.NoError(t, db.ImportLegacy(context.Background(), state.ImportOptions{
		CNSJSONPath:         cnsPath,
		EndpointJSONPath:    endpointPath,
		ManageEndpointState: true,
		BootID:              testBootID,
		Now:                 testNow,
	}))

	observed := readImportState(t, db)
	is := assert.New(t)
	is.True(observed.migrationComplete)
	is.Equal(state.AuthorityBolt, observed.metadata.Authority)
	is.NotContains(observed.networks, "null-network")
	is.Contains(observed.networks, "network-1")
	is.NotContains(observed.endpoints, "null-endpoint")
	is.NotContains(observed.endpoints["container-1"].IfnameToIPMap, "null-ifname")
	is.Equal("container-1", observed.ipOwners["ip-1"])
	is.Equal([]string{"ip-1"}, observed.assignments["container-1"].IPIDs)
}

func TestImportLegacyRejectsDuplicateInventoryAndEndpointOwnership(t *testing.T) {
	tests := []struct {
		name      string
		cns       map[string]any
		endpoints map[string]any
	}{
		{
			name: "duplicate IP ID",
			cns: legacyCNSWithInventory(map[string]map[string]string{
				"nc-1": {"duplicate-id": "10.0.0.4"},
				"nc-2": {"duplicate-id": "10.0.0.5"},
			}),
			endpoints: emptyLegacyEndpointEnvelope(),
		},
		{
			name: "duplicate IP address",
			cns: legacyCNSWithInventory(map[string]map[string]string{
				"nc-1": {"ip-1": "10.0.0.4"},
				"nc-2": {"ip-2": "10.0.0.4"},
			}),
			endpoints: emptyLegacyEndpointEnvelope(),
		},
		{
			name: "conflicting endpoint ownership",
			cns: legacyCNSWithInventory(map[string]map[string]string{
				"nc-1": {"ip-1": "10.0.0.4"},
			}),
			endpoints: map[string]any{
				"Endpoints": map[string]any{
					"container-1": legacyEndpoint("pod-1", "10.0.0.4"),
					"container-2": legacyEndpoint("pod-2", "10.0.0.4"),
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cnsPath := filepath.Join(dir, "azure-cns.json")
			endpointPath := filepath.Join(dir, "azure-endpoints.json")
			writeEnvelope(t, cnsPath, tt.cns)
			writeEnvelope(t, endpointPath, tt.endpoints)

			db, _ := openTestDB(t)
			before := readImportState(t, db)
			err := db.ImportLegacy(context.Background(), state.ImportOptions{
				CNSJSONPath:         cnsPath,
				EndpointJSONPath:    endpointPath,
				ManageEndpointState: true,
				BootID:              testBootID,
				Now:                 testNow,
			})

			require.ErrorIs(t, err, state.ErrInconsistentState)
			assertImportStateUnchanged(t, db, before)
		})
	}
}

func TestImportLegacyRejectsMalformedOrchestratorContextLists(t *testing.T) {
	tests := []struct {
		name string
		list string
	}{
		{name: "leading separator", list: ",nc-1"},
		{name: "trailing separator", list: "nc-1,"},
		{name: "empty middle ID", list: "nc-1,,nc-2"},
		{name: "leading whitespace", list: " nc-1"},
		{name: "trailing whitespace", list: "nc-1 "},
		{name: "duplicate ID", list: "nc-1,nc-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cnsPath := filepath.Join(dir, "azure-cns.json")
			cnsEnvelope := legacyCNSWithInventory(map[string]map[string]string{
				"nc-1": {},
				"nc-2": {},
			})
			cnsState := cnsEnvelope["ContainerNetworkService"].(map[string]any)
			cnsState["ContainerIDByOrchestratorContext"] = map[string]any{
				"orchestrator-context": tt.list,
			}
			writeEnvelope(t, cnsPath, cnsEnvelope)

			db, _ := openTestDB(t)
			before := readImportState(t, db)
			err := db.ImportLegacy(context.Background(), state.ImportOptions{
				CNSJSONPath: cnsPath,
				BootID:      testBootID,
				Now:         testNow,
			})

			require.ErrorIs(t, err, state.ErrInconsistentState)
			assertImportStateUnchanged(t, db, before)
		})
	}
}

func TestImportLegacyDeleteIntents(t *testing.T) {
	dir := t.TempDir()
	cnsPath := filepath.Join(dir, "azure-cns.json")
	endpointPath := filepath.Join(dir, "azure-endpoints.json")
	writeEnvelope(t, cnsPath, emptyLegacyCNSEnvelope())
	writeEnvelope(t, endpointPath, map[string]any{
		"Endpoints": map[string]any{},
		"EndpointDeleteIntents": map[string]any{
			"zero":     map[string]any{},
			"null":     nil,
			"expired":  map[string]any{"createdAt": testNow.Add(-testIntentTTL - time.Nanosecond)},
			"boundary": map[string]any{"createdAt": testNow.Add(-testIntentTTL)},
			"active":   map[string]any{"createdAt": testNow.Add(-testIntentTTL + time.Nanosecond)},
		},
	})

	db, _ := openTestDB(t)
	require.NoError(t, db.ImportLegacy(context.Background(), state.ImportOptions{
		CNSJSONPath:      cnsPath,
		EndpointJSONPath: endpointPath,
		BootID:           testBootID,
		Now:              testNow,
		DeleteIntentTTL:  testIntentTTL,
	}))

	observed := readImportState(t, db)
	is := assert.New(t)
	is.True(observed.migrationComplete)
	is.Equal(state.AuthorityBolt, observed.metadata.Authority)
	is.Equal(uint64(1), observed.metadata.Generation)
	is.Equal(map[string]state.DeleteIntent{
		"active": {CreatedAt: testNow.Add(-testIntentTTL + time.Nanosecond)},
	}, observed.deleteIntents)
}

func TestImportLegacyRejectsMalformedDeleteIntentsWithoutMutation(t *testing.T) {
	tests := []struct {
		name    string
		intents any
	}{
		{name: "wrong-type intent map", intents: []any{}},
		{name: "wrong-type intent", intents: map[string]any{"container-1": "invalid"}},
		{name: "invalid timestamp", intents: map[string]any{"container-1": map[string]any{"createdAt": "not-a-time"}}},
		{name: "numeric timestamp", intents: map[string]any{"container-1": map[string]any{"createdAt": 1}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cnsPath := filepath.Join(dir, "azure-cns.json")
			endpointPath := filepath.Join(dir, "azure-endpoints.json")
			writeEnvelope(t, cnsPath, emptyLegacyCNSEnvelope())
			writeEnvelope(t, endpointPath, map[string]any{
				"Endpoints":             map[string]any{},
				"EndpointDeleteIntents": tt.intents,
			})

			db, _ := openTestDB(t)
			before := readImportState(t, db)
			err := db.ImportLegacy(context.Background(), state.ImportOptions{
				CNSJSONPath:      cnsPath,
				EndpointJSONPath: endpointPath,
				BootID:           testBootID,
				Now:              testNow,
				DeleteIntentTTL:  testIntentTTL,
			})

			require.Error(t, err)
			assertImportStateUnchanged(t, db, before)
		})
	}
}

func TestImportLegacyRejectsEveryNonEmptyBoltBucket(t *testing.T) {
	tests := []struct {
		name string
		seed func(t *testing.T, tx *state.WriteTx)
	}{
		{
			name: "network containers",
			seed: func(t *testing.T, tx *state.WriteTx) {
				require.NoError(t, tx.PutNetworkContainer(sampleNCRecord()))
			},
		},
		{
			name: "IPs",
			seed: func(t *testing.T, tx *state.WriteTx) {
				require.NoError(t, tx.PutIP(sampleIPs()[testIPID]))
			},
		},
		{
			name: "networks",
			seed: func(t *testing.T, tx *state.WriteTx) {
				require.NoError(t, tx.PutNetwork(state.NetworkRecord{NetworkName: "network-1"}))
			},
		},
		{
			name: "orchestrator contexts",
			seed: func(t *testing.T, tx *state.WriteTx) {
				require.NoError(t, tx.PutOrchestratorContext("context-1", []string{"nc-1"}))
			},
		},
		{
			name: "PnP IDs",
			seed: func(t *testing.T, tx *state.WriteTx) {
				require.NoError(t, tx.PutPnPIDByMAC("00:11:22:33:44:55", "pnp-1"))
			},
		},
		{
			name: "assignments",
			seed: func(t *testing.T, tx *state.WriteTx) {
				require.NoError(t, tx.PutAssignment(state.AssignmentRecord{
					Pod: state.PodIdentity{
						PodKey:           "pod-1",
						InfraContainerID: "container-1",
					},
				}))
			},
		},
		{
			name: "IP owners",
			seed: func(t *testing.T, tx *state.WriteTx) {
				require.NoError(t, tx.PutIPOwner("ip-1", "pod-1"))
			},
		},
		{
			name: "endpoints",
			seed: func(t *testing.T, tx *state.WriteTx) {
				require.NoError(t, tx.PutEndpoint("container-1", state.EndpointRecord{}))
			},
		},
		{
			name: "delete intents",
			seed: func(t *testing.T, tx *state.WriteTx) {
				require.NoError(t, tx.PutDeleteIntent("container-1", state.DeleteIntent{CreatedAt: testNow}))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cnsPath := filepath.Join(dir, "azure-cns.json")
			writeEnvelope(t, cnsPath, emptyLegacyCNSEnvelope())

			db, _ := openTestDB(t)
			require.NoError(t, db.Update(context.Background(), func(tx *state.WriteTx) error {
				tt.seed(t, tx)
				return nil
			}))
			before := readImportState(t, db)
			err := db.ImportLegacy(context.Background(), state.ImportOptions{
				CNSJSONPath: cnsPath,
				BootID:      testBootID,
				Now:         testNow,
			})

			require.ErrorIs(t, err, state.ErrInconsistentState)
			assertImportStateUnchanged(t, db, before)
		})
	}
}

func TestImportLegacyRejectsUnknownAuthorityWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	cnsPath := filepath.Join(dir, "azure-cns.json")
	writeEnvelope(t, cnsPath, emptyLegacyCNSEnvelope())

	db, _ := openTestDB(t)
	require.NoError(t, db.Update(context.Background(), func(tx *state.WriteTx) error {
		meta, err := tx.Metadata()
		require.NoError(t, err)
		meta.Authority = state.Authority("unknown")
		return tx.PutMetadata(meta)
	}))
	before := readImportState(t, db)

	err := db.ImportLegacy(context.Background(), state.ImportOptions{
		CNSJSONPath: cnsPath,
		BootID:      testBootID,
		Now:         testNow,
	})

	require.ErrorIs(t, err, state.ErrInconsistentState)
	assertImportStateUnchanged(t, db, before)
}

func TestImportLegacyWriteFailureRollsBackJSONAuthoritativeState(t *testing.T) {
	dir := t.TempDir()
	cnsPath := filepath.Join(dir, "azure-cns.json")
	writeEnvelope(t, cnsPath, map[string]any{
		"ContainerNetworkService": map[string]any{
			"ContainerStatus": map[string]any{
				"": map[string]any{
					"ID": "",
					"CreateNetworkContainerRequest": cns.CreateNetworkContainerRequest{
						NetworkContainerid: "",
					},
				},
			},
		},
	})

	db, _ := openTestDB(t)
	require.NoError(t, db.ApplyNetworkContainer(context.Background(), sampleNCRecord(), sampleIPs()))
	require.NoError(t, db.Update(context.Background(), func(tx *state.WriteTx) error {
		meta, err := tx.Metadata()
		require.NoError(t, err)
		meta.Authority = state.AuthorityJSON
		return tx.PutMetadata(meta)
	}))
	before := readImportState(t, db)

	err := db.ImportLegacy(context.Background(), state.ImportOptions{
		CNSJSONPath: cnsPath,
		BootID:      testBootID,
		Now:         testNow,
	})

	require.Error(t, err)
	assertImportStateUnchanged(t, db, before)
}

func TestImportLegacyConcurrentCallsCommitOnce(t *testing.T) {
	const callers = 8

	dir := t.TempDir()
	cnsPath := filepath.Join(dir, "azure-cns.json")
	writeEnvelope(t, cnsPath, legacyCNSWithInventory(map[string]map[string]string{
		"nc-1": {"ip-1": "10.0.0.4"},
	}))

	db, _ := openTestDB(t)
	ready := make(chan struct{}, callers)
	release := make(chan struct{})
	results := make(chan error, callers)
	for range callers {
		ctx := &barrierContext{
			Context: context.Background(),
			ready:   ready,
			release: release,
		}
		go func() {
			results <- db.ImportLegacy(ctx, state.ImportOptions{
				CNSJSONPath: cnsPath,
				BootID:      testBootID,
				Now:         testNow,
			})
		}()
	}

	timer := time.NewTimer(5 * time.Second)
	timedOut := false
	for range callers {
		select {
		case <-ready:
		case <-timer.C:
			timedOut = true
		}
		if timedOut {
			break
		}
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	close(release)

	for range callers {
		require.NoError(t, <-results)
	}
	require.False(t, timedOut, "concurrent imports did not reach the commit barrier")

	observed := readImportState(t, db)
	is := assert.New(t)
	is.True(observed.migrationComplete)
	is.Equal(state.AuthorityBolt, observed.metadata.Authority)
	is.Equal(uint64(1), observed.metadata.Generation)
	is.Contains(observed.networkContainers, "nc-1")
	is.Contains(observed.ips, "ip-1")
}

func TestImportLegacyContextCancellationDoesNotMutateState(t *testing.T) {
	tests := []struct {
		name    string
		context func() context.Context
	}{
		{
			name: "canceled before import",
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
		},
		{
			name: "canceled before update",
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				return &cancelOnSecondErrContext{
					Context: ctx,
					cancel:  cancel,
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cnsPath := filepath.Join(dir, "azure-cns.json")
			writeEnvelope(t, cnsPath, emptyLegacyCNSEnvelope())

			db, _ := openTestDB(t)
			before := readImportState(t, db)
			err := db.ImportLegacy(tt.context(), state.ImportOptions{
				CNSJSONPath: cnsPath,
				BootID:      testBootID,
				Now:         testNow,
			})

			require.ErrorIs(t, err, context.Canceled)
			assertImportStateUnchanged(t, db, before)
		})
	}
}

func TestImportLegacyLockWaitCancellationDoesNotMutateState(t *testing.T) {
	dir := t.TempDir()
	cnsPath := filepath.Join(dir, "azure-cns.json")
	writeEnvelope(t, cnsPath, emptyLegacyCNSEnvelope())

	db, _ := openTestDB(t)
	before := readImportState(t, db)
	writerStarted := make(chan struct{})
	releaseWriter := make(chan struct{})
	writerResult := make(chan error, 1)
	go func() {
		writerResult <- db.Update(context.Background(), func(_ *state.WriteTx) error {
			close(writerStarted)
			<-releaseWriter
			return errHeldWrite
		})
	}()
	<-writerStarted

	baseCtx, cancel := context.WithCancel(context.Background())
	updateReached := make(chan struct{})
	importCtx := &notifyOnSecondErrContext{
		Context: baseCtx,
		reached: updateReached,
	}
	importResult := make(chan error, 1)
	go func() {
		importResult <- db.ImportLegacy(importCtx, state.ImportOptions{
			CNSJSONPath: cnsPath,
			BootID:      testBootID,
			Now:         testNow,
		})
	}()
	<-updateReached
	cancel()

	timer := time.NewTimer(2 * time.Second)
	var importErr error
	select {
	case importErr = <-importResult:
		if !timer.Stop() {
			<-timer.C
		}
	case <-timer.C:
		close(releaseWriter)
		require.ErrorIs(t, <-writerResult, errHeldWrite)
		importErr = <-importResult
		t.Fatalf("import did not stop while waiting for the write lock: %v", importErr)
	}

	require.ErrorIs(t, importErr, context.Canceled)
	close(releaseWriter)
	require.ErrorIs(t, <-writerResult, errHeldWrite)
	assertImportStateUnchanged(t, db, before)
}

type barrierContext struct {
	context.Context
	calls   atomic.Int32
	ready   chan<- struct{}
	release <-chan struct{}
}

func (c *barrierContext) Err() error {
	if c.calls.Add(1) == 2 {
		c.ready <- struct{}{}
		<-c.release
	}
	return c.Context.Err() //nolint:wrapcheck // A Context implementation must return the underlying cancellation sentinel.
}

type cancelOnSecondErrContext struct {
	context.Context
	calls  atomic.Int32
	cancel context.CancelFunc
}

func (c *cancelOnSecondErrContext) Err() error {
	if c.calls.Add(1) == 2 {
		c.cancel()
	}
	return c.Context.Err() //nolint:wrapcheck // A Context implementation must return the underlying cancellation sentinel.
}

type notifyOnSecondErrContext struct {
	context.Context
	calls   atomic.Int32
	reached chan<- struct{}
}

func (c *notifyOnSecondErrContext) Err() error {
	if c.calls.Add(1) == 2 {
		c.reached <- struct{}{}
	}
	return c.Context.Err() //nolint:wrapcheck // A Context implementation must return the underlying cancellation sentinel.
}

func readImportState(t *testing.T, db *state.DB) observedImportState {
	t.Helper()
	var observed observedImportState
	err := db.View(context.Background(), func(tx *state.ReadTx) error {
		var err error
		observed.metadata, err = tx.Metadata()
		if err != nil {
			return fmt.Errorf("reading metadata: %w", err)
		}
		observed.migrationComplete = tx.MigrationComplete()
		observed.rollbackComplete = tx.RollbackComplete()
		if observed.networkContainers, err = tx.NetworkContainers(); err != nil {
			return fmt.Errorf("reading network containers: %w", err)
		}
		if observed.ips, err = tx.IPs(); err != nil {
			return fmt.Errorf("reading IPs: %w", err)
		}
		if observed.networks, err = tx.Networks(); err != nil {
			return fmt.Errorf("reading networks: %w", err)
		}
		if observed.orchestratorContexts, err = tx.OrchestratorContexts(); err != nil {
			return fmt.Errorf("reading orchestrator contexts: %w", err)
		}
		if observed.pnpIDByMAC, err = tx.PnPIDByMAC(); err != nil {
			return fmt.Errorf("reading PnP IDs: %w", err)
		}
		if observed.assignments, err = tx.Assignments(); err != nil {
			return fmt.Errorf("reading assignments: %w", err)
		}
		if observed.ipOwners, err = tx.IPOwners(); err != nil {
			return fmt.Errorf("reading IP owners: %w", err)
		}
		if observed.endpoints, err = tx.Endpoints(); err != nil {
			return fmt.Errorf("reading endpoints: %w", err)
		}
		observed.deleteIntents, err = tx.DeleteIntents()
		if err != nil {
			return fmt.Errorf("reading delete intents: %w", err)
		}
		return nil
	})
	require.NoError(t, err)
	return observed
}

func assertImportStateUnchanged(t *testing.T, db *state.DB, before observedImportState) {
	t.Helper()
	after := readImportState(t, db)
	is := assert.New(t)
	is.Equal(before.migrationComplete, after.migrationComplete, "migration marker changed")
	is.Equal(before.rollbackComplete, after.rollbackComplete, "rollback marker changed")
	is.Equal(before.metadata.Authority, after.metadata.Authority, "authority changed")
	is.Equal(before.metadata.Generation, after.metadata.Generation, "generation changed")
	is.Equal(before.metadata, after.metadata, "metadata bucket changed")
	is.Equal(before.networkContainers, after.networkContainers, "network container bucket changed")
	is.Equal(before.ips, after.ips, "IP bucket changed")
	is.Equal(before.networks, after.networks, "network bucket changed")
	is.Equal(before.orchestratorContexts, after.orchestratorContexts, "orchestrator context bucket changed")
	is.Equal(before.pnpIDByMAC, after.pnpIDByMAC, "PnP bucket changed")
	is.Equal(before.assignments, after.assignments, "assignment bucket changed")
	is.Equal(before.ipOwners, after.ipOwners, "IP owner bucket changed")
	is.Equal(before.endpoints, after.endpoints, "endpoint bucket changed")
	is.Equal(before.deleteIntents, after.deleteIntents, "delete intent bucket changed")
}

func assertEmptyImportComplete(t *testing.T, db *state.DB) {
	t.Helper()
	observed := readImportState(t, db)
	is := assert.New(t)
	is.True(observed.migrationComplete)
	is.False(observed.rollbackComplete)
	is.Equal(state.AuthorityBolt, observed.metadata.Authority)
	is.Equal(uint64(1), observed.metadata.Generation)
	is.Empty(observed.networkContainers)
	is.Empty(observed.ips)
	is.Empty(observed.networks)
	is.Empty(observed.orchestratorContexts)
	is.Empty(observed.pnpIDByMAC)
	is.Empty(observed.assignments)
	is.Empty(observed.ipOwners)
	is.Empty(observed.endpoints)
	is.Empty(observed.deleteIntents)
}

func emptyLegacyCNSEnvelope() map[string]any {
	return map[string]any{"ContainerNetworkService": map[string]any{}}
}

func emptyLegacyEndpointEnvelope() map[string]any {
	return map[string]any{"Endpoints": map[string]any{}}
}

func legacyCNSWithInventory(inventory map[string]map[string]string) map[string]any {
	containerStatus := make(map[string]any, len(inventory))
	for ncID, ips := range inventory {
		secondaryIPs := make(map[string]cns.SecondaryIPConfig, len(ips))
		for ipID, address := range ips {
			secondaryIPs[ipID] = cns.SecondaryIPConfig{
				IPAddress: address,
			}
		}
		containerStatus[ncID] = map[string]any{
			"ID": ncID,
			"CreateNetworkContainerRequest": cns.CreateNetworkContainerRequest{
				NetworkContainerid: ncID,
				SecondaryIPConfigs: secondaryIPs,
			},
		}
	}
	return map[string]any{
		"ContainerNetworkService": map[string]any{
			"ContainerStatus": containerStatus,
		},
	}
}

func legacyEndpoint(podName, address string) map[string]any {
	return map[string]any{
		"PodName":      podName,
		"PodNamespace": "default",
		"IfnameToIPMap": map[string]any{
			"eth0": legacyIPInfo(address),
		},
	}
}

func legacyIPInfo(address string) map[string]any {
	return map[string]any{
		"IPv4": []net.IPNet{{
			IP:   net.ParseIP(address),
			Mask: net.CIDRMask(24, 32),
		}},
	}
}

func writeBytes(t *testing.T, path string, contents []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, contents, 0o600))
}
