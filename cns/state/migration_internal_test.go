// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//nolint:goconst // Repeated values keep state fixtures readable.
package state

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

func openInternalTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"), Options{})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	return db
}

func TestLegacyNCListIDs(t *testing.T) {
	tests := []struct {
		name string
		list legacyNCList
		want []string
	}{
		{name: "empty", list: "", want: nil},
		{name: "single", list: "nc-1", want: []string{"nc-1"}},
		{name: "multiple", list: "nc-1,nc-2", want: []string{"nc-1", "nc-2"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.list.IDs()
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReadLegacyEnvelopeVariants(t *testing.T) {
	t.Run("empty path", func(t *testing.T) {
		envelope, exists, err := readLegacyEnvelope("")
		require.NoError(t, err)
		assert.False(t, exists)
		assert.Nil(t, envelope)
	})

	t.Run("missing file", func(t *testing.T) {
		envelope, exists, err := readLegacyEnvelope(filepath.Join(t.TempDir(), "missing.json"))
		require.NoError(t, err)
		assert.False(t, exists)
		assert.Nil(t, envelope)
	})

	t.Run("empty file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.json")
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		envelope, exists, err := readLegacyEnvelope(path)
		require.NoError(t, err)
		assert.False(t, exists)
		assert.Nil(t, envelope)
	})

	t.Run("invalid JSON", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "invalid.json")
		require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
		_, _, err := readLegacyEnvelope(path)
		require.Error(t, err)
	})

	t.Run("valid envelope", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "valid.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"key":{"value":1}}`), 0o600))
		envelope, exists, err := readLegacyEnvelope(path)
		require.NoError(t, err)
		assert.True(t, exists)
		assert.Contains(t, envelope, "key")
	})
}

func TestAddLegacyCNSStatePreservesCollections(t *testing.T) {
	snapshot := NewSnapshot()
	legacy := legacyCNSState{
		Location:         "location",
		NetworkType:      "network-type",
		OrchestratorType: "orchestrator",
		NodeID:           "node",
		Initialized:      true,
		ContainerIDByOrchestratorContext: map[string]legacyNCList{
			"context": "nc-1,nc-2",
		},
		ContainerStatus: map[string]legacyContainerStatus{
			"nc-1": {
				CreateNetworkContainerRequest: cns.CreateNetworkContainerRequest{
					SecondaryIPConfigs: map[string]cns.SecondaryIPConfig{
						"ip-1": {IPAddress: "10.0.0.4", NCVersion: 2},
					},
				},
			},
		},
		Networks: map[string]*legacyNetworkInfo{
			"nil": nil,
			"network-1": {
				NetworkName: "network-1",
				Options:     map[string]any{"key": "value"},
			},
		},
		PnpIDByMacAddress: map[string]string{
			"00:11:22:33:44:55": "pnp-1",
		},
	}

	require.NoError(t, addLegacyCNSState(&snapshot, legacy))
	assert.Equal(t, "location", snapshot.Metadata.Location)
	assert.Equal(t, []string{"nc-1", "nc-2"}, snapshot.OrchestratorContexts["context"])
	assert.Equal(t, "pnp-1", snapshot.PnPIDByMAC["00:11:22:33:44:55"])
	assert.NotContains(t, snapshot.Networks, "nil")
	assert.Contains(t, snapshot.Networks, "network-1")
	assert.Equal(t, "nc-1", snapshot.NetworkContainers["nc-1"].ID)
	assert.Equal(t, "nc-1", snapshot.IPs["ip-1"].NCID)
}

func TestAddLegacyEndpointsSkipsNilRecords(t *testing.T) {
	snapshot := NewSnapshot()
	addLegacyEndpoints(&snapshot, map[string]*legacyEndpointInfo{
		"nil-endpoint": nil,
		"container-1": {
			PodName:      "pod",
			PodNamespace: "namespace",
			IfnameToIPMap: map[string]*legacyIPInfo{
				"nil": nil,
				"eth0": {
					IPv4: []net.IPNet{{
						IP:   net.ParseIP("10.0.0.4"),
						Mask: net.CIDRMask(24, 32),
					}},
				},
			},
		},
	})

	assert.NotContains(t, snapshot.Endpoints, "nil-endpoint")
	endpoint := snapshot.Endpoints["container-1"]
	assert.NotContains(t, endpoint.IfnameToIPMap, "nil")
	assert.Contains(t, endpoint.IfnameToIPMap, "eth0")
}

func TestAddAssignmentsFromEndpointsValidation(t *testing.T) {
	t.Run("duplicate inventory address", func(t *testing.T) {
		snapshot := NewSnapshot()
		snapshot.IPs["ip-1"] = IPRecord{ID: "ip-1", IPAddress: "10.0.0.4"}
		snapshot.IPs["ip-2"] = IPRecord{ID: "ip-2", IPAddress: "10.0.0.4"}
		err := addAssignmentsFromEndpoints(&snapshot)
		require.ErrorIs(t, err, ErrInconsistentState)
	})

	t.Run("endpoint IP outside inventory", func(t *testing.T) {
		snapshot := NewSnapshot()
		snapshot.Endpoints["container-1"] = EndpointRecord{
			IfnameToIPMap: map[string]*IPInfoRecord{
				"eth0": {
					IPv4: []net.IPNet{{
						IP:   net.ParseIP("10.0.0.4"),
						Mask: net.CIDRMask(24, 32),
					}},
				},
			},
		}
		err := addAssignmentsFromEndpoints(&snapshot)
		require.ErrorIs(t, err, ErrInconsistentState)
	})

	t.Run("multiple endpoint owners", func(t *testing.T) {
		snapshot := NewSnapshot()
		snapshot.IPs["ip-1"] = IPRecord{ID: "ip-1", IPAddress: "10.0.0.4"}
		endpoint := EndpointRecord{
			IfnameToIPMap: map[string]*IPInfoRecord{
				"eth0": {
					IPv4: []net.IPNet{{
						IP:   net.ParseIP("10.0.0.4"),
						Mask: net.CIDRMask(24, 32),
					}},
				},
			},
		}
		snapshot.Endpoints["container-1"] = endpoint
		snapshot.Endpoints["container-2"] = endpoint
		err := addAssignmentsFromEndpoints(&snapshot)
		require.ErrorIs(t, err, ErrInconsistentState)
	})

	t.Run("skips nil and non-infra interfaces", func(t *testing.T) {
		snapshot := NewSnapshot()
		snapshot.Endpoints["container-1"] = EndpointRecord{
			IfnameToIPMap: map[string]*IPInfoRecord{
				"nil": nil,
				"backend": {
					NICType: cns.BackendNIC,
					IPv4: []net.IPNet{{
						IP:   net.ParseIP("10.0.0.4"),
						Mask: net.CIDRMask(24, 32),
					}},
				},
			},
		}
		require.NoError(t, addAssignmentsFromEndpoints(&snapshot))
		assert.Empty(t, snapshot.Assignments)
		assert.Empty(t, snapshot.IPOwners)
	})
}

func TestTxStateEmptyChecksEachCollection(t *testing.T) {
	tests := []struct {
		name string
		put  func(*WriteTx) error
	}{
		{
			name: "network container",
			put: func(tx *WriteTx) error {
				return tx.PutNetworkContainer(NetworkContainerRecord{ID: "nc-1"})
			},
		},
		{
			name: "IP",
			put: func(tx *WriteTx) error {
				return tx.PutIP(IPRecord{ID: "ip-1"})
			},
		},
		{
			name: "network",
			put: func(tx *WriteTx) error {
				return tx.PutNetwork(NetworkRecord{NetworkName: "network-1"})
			},
		},
		{
			name: "endpoint",
			put: func(tx *WriteTx) error {
				return tx.PutEndpoint("container-1", EndpointRecord{})
			},
		},
		{
			name: "assignment",
			put: func(tx *WriteTx) error {
				return tx.PutAssignment(AssignmentRecord{Pod: PodIdentity{PodKey: "pod-1"}})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			db := openInternalTestDB(t)
			require.NoError(t, db.Update(ctx, tt.put))
			require.NoError(t, db.View(ctx, func(tx *ReadTx) error {
				empty, err := txStateEmpty(tx)
				require.NoError(t, err)
				assert.False(t, empty)
				return nil
			}))
		})
	}

	t.Run("empty", func(t *testing.T) {
		db := openInternalTestDB(t)
		require.NoError(t, db.View(context.Background(), func(tx *ReadTx) error {
			empty, err := txStateEmpty(tx)
			require.NoError(t, err)
			assert.True(t, empty)
			return nil
		}))
	})
}

func TestLegacyEnvelopeEncodingFailures(t *testing.T) {
	snapshot := NewSnapshot()
	snapshot.Networks["network-1"] = NetworkRecord{
		NetworkName: "network-1",
		Options:     map[string]any{"unsupported": make(chan int)},
	}
	_, _, err := legacyEnvelopes(snapshot)
	require.ErrorContains(t, err, "encoding legacy CNS state")
}

func TestAtomicWriteJSONBoundaryFailures(t *testing.T) {
	fileSystem := osRollbackFileSystem{}
	require.NoError(t, atomicWriteJSON(fileSystem, "", map[string]string{"key": "value"}))

	err := atomicWriteJSON(fileSystem, filepath.Join(t.TempDir(), "invalid.json"), make(chan int))
	require.ErrorContains(t, err, "encoding legacy state")

	parentFile := filepath.Join(t.TempDir(), "parent")
	require.NoError(t, os.WriteFile(parentFile, []byte("file"), 0o600))
	err = atomicWriteJSON(fileSystem, filepath.Join(parentFile, "state.json"), map[string]string{"key": "value"})
	require.ErrorContains(t, err, "creating legacy state directory")
}

func TestWriteSnapshotPersistenceFailures(t *testing.T) {
	t.Run("metadata", func(t *testing.T) {
		db := openInternalTestDB(t)
		require.NoError(t, db.db.View(func(rawTx *bolt.Tx) error {
			err := writeSnapshot(&WriteTx{ReadTx: ReadTx{tx: rawTx}}, NewSnapshot())
			require.ErrorIs(t, err, bolterrors.ErrTxNotWritable)
			return nil
		}))
	})

	tests := []struct {
		name     string
		snapshot func() Snapshot
	}{
		{
			name: "network container",
			snapshot: func() Snapshot {
				snapshot := NewSnapshot()
				snapshot.NetworkContainers["invalid"] = NetworkContainerRecord{}
				return snapshot
			},
		},
		{
			name: "IP",
			snapshot: func() Snapshot {
				snapshot := NewSnapshot()
				snapshot.IPs["invalid"] = IPRecord{}
				return snapshot
			},
		},
		{
			name: "network",
			snapshot: func() Snapshot {
				snapshot := NewSnapshot()
				snapshot.Networks["invalid"] = NetworkRecord{}
				return snapshot
			},
		},
		{
			name: "orchestrator context",
			snapshot: func() Snapshot {
				snapshot := NewSnapshot()
				snapshot.OrchestratorContexts[""] = []string{"nc-1"}
				return snapshot
			},
		},
		{
			name: "PnP ID",
			snapshot: func() Snapshot {
				snapshot := NewSnapshot()
				snapshot.PnPIDByMAC[""] = "pnp-1"
				return snapshot
			},
		},
		{
			name: "assignment",
			snapshot: func() Snapshot {
				snapshot := NewSnapshot()
				snapshot.Assignments["invalid"] = AssignmentRecord{}
				return snapshot
			},
		},
		{
			name: "IP owner",
			snapshot: func() Snapshot {
				snapshot := NewSnapshot()
				snapshot.IPOwners[""] = "pod-1"
				return snapshot
			},
		},
		{
			name: "endpoint",
			snapshot: func() Snapshot {
				snapshot := NewSnapshot()
				snapshot.Endpoints[""] = EndpointRecord{}
				return snapshot
			},
		},
		{
			name: "delete intent",
			snapshot: func() Snapshot {
				snapshot := NewSnapshot()
				snapshot.DeleteIntents[""] = DeleteIntent{}
				return snapshot
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openInternalTestDB(t)
			err := db.Update(context.Background(), func(tx *WriteTx) error {
				return writeSnapshot(tx, tt.snapshot())
			})
			require.ErrorIs(t, err, bolterrors.ErrKeyRequired)
		})
	}
}
