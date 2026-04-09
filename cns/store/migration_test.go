// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package store_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/azure-container-networking/cns/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testdataDir is the relative path from the package root to the testdata files.
const testdataDir = "../state/testdata"

// copyTestdata copies a testdata file into a temporary directory and returns
// the destination path.  This prevents migration from renaming the tracked
// source file.
func copyTestdata(t *testing.T, filename string) string {
	t.Helper()
	src := filepath.Join(testdataDir, filename)
	raw, err := os.ReadFile(src)
	require.NoError(t, err, "read testdata file %s", src)

	dst := filepath.Join(t.TempDir(), filename)
	require.NoError(t, os.WriteFile(dst, raw, 0o600))
	return dst
}

// ---- MigrateCNSState ----

func TestMigrateCNSState_FromTestdata(t *testing.T) {
	ctx := context.Background()
	jsonPath := copyTestdata(t, "azure-cns.json")

	s, err := store.OpenNCStore(t.TempDir()+"/cns.db", nil)
	require.NoError(t, err)
	defer s.Close()

	require.NoError(t, store.MigrateCNSState(ctx, jsonPath, s))

	// Verify NC count — testdata has exactly one NC.
	ncs, err := s.ListNCs(ctx)
	require.NoError(t, err)
	assert.Len(t, ncs, 1, "expected 1 NC after migration")

	nc := ncs[0]
	assert.Equal(t, "a2e19e42-4505-499f-87b4-9f235c03207f", nc.ID)
	assert.Equal(t, "Docker", nc.NetworkContainerType)
	assert.Equal(t, "10.10.0.4", nc.HostPrimaryIP)

	// Verify IPs were migrated — testdata NC has 256 secondary IPs.
	ips, err := s.ListIPs(ctx)
	require.NoError(t, err)
	assert.Len(t, ips, 256, "expected 256 secondary IPs after migration")

	// Every IP must point back to the single NC.
	for _, ip := range ips {
		assert.Equal(t, nc.ID, ip.NCID, "IP %s has wrong NCID", ip.IPAddress)
		assert.NotEmpty(t, ip.IPAddress)
	}

	// Verify metadata.
	meta, err := s.GetMeta(ctx)
	require.NoError(t, err)
	assert.Equal(t, "KubernetesCRD", meta.OrchestratorType)

	// Verify the source file was renamed.
	_, err = os.Stat(jsonPath)
	assert.True(t, os.IsNotExist(err), "source JSON should have been renamed")
	_, err = os.Stat(jsonPath + ".migrated")
	assert.NoError(t, err, ".migrated backup should exist")
}

func TestMigrateCNSState_Idempotent(t *testing.T) {
	ctx := context.Background()
	jsonPath := copyTestdata(t, "azure-cns.json")

	s, err := store.OpenNCStore(t.TempDir()+"/cns.db", nil)
	require.NoError(t, err)
	defer s.Close()

	// First migration.
	require.NoError(t, store.MigrateCNSState(ctx, jsonPath, s))
	ncs1, err := s.ListNCs(ctx)
	require.NoError(t, err)

	// The source was renamed; restore it so the second call has a file to read.
	// Since it was renamed to .migrated and the store already has data, the
	// second call should detect existing NCs and be a no-op.  We can test the
	// idempotency without a file by calling with a non-existent path.
	noFile := t.TempDir() + "/no-such.json"
	require.NoError(t, store.MigrateCNSState(ctx, noFile, s))

	ncs2, err := s.ListNCs(ctx)
	require.NoError(t, err)
	assert.Equal(t, len(ncs1), len(ncs2), "second migration must not modify NC count")
}

func TestMigrateCNSState_PartialRecovery(t *testing.T) {
	ctx := context.Background()
	jsonPath := copyTestdata(t, "azure-cns.json")

	s, err := store.OpenNCStore(t.TempDir()+"/cns.db", nil)
	require.NoError(t, err)
	defer s.Close()

	// Simulate an interrupted migration by pre-populating only a single NC row.
	require.NoError(t, s.PutNC(ctx, sampleNC("a2e19e42-4505-499f-87b4-9f235c03207f")))

	require.NoError(t, store.MigrateCNSState(ctx, jsonPath, s))

	ncs, err := s.ListNCs(ctx)
	require.NoError(t, err)
	require.Len(t, ncs, 1)
	assert.Equal(t, "10.10.0.4", ncs[0].HostPrimaryIP)

	ips, err := s.ListIPs(ctx)
	require.NoError(t, err)
	assert.Len(t, ips, 256, "partial state should be completed on rerun")
}

func TestMigrateCNSState_CompleteMarkerSkipsRerun(t *testing.T) {
	ctx := context.Background()
	jsonPath := copyTestdata(t, "azure-cns.json")

	s, err := store.OpenNCStore(t.TempDir()+"/cns.db", nil)
	require.NoError(t, err)
	defer s.Close()

	require.NoError(t, store.MigrateCNSState(ctx, jsonPath, s))

	// Recreate source path; marker should cause early no-op before file handling.
	require.NoError(t, os.WriteFile(jsonPath, []byte("{}"), 0o600))
	require.NoError(t, store.MigrateCNSState(ctx, jsonPath, s))

	_, err = os.Stat(jsonPath)
	assert.NoError(t, err, "second migration should skip before renaming source")
}

func TestMigrateCNSState_EmptyFile(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	// Write an empty JSON object (not even a CNS key).
	emptyPath := filepath.Join(tmp, "empty.json")
	require.NoError(t, os.WriteFile(emptyPath, []byte("{}"), 0o600))

	s, err := store.OpenNCStore(tmp+"/cns.db", nil)
	require.NoError(t, err)
	defer s.Close()

	require.NoError(t, store.MigrateCNSState(ctx, emptyPath, s))

	ncs, err := s.ListNCs(ctx)
	require.NoError(t, err)
	assert.Empty(t, ncs)
}

func TestMigrateCNSState_FileNotExist(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	// Non-existent file is not an error — fresh node.
	require.NoError(t, store.MigrateCNSState(ctx, "/no/such/file.json", s))
}

// ---- MigrateEndpointState ----

func TestMigrateEndpointState_FromTestdata(t *testing.T) {
	ctx := context.Background()
	jsonPath := copyTestdata(t, "azure-endpoints.json")

	s, err := store.OpenEndpointStore(t.TempDir()+"/endpoints.db", nil)
	require.NoError(t, err)
	defer s.Close()

	require.NoError(t, store.MigrateEndpointState(ctx, jsonPath, s))

	eps, err := s.ListEndpoints(ctx)
	require.NoError(t, err)

	// testdata has 7 endpoints.
	assert.Len(t, eps, 7, "expected 7 endpoints after migration")

	// Spot-check a known entry.
	const knownCID = "44e640acfd7fea8ab5b82adf9537ad94c2ffbf5855dcdc1907b00f47654a37f5"
	ep, ok := eps[knownCID]
	require.True(t, ok, "expected container ID %s to be present", knownCID)
	assert.Equal(t, "gatekeeper-controller-74df7bdb7-7c5l4", ep.PodName)
	assert.Equal(t, "gatekeeper-system", ep.PodNamespace)
	require.Contains(t, ep.IfnameToIPMap, "")
	ipInfo := ep.IfnameToIPMap[""]
	require.NotEmpty(t, ipInfo.IPv4)
	assert.Equal(t, "192.168.0.109", ipInfo.IPv4[0].IP.String())

	// Verify source renamed.
	_, err = os.Stat(jsonPath)
	assert.True(t, os.IsNotExist(err), "source should have been renamed")
	_, err = os.Stat(jsonPath + ".migrated")
	assert.NoError(t, err, ".migrated backup should exist")
}

func TestMigrateEndpointState_Idempotent(t *testing.T) {
	ctx := context.Background()
	jsonPath := copyTestdata(t, "azure-endpoints.json")

	s, err := store.OpenEndpointStore(t.TempDir()+"/endpoints.db", nil)
	require.NoError(t, err)
	defer s.Close()

	// First migration.
	require.NoError(t, store.MigrateEndpointState(ctx, jsonPath, s))
	eps1, _ := s.ListEndpoints(ctx)

	// Second call with a non-existent file — should be a no-op.
	require.NoError(t, store.MigrateEndpointState(ctx, "/no/such/endpoints.json", s))
	eps2, err := s.ListEndpoints(ctx)
	require.NoError(t, err)
	assert.Equal(t, len(eps1), len(eps2))
}

func TestMigrateEndpointState_PartialRecovery(t *testing.T) {
	ctx := context.Background()
	jsonPath := copyTestdata(t, "azure-endpoints.json")

	s, err := store.OpenEndpointStore(t.TempDir()+"/endpoints.db", nil)
	require.NoError(t, err)
	defer s.Close()

	// Simulate interrupted migration by seeding only one endpoint.
	require.NoError(t, s.PutEndpoint(ctx, "seed", sampleEndpoint("seed-pod", "default")))

	require.NoError(t, store.MigrateEndpointState(ctx, jsonPath, s))

	eps, err := s.ListEndpoints(ctx)
	require.NoError(t, err)
	assert.Len(t, eps, 8, "partial endpoint state should be completed on rerun")
	assert.Contains(t, eps, "44e640acfd7fea8ab5b82adf9537ad94c2ffbf5855dcdc1907b00f47654a37f5")
}

func TestMigrateEndpointState_CompleteMarkerSkipsRerun(t *testing.T) {
	ctx := context.Background()
	jsonPath := copyTestdata(t, "azure-endpoints.json")

	s, err := store.OpenEndpointStore(t.TempDir()+"/endpoints.db", nil)
	require.NoError(t, err)
	defer s.Close()

	require.NoError(t, store.MigrateEndpointState(ctx, jsonPath, s))

	// Recreate source path; marker should cause early no-op before file handling.
	require.NoError(t, os.WriteFile(jsonPath, []byte("{}"), 0o600))
	require.NoError(t, store.MigrateEndpointState(ctx, jsonPath, s))

	_, err = os.Stat(jsonPath)
	assert.NoError(t, err, "second migration should skip before renaming source")
}

func TestMigrateEndpointState_EmptyFile(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	emptyPath := filepath.Join(tmp, "endpoints.json")
	require.NoError(t, os.WriteFile(emptyPath, []byte("{}"), 0o600))

	s, err := store.OpenEndpointStore(tmp+"/ep.db", nil)
	require.NoError(t, err)
	defer s.Close()

	require.NoError(t, store.MigrateEndpointState(ctx, emptyPath, s))

	eps, err := s.ListEndpoints(ctx)
	require.NoError(t, err)
	assert.Empty(t, eps)
}

func TestMigrateEndpointState_FileNotExist(t *testing.T) {
	ctx := context.Background()
	s := openTestEndpointStore(t)

	require.NoError(t, store.MigrateEndpointState(ctx, "/no/such/endpoints.json", s))
}

// ---- Full round-trip ----

func TestMigrateAll_RoundTrip(t *testing.T) {
	ctx := context.Background()
	cnsJSON := copyTestdata(t, "azure-cns.json")
	epJSON := copyTestdata(t, "azure-endpoints.json")

	tmp := t.TempDir()
	ncStore, err := store.OpenNCStore(tmp+"/cns.db", nil)
	require.NoError(t, err)
	defer ncStore.Close()

	epStore, err := store.OpenEndpointStore(tmp+"/endpoints.db", nil)
	require.NoError(t, err)
	defer epStore.Close()

	require.NoError(t, store.MigrateCNSState(ctx, cnsJSON, ncStore))
	require.NoError(t, store.MigrateEndpointState(ctx, epJSON, epStore))

	// Build a map of IP → NCID from the IPs bucket.
	allIPs, err := ncStore.ListIPs(ctx)
	require.NoError(t, err)
	ipToNCID := make(map[string]string, len(allIPs))
	for _, ip := range allIPs {
		ipToNCID[ip.IPAddress] = ip.NCID
	}

	// Every endpoint's IPv4 address should be present in the IPs bucket and
	// point to the single NC in testdata.
	ncs, err := ncStore.ListNCs(ctx)
	require.NoError(t, err)
	require.Len(t, ncs, 1)
	ncID := ncs[0].ID

	eps, err := epStore.ListEndpoints(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, eps)

	for containerID, ep := range eps {
		for _, ipInfo := range ep.IfnameToIPMap {
			for _, ipNet := range ipInfo.IPv4 {
				ipStr := ipNet.IP.String()
				gotNCID, found := ipToNCID[ipStr]
				assert.True(t, found, "endpoint %s IP %s not in IPs bucket", containerID, ipStr)
				if found {
					assert.Equal(t, ncID, gotNCID,
						"endpoint %s IP %s NCID mismatch", containerID, ipStr)
				}
			}
		}
	}

	// Verify testdata JSON files were renamed.
	_, statErr := os.Stat(cnsJSON)
	assert.True(t, os.IsNotExist(statErr), "azure-cns.json copy should have been renamed")
	_, statErr = os.Stat(epJSON)
	assert.True(t, os.IsNotExist(statErr), "azure-endpoints.json copy should have been renamed")
}

// ---- JSON round-trip fidelity for NCRecord ----

func TestNCRecord_JSONFidelity(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	in := sampleNC("nc-fidelity")
	in.OrchestratorContext = []byte(`{"podName":"foo","podNamespace":"bar"}`)
	require.NoError(t, s.PutNC(ctx, in))

	got, err := s.GetNC(ctx, "nc-fidelity")
	require.NoError(t, err)

	// OrchestratorContext should round-trip as raw JSON bytes.
	var inCtx, outCtx map[string]interface{}
	require.NoError(t, json.Unmarshal(in.OrchestratorContext, &inCtx))
	require.NoError(t, json.Unmarshal(got.OrchestratorContext, &outCtx))
	assert.Equal(t, inCtx, outCtx)
}

func TestMigrateEndpointState_IPNetMaskPreserved(t *testing.T) {
	ctx := context.Background()
	jsonPath := copyTestdata(t, "azure-endpoints.json")

	s, err := store.OpenEndpointStore(t.TempDir()+"/ep-mask.db", nil)
	require.NoError(t, err)
	defer s.Close()

	require.NoError(t, store.MigrateEndpointState(ctx, jsonPath, s))

	eps, err := s.ListEndpoints(ctx)
	require.NoError(t, err)

	// Verify at least one endpoint has a non-nil mask after migration.
	found := false
	for _, ep := range eps {
		for _, ipInfo := range ep.IfnameToIPMap {
			for _, ipNet := range ipInfo.IPv4 {
				require.NotNil(t, ipNet.Mask, "IPv4 mask should not be nil after migration")
				ones, bits := ipNet.Mask.Size()
				assert.Positive(t, ones, "mask should have non-zero prefix length")
				assert.Equal(t, 32, bits, "IPv4 mask should be 32-bit")
				found = true
			}
		}
	}
	assert.True(t, found, "should have found at least one IPv4 address to check mask")
}
