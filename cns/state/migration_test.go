// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//nolint:goconst // Repeated legacy keys make migration fixtures easier to read.
package state_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/state"
	nncv1alpha "github.com/Azure/azure-container-networking/crd/nodenetworkconfig/api/v1alpha"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportLegacyPreservesUUIDsFieldsAndAssignments(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cnsPath := filepath.Join(dir, "azure-cns.json")
	endpointPath := filepath.Join(dir, "azure-endpoints.json")

	request := cns.CreateNetworkContainerRequest{
		NetworkContainerid: "nc-1",
		Version:            "4",
		AuthorizationToken: "secret",
		IPv6Configuration: cns.IPConfiguration{
			IPSubnet: cns.IPSubnet{IPAddress: "fd00::", PrefixLength: 120},
		},
		SecondaryIPConfigs: map[string]cns.SecondaryIPConfig{
			"dynamic-ip-uuid": {IPAddress: "10.0.0.4", NCVersion: 4},
		},
		EndpointPolicies: []cns.NetworkContainerRequestPolicies{{Type: "acl"}},
		NCStatus:         nncv1alpha.NCUpdateSuccess,
	}
	writeEnvelope(t, cnsPath, map[string]any{
		"ContainerNetworkService": map[string]any{
			"OrchestratorType": "KubernetesCRD",
			"ContainerStatus": map[string]any{
				"nc-1": map[string]any{
					"ID":                            "nc-1",
					"VMVersion":                     "4",
					"HostVersion":                   "3",
					"VfpUpdateComplete":             true,
					"CreateNetworkContainerRequest": request,
				},
			},
		},
	})
	writeEnvelope(t, endpointPath, map[string]any{
		"Endpoints": map[string]any{
			"container-1": map[string]any{
				"PodName":      "pod-1",
				"PodNamespace": "default",
				"IfnameToIPMap": map[string]any{
					"eth0": map[string]any{
						"IPv4": []net.IPNet{{
							IP:   net.ParseIP("10.0.0.4"),
							Mask: net.CIDRMask(24, 32),
						}},
					},
				},
			},
		},
	})

	db, _ := openTestDB(t)
	require.NoError(t, db.ImportLegacy(ctx, state.ImportOptions{
		CNSJSONPath:         cnsPath,
		EndpointJSONPath:    endpointPath,
		ManageEndpointState: true,
		BootID:              "boot-1",
		Now:                 testNow,
	}))

	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Contains(t, snapshot.IPs, "dynamic-ip-uuid")
	assert.Equal(t, "container-1", snapshot.IPOwners["dynamic-ip-uuid"])
	assert.Equal(t, []string{"dynamic-ip-uuid"}, snapshot.Assignments["container-1"].IPIDs)
	record := snapshot.NetworkContainers["nc-1"]
	assert.Empty(t, record.Request.AuthorizationToken)
	assert.Equal(t, "fd00::", record.Request.IPv6Configuration.IPSubnet.IPAddress)
	require.Len(t, record.Request.EndpointPolicies, 1)
	assert.Equal(t, nncv1alpha.NCUpdateSuccess, record.Request.NCStatus)
}

func TestImportLegacyRealFixtures(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.ImportLegacy(ctx, state.ImportOptions{
		CNSJSONPath:         filepath.Join("testdata", "azure-cns.json"),
		EndpointJSONPath:    filepath.Join("testdata", "azure-endpoints.json"),
		ManageEndpointState: true,
		BootID:              "boot-1",
		Now:                 testNow,
	}))

	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Len(t, snapshot.NetworkContainers, 1)
	assert.Len(t, snapshot.IPs, 256)
	assert.Len(t, snapshot.Endpoints, 7)
	assert.Len(t, snapshot.Assignments, 7)
}

func TestImportLegacyMarkerSkipsStaleOrCorruptInputs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cnsPath := filepath.Join(dir, "azure-cns.json")
	writeEnvelope(t, cnsPath, map[string]any{"ContainerNetworkService": map[string]any{}})

	db, _ := openTestDB(t)
	require.NoError(t, db.ImportLegacy(ctx, state.ImportOptions{CNSJSONPath: cnsPath, BootID: "boot-1"}))
	require.NoError(t, os.WriteFile(cnsPath, []byte("{"), 0o600))
	require.NoError(t, db.ImportLegacy(ctx, state.ImportOptions{CNSJSONPath: cnsPath, BootID: "boot-1"}))
}

func TestImportLegacyRejectsDanglingEndpointWithoutPartialCommit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cnsPath := filepath.Join(dir, "azure-cns.json")
	endpointPath := filepath.Join(dir, "azure-endpoints.json")
	writeEnvelope(t, cnsPath, map[string]any{"ContainerNetworkService": map[string]any{}})
	writeEnvelope(t, endpointPath, map[string]any{
		"Endpoints": map[string]any{
			"container-1": map[string]any{
				"IfnameToIPMap": map[string]any{
					"eth0": map[string]any{
						"IPv4": []net.IPNet{{
							IP:   net.ParseIP("10.0.0.4"),
							Mask: net.CIDRMask(24, 32),
						}},
					},
				},
			},
		},
	})

	db, _ := openTestDB(t)
	err := db.ImportLegacy(ctx, state.ImportOptions{
		CNSJSONPath:         cnsPath,
		EndpointJSONPath:    endpointPath,
		ManageEndpointState: true,
		BootID:              "boot-1",
	})
	require.ErrorIs(t, err, state.ErrInconsistentState)

	snapshot, snapshotErr := db.Snapshot(ctx)
	require.NoError(t, snapshotErr)
	assert.Empty(t, snapshot.Endpoints)
	assert.Empty(t, snapshot.Assignments)
}

func TestRollbackExportCanBeReimported(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))
	assignment := state.AssignmentRecord{
		Pod:   state.PodIdentity{PodKey: "container-1", InfraContainerID: "container-1"},
		IPIDs: []string{"ip-uuid-1"},
	}
	require.NoError(t, db.AssignEndpoint(ctx, assignment, sampleEndpoint("10.0.0.4"), testNow, testIntentTTL))

	dir := t.TempDir()
	cnsPath := filepath.Join(dir, "azure-cns.json")
	endpointPath := filepath.Join(dir, "azure-endpoints.json")
	require.NoError(t, db.ExportLegacy(ctx, cnsPath, endpointPath))

	exported, err := os.ReadFile(cnsPath)
	require.NoError(t, err)
	assert.NotContains(t, string(exported), "must-not-persist")

	reimported, err := state.Open(filepath.Join(dir, "reimported.db"), state.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reimported.Close()) })
	require.NoError(t, reimported.ImportLegacy(ctx, state.ImportOptions{
		CNSJSONPath:         cnsPath,
		EndpointJSONPath:    endpointPath,
		ManageEndpointState: true,
		BootID:              "boot-1",
		Now:                 testNow,
	}))

	snapshot, err := reimported.Snapshot(ctx)
	require.NoError(t, err)
	assert.Contains(t, snapshot.IPs, "ip-uuid-1")
	assert.Equal(t, "container-1", snapshot.IPOwners["ip-uuid-1"])
}

func TestRollbackExportDoesNotOverwriteAuthoritativeJSON(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))

	dir := t.TempDir()
	cnsPath := filepath.Join(dir, "azure-cns.json")
	endpointPath := filepath.Join(dir, "azure-endpoints.json")
	require.NoError(t, db.ExportLegacy(ctx, cnsPath, endpointPath))
	require.NoError(t, os.WriteFile(cnsPath, []byte(`{"new":"json-state"}`), 0o600))

	require.NoError(t, db.ExportLegacy(ctx, cnsPath, endpointPath))
	data, err := os.ReadFile(cnsPath)
	require.NoError(t, err)
	assert.JSONEq(t, `{"new":"json-state"}`, string(data))
}

func TestRollbackExportRetriesAfterTornFilePair(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))

	dir := t.TempDir()
	cnsPath := filepath.Join(dir, "azure-cns.json")
	endpointPath := filepath.Join(dir, "azure-endpoints.json")
	require.NoError(t, os.Mkdir(endpointPath, 0o755))

	require.Error(t, db.ExportLegacy(ctx, cnsPath, endpointPath))
	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, state.AuthorityBolt, snapshot.Metadata.Authority)

	require.NoError(t, os.Remove(endpointPath))
	require.NoError(t, db.ExportLegacy(ctx, cnsPath, endpointPath))
	snapshot, err = db.Snapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, state.AuthorityJSON, snapshot.Metadata.Authority)
}

func writeEnvelope(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}
