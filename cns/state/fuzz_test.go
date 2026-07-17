// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//nolint:goconst // Repeated values keep fuzz wire fixtures readable.
package state_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/state"
	"github.com/Azure/azure-container-networking/cns/wireserver"
	"github.com/stretchr/testify/require"
)

const (
	fuzzAuthorizationToken = "AUTHORIZATION_TOKEN_MUST_NOT_PERSIST_7f0e9c3a"
	maxImportFuzzBytes     = 64 << 10
	maxRoundTripFuzzBytes  = 4 << 10
)

func FuzzImportLegacyCNSJSON(f *testing.F) {
	for _, seed := range [][]byte{
		nil,
		{},
		[]byte("{"),
		[]byte("null"),
		[]byte("[]"),
		[]byte("{}"),
	} {
		f.Add(seed)
	}
	f.Add(marshalFuzzSeed(f, emptyLegacyCNSEnvelope()))
	f.Add(marshalFuzzSeed(f, legacyFuzzCNSEnvelope()))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxImportFuzzBytes {
			t.Skip()
		}

		dir := t.TempDir()
		cnsPath := filepath.Join(dir, "azure-cns.json")
		endpointPath := filepath.Join(dir, "azure-endpoints.json")
		require.NoError(t, os.WriteFile(cnsPath, data, 0o600))
		writeFuzzJSON(t, endpointPath, emptyLegacyEndpointEnvelope())

		db := openImportFuzzDB(t, dir)
		before := readImportState(t, db)
		err := db.ImportLegacy(context.Background(), state.ImportOptions{
			CNSJSONPath:         cnsPath,
			EndpointJSONPath:    endpointPath,
			ManageEndpointState: false,
			BootID:              "fuzz-boot",
			Now:                 fuzzPropertyNow(),
		})
		if err != nil {
			assertImportStateUnchanged(t, db, before)
			return
		}

		assertSuccessfulFuzzImport(t, db)
	})
}

func FuzzImportLegacyEndpointJSON(f *testing.F) {
	for _, seed := range [][]byte{
		nil,
		{},
		[]byte("{"),
		[]byte("null"),
		[]byte("[]"),
		[]byte("{}"),
	} {
		f.Add(seed)
	}
	f.Add(marshalFuzzSeed(f, emptyLegacyEndpointEnvelope()))
	f.Add(marshalFuzzSeed(f, legacyFuzzEndpointEnvelope()))
	f.Add(marshalFuzzSeed(f, legacyDuplicateOwnerEndpointEnvelope()))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxImportFuzzBytes {
			t.Skip()
		}

		dir := t.TempDir()
		cnsPath := filepath.Join(dir, "azure-cns.json")
		endpointPath := filepath.Join(dir, "azure-endpoints.json")
		writeFuzzJSON(t, cnsPath, legacyFuzzCNSEnvelope())
		require.NoError(t, os.WriteFile(endpointPath, data, 0o600))

		db := openImportFuzzDB(t, dir)
		before := readImportState(t, db)
		err := db.ImportLegacy(context.Background(), state.ImportOptions{
			CNSJSONPath:         cnsPath,
			EndpointJSONPath:    endpointPath,
			ManageEndpointState: true,
			BootID:              "fuzz-boot",
			Now:                 fuzzPropertyNow(),
			DeleteIntentTTL:     48 * time.Hour,
		})
		if err != nil {
			assertImportStateUnchanged(t, db, before)
			return
		}

		assertSuccessfulFuzzImport(t, db)
	})
}

func FuzzSnapshotValidate(f *testing.F) {
	const mutationCount = 13
	for mutation := range mutationCount {
		f.Add(uint8(mutation), []byte{byte(mutation), 0x5a})
	}

	f.Fuzz(func(t *testing.T, selector uint8, payload []byte) {
		snapshot := validStateSnapshot()
		require.NoError(t, snapshot.Validate())

		value := fmt.Sprintf("mutated-%x", payloadPrefix(payload, 16))
		mutation := int(selector) % mutationCount
		switch mutation {
		case 0:
			snapshot.Metadata.SchemaVersion++
		case 1:
			record := snapshot.NetworkContainers[testNCID]
			record.ID = value
			snapshot.NetworkContainers[testNCID] = record
		case 2:
			record := snapshot.NetworkContainers[testNCID]
			record.Request.IPv6Configuration.IPSubnet.IPAddress = "invalid-" + value
			snapshot.NetworkContainers[testNCID] = record
		case 3:
			record := snapshot.IPs[testIPID]
			record.ID = value
			snapshot.IPs[testIPID] = record
		case 4:
			record := snapshot.IPs[testIPID]
			record.NCID = value
			snapshot.IPs[testIPID] = record
		case 5:
			record := snapshot.IPs[testIPID]
			record.IPAddress = "invalid-" + value
			snapshot.IPs[testIPID] = record
		case 6:
			endpoint := snapshot.Endpoints[testContainerID]
			endpoint.IfnameToIPMap[testIfName].IPv4[0].IP = net.IP{1, 2}
			snapshot.Endpoints[testContainerID] = endpoint
		case 7:
			endpoint := snapshot.Endpoints[testContainerID]
			endpoint.IfnameToIPMap[testIfName].IPv4[0].Mask = net.IPMask{0xff, 0, 0xff, 0}
			snapshot.Endpoints[testContainerID] = endpoint
		case 8:
			assignment := snapshot.Assignments[testContainerID]
			assignment.Pod.PodKey = value
			snapshot.Assignments[testContainerID] = assignment
		case 9:
			assignment := snapshot.Assignments[testContainerID]
			assignment.Pod.InfraContainerID = value
			snapshot.Assignments[testContainerID] = assignment
		case 10:
			assignment := snapshot.Assignments[testContainerID]
			assignment.IPIDs[0] = value
			snapshot.Assignments[testContainerID] = assignment
		case 11:
			snapshot.IPOwners[testIPID] = value
		case 12:
			endpoint := snapshot.Endpoints[testContainerID]
			endpoint.IfnameToIPMap[testIfName].IPv4[0].IP = net.ParseIP("10.0.0.5")
			snapshot.Endpoints[testContainerID] = endpoint
		}

		err := snapshot.Validate()
		if mutation == 0 {
			require.ErrorIs(t, err, state.ErrSchemaMismatch)
			return
		}
		require.ErrorIs(t, err, state.ErrInconsistentState)
	})
}

func FuzzExportImportRoundTrip(f *testing.F) {
	for _, seed := range [][]byte{
		nil,
		{0},
		{1, 2, 3, 4},
		{0xff, 0x80, 0x40, 0x20, 0x10},
		[]byte("dual-stack-multi-nic"),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxRoundTripFuzzBytes {
			t.Skip()
		}

		ctx := context.Background()
		dir := t.TempDir()
		sourcePath := filepath.Join(dir, "source.db")
		source, err := state.Open(sourcePath, state.Options{NoSync: true})
		require.NoError(t, err)
		t.Cleanup(func() {
			if source != nil {
				require.NoError(t, source.Close())
			}
		})

		seedRoundTripState(t, source, data)
		before, err := source.Snapshot(ctx)
		require.NoError(t, err)
		assertNoAuthorizationTokens(t, before)
		require.NoError(t, source.Close())
		source = nil
		assertFileOmitsSecret(t, sourcePath)
		source, err = state.Open(sourcePath, state.Options{NoSync: true})
		require.NoError(t, err)

		cnsPath := filepath.Join(dir, "azure-cns.json")
		endpointPath := filepath.Join(dir, "azure-endpoints.json")
		require.NoError(t, source.ExportLegacy(ctx, cnsPath, endpointPath))
		assertFileOmitsSecret(t, cnsPath)
		assertFileOmitsSecret(t, endpointPath)

		reimportPath := filepath.Join(dir, "reimport.db")
		reimported, err := state.Open(reimportPath, state.Options{NoSync: true})
		require.NoError(t, err)
		t.Cleanup(func() {
			require.NoError(t, reimported.Close())
		})
		require.NoError(t, reimported.ImportLegacy(ctx, state.ImportOptions{
			CNSJSONPath:         cnsPath,
			EndpointJSONPath:    endpointPath,
			ManageEndpointState: true,
			BootID:              before.Metadata.BootID,
			Now:                 fuzzPropertyNow().Add(time.Hour),
			DeleteIntentTTL:     48 * time.Hour,
		}))

		after, err := reimported.Snapshot(ctx)
		require.NoError(t, err)
		assertNoAuthorizationTokens(t, after)
		require.Equal(t, normalizeRoundTripSnapshot(before), normalizeRoundTripSnapshot(after))
	})
}

func openImportFuzzDB(t *testing.T, dir string) *state.DB {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(dir, "import.db")
	db, err := state.Open(path, state.Options{NoSync: true})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	require.NoError(t, db.ApplyNetworkContainer(ctx, sampleNCRecord(), sampleIPs()))
	require.NoError(t, db.AssignEndpoint(
		ctx,
		testAssignment(testIPID),
		sampleEndpoint(testIPAddress),
		fuzzPropertyNow(),
		24*time.Hour,
	))
	require.NoError(t, db.Update(ctx, func(tx *state.WriteTx) error {
		meta, err := tx.Metadata()
		if err != nil {
			return fmt.Errorf("reading metadata: %w", err)
		}
		meta.Authority = state.AuthorityJSON
		meta.BootID = "baseline-boot"
		meta.NodeID = "baseline-node"
		if err := tx.PutMetadata(meta); err != nil {
			return fmt.Errorf("writing metadata: %w", err)
		}
		if err := tx.PutNetwork(state.NetworkRecord{
			NetworkName: "baseline-network",
			Options: map[string]any{
				"mode": "fuzz",
			},
		}); err != nil {
			return fmt.Errorf("writing network: %w", err)
		}
		if err := tx.PutOrchestratorContext("baseline-context", []string{testNCID}); err != nil {
			return fmt.Errorf("writing orchestrator context: %w", err)
		}
		if err := tx.PutPnPIDByMAC("00:11:22:33:44:55", "baseline-pnp"); err != nil {
			return fmt.Errorf("writing PnP ID: %w", err)
		}
		if err := tx.PutDeleteIntent("deleted-container", state.DeleteIntent{
			CreatedAt: fuzzPropertyNow().Add(-time.Hour),
		}); err != nil {
			return fmt.Errorf("writing delete intent: %w", err)
		}
		if err := tx.SetRollbackComplete(); err != nil {
			return fmt.Errorf("setting rollback marker: %w", err)
		}
		return nil
	}))
	return db
}

func assertSuccessfulFuzzImport(t *testing.T, db *state.DB) {
	t.Helper()
	snapshot, err := db.Snapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, state.AuthorityBolt, snapshot.Metadata.Authority)
	assertNoAuthorizationTokens(t, snapshot)

	observed := readImportState(t, db)
	require.True(t, observed.migrationComplete)
	require.False(t, observed.rollbackComplete)
	require.Equal(t, state.AuthorityBolt, observed.metadata.Authority)
}

func assertNoAuthorizationTokens(t *testing.T, snapshot state.Snapshot) {
	t.Helper()
	for ncID := range snapshot.NetworkContainers {
		record := snapshot.NetworkContainers[ncID]
		require.Empty(t, record.Request.AuthorizationToken, "NC %q persisted an authorization token", ncID)
	}
}

func assertFileOmitsSecret(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.False(t, bytes.Contains(data, []byte(fuzzAuthorizationToken)), "secret persisted in %q", path)
	require.False(t, bytes.Contains(data, []byte("must-not-persist")), "test secret persisted in %q", path)
}

func seedRoundTripState(t *testing.T, db *state.DB, data []byte) {
	t.Helper()
	ctx := context.Background()
	cursor := fuzzByteCursor{data: data}
	ncCount := 2 + int(cursor.next()%2)
	pairsPerNC := 2 + int(cursor.next()%2)

	durable := state.NewSnapshot()
	durable.Metadata = state.Metadata{
		SchemaVersion:    state.SchemaVersion,
		Authority:        state.AuthorityBolt,
		BootID:           fmt.Sprintf("boot-%02x", cursor.next()),
		OrchestratorType: "KubernetesCRD",
		NodeID:           fmt.Sprintf("node-%02x", cursor.next()),
		Location:         "westus2",
		NetworkType:      "azure",
		Initialized:      true,
		TimeStamp:        fuzzPropertyNow(),
	}

	v4ByNC := make(map[string][]state.IPRecord, ncCount)
	v6ByNC := make(map[string][]state.IPRecord, ncCount)
	allNCIDs := make([]string, 0, ncCount)
	for ncIndex := range ncCount {
		ncID := fmt.Sprintf("nc-%d", ncIndex)
		allNCIDs = append(allNCIDs, ncID)
		request := cns.CreateNetworkContainerRequest{
			NetworkContainerid:         ncID,
			Version:                    strconv.FormatUint(uint64(1+cursor.next()%9), 10),
			AuthorizationToken:         fuzzAuthorizationToken,
			OrchestratorContext:        json.RawMessage(fmt.Sprintf(`{"nc":%q}`, ncID)),
			IPConfiguration:            fuzzIPConfiguration(fmt.Sprintf("10.%d.0.0", ncIndex+1), 16),
			IPv6Configuration:          fuzzIPConfiguration(fmt.Sprintf("fd00:%x::", ncIndex+1), 64),
			CnetAddressSpace:           []cns.IPSubnet{{IPAddress: fmt.Sprintf("172.16.%d.0", ncIndex), PrefixLength: 24}},
			EndpointPolicies:           []cns.NetworkContainerRequestPolicies{{Type: "acl"}},
			SecondaryIPConfigs:         map[string]cns.SecondaryIPConfig{"discarded": {IPAddress: "192.0.2.1"}},
			SkipDefaultRoutes:          cursor.next()%2 == 0,
			AllowHostToNCCommunication: true,
		}
		durable.NetworkContainers[ncID] = state.NetworkContainerRecord{
			ID:                ncID,
			VMVersion:         strconv.FormatUint(uint64(2+cursor.next()%4), 10),
			HostVersion:       strconv.FormatUint(uint64(1+cursor.next()%4), 10),
			VFPUpdateComplete: cursor.next()%2 == 0,
			Request:           request,
		}
		durable.Networks["network-"+ncID] = state.NetworkRecord{
			NetworkName: "network-" + ncID,
			NicInfo: &wireserver.InterfaceInfo{
				Subnet:       fmt.Sprintf("10.%d.0.0/16", ncIndex+1),
				Gateway:      fmt.Sprintf("10.%d.0.1", ncIndex+1),
				IsPrimary:    ncIndex == 0,
				PrimaryIP:    fmt.Sprintf("10.%d.0.4", ncIndex+1),
				SecondaryIPs: []string{fmt.Sprintf("10.%d.0.5", ncIndex+1)},
			},
			Options: map[string]any{
				"enabled": true,
				"mode":    "fuzz",
				"weight":  float64(ncIndex + 1),
			},
		}
		durable.OrchestratorContexts["context-"+ncID] = []string{ncID}
		durable.PnPIDByMAC[fmt.Sprintf("00:11:22:33:44:%02x", ncIndex)] = "pnp-" + ncID

		for pairIndex := range pairsPerNC {
			v4 := state.IPRecord{
				ID:        fmt.Sprintf("%s-v4-%d", ncID, pairIndex),
				IPAddress: fmt.Sprintf("10.%d.%d.%d", ncIndex+1, pairIndex+1, 10+pairIndex),
				NCID:      ncID,
				NCVersion: 1 + int(cursor.next()%9),
			}
			v6 := state.IPRecord{
				ID:        fmt.Sprintf("%s-v6-%d", ncID, pairIndex),
				IPAddress: fmt.Sprintf("fd00:%x:%x::%x", ncIndex+1, pairIndex+1, 10+pairIndex),
				NCID:      ncID,
				NCVersion: 1 + int(cursor.next()%9),
			}
			durable.IPs[v4.ID] = v4
			durable.IPs[v6.ID] = v6
			v4ByNC[ncID] = append(v4ByNC[ncID], v4)
			v6ByNC[ncID] = append(v6ByNC[ncID], v6)
		}
	}
	durable.OrchestratorContexts["all"] = allNCIDs
	require.NoError(t, db.ReplaceDurableState(ctx, durable))

	containers := make([]string, 0, ncCount)
	for ncIndex, ncID := range allNCIDs {
		pairIndex := int(cursor.next()) % pairsPerNC
		records := []state.IPRecord{v4ByNC[ncID][pairIndex], v6ByNC[ncID][pairIndex]}
		containerID := fmt.Sprintf("container-%d", ncIndex)
		containers = append(containers, containerID)
		require.NoError(t, db.AssignEndpoint(
			ctx,
			state.AssignmentRecord{
				Pod: state.PodIdentity{
					PodKey:           containerID,
					InfraContainerID: containerID,
					InterfaceID:      containerID,
					PodName:          "pod-" + containerID,
					PodNamespace:     "default",
				},
				IPIDs: []string{records[0].ID, records[1].ID},
			},
			endpointForIPs(containerID, records),
			fuzzPropertyNow(),
			48*time.Hour,
		))
	}

	tombstoned := containers[len(containers)-1]
	require.NoError(t, db.ReleaseEndpoint(
		ctx,
		tombstoned,
		tombstoned,
		state.DeleteIntent{CreatedAt: fuzzPropertyNow()},
		48*time.Hour,
	))
}

func endpointForIPs(containerID string, records []state.IPRecord) state.EndpointRecord {
	endpoint := state.EndpointRecord{
		PodName:       "pod-" + containerID,
		PodNamespace:  "default",
		IfnameToIPMap: make(map[string]*state.IPInfoRecord, len(records)+1),
	}
	for index, record := range records {
		info := &state.IPInfoRecord{
			NetworkContainerID: record.NCID,
			NICType:            cns.InfraNIC,
		}
		ip := net.ParseIP(record.IPAddress)
		if ip.To4() != nil {
			info.IPv4 = []net.IPNet{{
				IP:   ip,
				Mask: net.CIDRMask(24, 32),
			}}
		} else {
			info.IPv6 = []net.IPNet{{
				IP:   ip,
				Mask: net.CIDRMask(64, 128),
			}}
		}
		endpoint.IfnameToIPMap[fmt.Sprintf("eth%d", index)] = info
	}
	endpoint.IfnameToIPMap["delegated0"] = &state.IPInfoRecord{
		IPv4: []net.IPNet{{
			IP:   net.ParseIP("192.0.2.10"),
			Mask: net.CIDRMask(32, 32),
		}},
		HostVethName: "veth-" + containerID,
		NICType:      cns.DelegatedVMNIC,
	}
	return endpoint
}

func normalizeRoundTripSnapshot(snapshot state.Snapshot) state.Snapshot {
	snapshot.Metadata.Generation = 0
	assignments := make(map[string]state.AssignmentRecord, len(snapshot.Assignments))
	for podKey, assignment := range snapshot.Assignments {
		assignment.IPIDs = slices.Clone(assignment.IPIDs)
		slices.Sort(assignment.IPIDs)
		assignments[podKey] = assignment
	}
	snapshot.Assignments = assignments
	return snapshot
}

func legacyFuzzCNSEnvelope() map[string]any {
	return map[string]any{
		"ContainerNetworkService": map[string]any{
			"Location":         "westus2",
			"NetworkType":      "azure",
			"OrchestratorType": "KubernetesCRD",
			"NodeID":           "fuzz-node",
			"Initialized":      true,
			"ContainerIDByOrchestratorContext": map[string]string{
				"fuzz-context": "nc-fuzz",
			},
			"ContainerStatus": map[string]any{
				"nc-fuzz": map[string]any{
					"ID":                "nc-fuzz",
					"VMVersion":         "2",
					"HostVersion":       "1",
					"VfpUpdateComplete": true,
					"CreateNetworkContainerRequest": cns.CreateNetworkContainerRequest{
						NetworkContainerid: "nc-fuzz",
						Version:            "2",
						AuthorizationToken: fuzzAuthorizationToken,
						IPv6Configuration: fuzzIPConfiguration(
							"fd00::",
							120,
						),
						SecondaryIPConfigs: map[string]cns.SecondaryIPConfig{
							"fuzz-v4": {IPAddress: "10.0.0.4", NCVersion: 2},
							"fuzz-v6": {IPAddress: "fd00::4", NCVersion: 2},
						},
					},
				},
			},
		},
	}
}

func legacyFuzzEndpointEnvelope() map[string]any {
	return map[string]any{
		"Endpoints": map[string]any{
			"container-fuzz": map[string]any{
				"PodName":      "pod-fuzz",
				"PodNamespace": "default",
				"IfnameToIPMap": map[string]any{
					"eth0": map[string]any{
						"IPv4": []net.IPNet{{
							IP:   net.ParseIP("10.0.0.4"),
							Mask: net.CIDRMask(24, 32),
						}},
						"NetworkContainerID": "nc-fuzz",
						"NICType":            cns.InfraNIC,
					},
					"eth1": map[string]any{
						"IPv6": []net.IPNet{{
							IP:   net.ParseIP("fd00::4"),
							Mask: net.CIDRMask(120, 128),
						}},
						"NetworkContainerID": "nc-fuzz",
						"NICType":            cns.InfraNIC,
					},
				},
			},
		},
		"EndpointDeleteIntents": map[string]any{
			"deleted-fuzz": map[string]any{
				"createdAt": fuzzPropertyNow(),
			},
		},
	}
}

func legacyDuplicateOwnerEndpointEnvelope() map[string]any {
	envelope := legacyFuzzEndpointEnvelope()
	endpoints := envelope["Endpoints"].(map[string]any)
	endpoints["container-duplicate"] = legacyEndpoint("pod-duplicate", "10.0.0.4")
	return envelope
}

func fuzzIPConfiguration(address string, prefixLength uint8) cns.IPConfiguration {
	return cns.IPConfiguration{
		IPSubnet: cns.IPSubnet{
			IPAddress:    address,
			PrefixLength: prefixLength,
		},
	}
}

func marshalFuzzSeed(f *testing.F, value any) []byte {
	f.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		f.Fatalf("marshaling fuzz seed: %v", err)
	}
	return data
}

func writeFuzzJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func payloadPrefix(payload []byte, limit int) []byte {
	if len(payload) <= limit {
		return payload
	}
	return payload[:limit]
}

func fuzzPropertyNow() time.Time {
	return time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
}

type fuzzByteCursor struct {
	data  []byte
	index int
}

func (c *fuzzByteCursor) next() byte {
	if len(c.data) == 0 {
		return 0
	}
	value := c.data[c.index%len(c.data)]
	c.index++
	return value
}
