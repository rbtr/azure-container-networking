// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//nolint:goconst // Repeated values keep state-machine fixtures readable.
package state_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/state"
	"github.com/stretchr/testify/require"
)

const stateMachineSecret = "STATE_MACHINE_AUTHORIZATION_TOKEN_MUST_NOT_PERSIST"

type randomizedStateMachine struct {
	t             *testing.T
	ctx           context.Context
	db            *state.DB
	path          string
	rng           *rand.Rand
	now           time.Time
	intentTTL     time.Duration
	bootID        string
	bootSequence  int
	nextContainer int
	nextDynamicIP int
	records       map[string]state.NetworkContainerRecord
	ipsByNC       map[string]map[string]state.IPRecord
	ncOrdinals    map[string]int
	ncRevisions   map[string]int
	assignments   map[string]state.AssignmentRecord
	owners        map[string]string
	endpoints     map[string]state.EndpointRecord
	deleteIntents map[string]state.DeleteIntent
}

func TestRandomizedAssignmentStateMachine(t *testing.T) {
	for _, seed := range []int64{42, 20260717} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			machine := newRandomizedStateMachine(t, seed)
			machine.exerciseRequiredTransitions()
			for step := range 180 {
				machine.now = machine.now.Add(time.Second)
				switch machine.rng.Intn(9) {
				case 0:
					machine.assignRandom()
				case 1:
					machine.releaseRandom()
				case 2:
					machine.retryRandomTombstone(machine.rng.Intn(2) == 0)
				case 3:
					machine.pruneRandom()
				case 4:
					machine.mutateInventory()
				case 5:
					machine.reopen()
				case 6:
					machine.applyBoot(machine.rng.Intn(2) == 0, machine.rng.Intn(2) == 0)
				case 7:
					machine.patchRandomEndpoint()
				case 8:
					machine.applyCurrentBoot()
				}
				machine.assertState(fmt.Sprintf("step %d", step))
			}
		})
	}
}

func newRandomizedStateMachine(t *testing.T, seed int64) *randomizedStateMachine {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state-machine.db")
	db, err := state.Open(path, state.Options{NoSync: true})
	require.NoError(t, err)

	machine := &randomizedStateMachine{
		t:             t,
		ctx:           context.Background(),
		db:            db,
		path:          path,
		rng:           rand.New(rand.NewSource(seed)), //nolint:gosec // deterministic model-based test
		now:           fuzzPropertyNow(),
		intentTTL:     30 * time.Minute,
		records:       make(map[string]state.NetworkContainerRecord),
		ipsByNC:       make(map[string]map[string]state.IPRecord),
		ncOrdinals:    make(map[string]int),
		ncRevisions:   make(map[string]int),
		assignments:   make(map[string]state.AssignmentRecord),
		owners:        make(map[string]string),
		endpoints:     make(map[string]state.EndpointRecord),
		deleteIntents: make(map[string]state.DeleteIntent),
	}
	t.Cleanup(func() {
		if machine.db != nil {
			require.NoError(t, machine.db.Close())
		}
	})

	for ncOrdinal := range 3 {
		ncID := fmt.Sprintf("nc-%d", ncOrdinal)
		machine.ncOrdinals[ncID] = ncOrdinal
		machine.applyNetworkContainer(ncID, initialStateMachineIPs(ncID, ncOrdinal))
	}
	machine.applyBoot(false, false)
	machine.assertState("initial state")
	return machine
}

func (m *randomizedStateMachine) exerciseRequiredTransitions() {
	m.t.Helper()
	containerID := "required-container"
	ipIDs := []string{"nc-0-v4-0", "nc-1-v6-0"}
	m.assign(containerID, ipIDs)
	m.assertState("cross-NC dual-stack multi-NIC assignment")

	m.release(containerID)
	m.assertState("release creates tombstone")

	m.assignExpectDeleteIntent(containerID, ipIDs)
	m.assertState("unexpired tombstone blocks assignment")

	m.now = m.deleteIntents[containerID].CreatedAt.Add(m.intentTTL)
	m.assign(containerID, ipIDs)
	m.assertState("expired tombstone permits assignment")

	next := cloneIPRecords(m.ipsByNC["nc-2"])
	next["nc-2-dynamic-v4"] = state.IPRecord{
		ID:        "nc-2-dynamic-v4",
		IPAddress: "10.3.10.20",
		NCID:      "nc-2",
		NCVersion: 3,
	}
	m.applyNetworkContainer("nc-2", next)
	delete(next, "nc-2-v4-1")
	m.applyNetworkContainer("nc-2", next)
	m.assertState("inventory add and remove")

	m.reopen()
	m.assertState("process reopen")
	m.applyBoot(false, true)
	m.assertState("boot change retains endpoints and resets readiness")
	m.applyBoot(true, false)
	m.assertState("boot change clears endpoints")
}

func (m *randomizedStateMachine) applyNetworkContainer(
	ncID string,
	ips map[string]state.IPRecord,
) {
	m.t.Helper()
	revision := m.ncRevisions[ncID] + 1
	record := stateMachineNCRecord(ncID, m.ncOrdinals[ncID], revision)
	require.NoError(m.t, m.db.ApplyNetworkContainer(m.ctx, record, ips))
	m.ncRevisions[ncID] = revision
	m.records[ncID] = state.NewNetworkContainerRecord(
		record.ID,
		record.VMVersion,
		record.HostVersion,
		record.VFPUpdateComplete,
		record.Request,
	)
	m.ipsByNC[ncID] = cloneIPRecords(ips)
}

func (m *randomizedStateMachine) assignRandom() {
	m.t.Helper()
	ipIDs := m.availableDualStackPair()
	if len(ipIDs) == 0 {
		m.addDynamicIP()
		ipIDs = m.availableDualStackPair()
	}
	if len(ipIDs) == 0 {
		return
	}
	containerID := fmt.Sprintf("container-%04d", m.nextContainer)
	m.nextContainer++
	m.assign(containerID, ipIDs)
}

func (m *randomizedStateMachine) assign(containerID string, ipIDs []string) {
	m.t.Helper()
	records := make([]state.IPRecord, 0, len(ipIDs))
	for _, ipID := range ipIDs {
		records = append(records, m.allIPs()[ipID])
	}
	slices.Sort(ipIDs)
	assignment := state.AssignmentRecord{
		Pod: state.PodIdentity{
			PodKey:           containerID,
			InfraContainerID: containerID,
			InterfaceID:      containerID,
			PodName:          "pod-" + containerID,
			PodNamespace:     "default",
		},
		IPIDs: slices.Clone(ipIDs),
	}
	endpoint := endpointForIPs(containerID, records)
	require.NoError(m.t, m.db.AssignEndpoint(
		m.ctx,
		assignment,
		endpoint,
		m.now,
		m.intentTTL,
	))
	delete(m.deleteIntents, containerID)
	m.assignments[containerID] = assignment
	m.endpoints[containerID] = endpoint
	for _, ipID := range ipIDs {
		m.owners[ipID] = containerID
	}
}

func (m *randomizedStateMachine) assignExpectDeleteIntent(containerID string, ipIDs []string) {
	m.t.Helper()
	records := make([]state.IPRecord, 0, len(ipIDs))
	for _, ipID := range ipIDs {
		records = append(records, m.allIPs()[ipID])
	}
	err := m.db.AssignEndpoint(
		m.ctx,
		state.AssignmentRecord{
			Pod: state.PodIdentity{
				PodKey:           containerID,
				InfraContainerID: containerID,
			},
			IPIDs: slices.Clone(ipIDs),
		},
		endpointForIPs(containerID, records),
		m.now,
		m.intentTTL,
	)
	require.ErrorIs(m.t, err, state.ErrDeleteIntent)
}

func (m *randomizedStateMachine) releaseRandom() {
	m.t.Helper()
	keys := sortedMapKeys(m.assignments)
	if len(keys) == 0 {
		m.assignRandom()
		return
	}
	m.release(keys[m.rng.Intn(len(keys))])
}

func (m *randomizedStateMachine) release(containerID string) {
	m.t.Helper()
	require.NoError(m.t, m.db.ReleaseEndpoint(
		m.ctx,
		containerID,
		containerID,
		state.DeleteIntent{CreatedAt: m.now},
		m.intentTTL,
	))
	m.pruneModelDeleteIntents()
	if assignment, ok := m.assignments[containerID]; ok {
		for _, ipID := range assignment.IPIDs {
			delete(m.owners, ipID)
		}
	}
	delete(m.assignments, containerID)
	delete(m.endpoints, containerID)
	m.deleteIntents[containerID] = state.DeleteIntent{CreatedAt: m.now}
}

func (m *randomizedStateMachine) retryRandomTombstone(expire bool) {
	m.t.Helper()
	keys := sortedMapKeys(m.deleteIntents)
	if len(keys) == 0 {
		m.releaseRandom()
		return
	}
	containerID := keys[m.rng.Intn(len(keys))]
	intent := m.deleteIntents[containerID]
	expiresAt := intent.CreatedAt.Add(m.intentTTL)
	if expire {
		if m.now.Before(expiresAt) {
			m.now = expiresAt
		}
		ipIDs := m.availableDualStackPair()
		if len(ipIDs) != 0 {
			m.assign(containerID, ipIDs)
		}
		return
	}
	if !m.now.Before(expiresAt) {
		return
	}
	ipIDs := m.availableDualStackPair()
	if len(ipIDs) != 0 {
		m.assignExpectDeleteIntent(containerID, ipIDs)
	}
}

func (m *randomizedStateMachine) pruneRandom() {
	m.t.Helper()
	m.now = m.now.Add(time.Duration(m.rng.Intn(61)) * time.Minute)
	require.NoError(m.t, m.db.PruneDeleteIntents(m.ctx, m.now, m.intentTTL))
	m.pruneModelDeleteIntents()
}

func (m *randomizedStateMachine) pruneModelDeleteIntents() {
	for containerID, intent := range m.deleteIntents {
		if !m.now.Before(intent.CreatedAt.Add(m.intentTTL)) {
			delete(m.deleteIntents, containerID)
		}
	}
}

func (m *randomizedStateMachine) mutateInventory() {
	m.t.Helper()
	ncIDs := sortedMapKeys(m.ipsByNC)
	ncID := ncIDs[m.rng.Intn(len(ncIDs))]
	next := cloneIPRecords(m.ipsByNC[ncID])

	owned := make([]string, 0)
	unowned := make([]string, 0)
	for ipID := range next {
		if _, ok := m.owners[ipID]; ok {
			owned = append(owned, ipID)
		} else {
			unowned = append(unowned, ipID)
		}
	}
	slices.Sort(owned)
	slices.Sort(unowned)

	if len(owned) != 0 && m.rng.Intn(5) == 0 {
		delete(next, owned[m.rng.Intn(len(owned))])
		revision := m.ncRevisions[ncID] + 1
		record := stateMachineNCRecord(ncID, m.ncOrdinals[ncID], revision)
		err := m.db.ApplyNetworkContainer(m.ctx, record, next)
		require.ErrorIs(m.t, err, state.ErrInconsistentState)
		return
	}
	if len(unowned) != 0 && m.rng.Intn(2) == 0 {
		delete(next, unowned[m.rng.Intn(len(unowned))])
		m.applyNetworkContainer(ncID, next)
		return
	}
	m.addDynamicIPToNC(ncID)
}

func (m *randomizedStateMachine) addDynamicIP() {
	m.t.Helper()
	ncIDs := sortedMapKeys(m.ipsByNC)
	m.addDynamicIPToNC(ncIDs[m.rng.Intn(len(ncIDs))])
}

func (m *randomizedStateMachine) addDynamicIPToNC(ncID string) {
	m.t.Helper()
	next := cloneIPRecords(m.ipsByNC[ncID])
	m.nextDynamicIP++
	ordinal := m.ncOrdinals[ncID]
	familyV4 := m.nextDynamicIP%2 == 0
	id := fmt.Sprintf("%s-dynamic-%04d", ncID, m.nextDynamicIP)
	address := fmt.Sprintf("fd00:%x:10::%x", ordinal+1, m.nextDynamicIP+20)
	if familyV4 {
		address = fmt.Sprintf("10.%d.%d.%d", ordinal+1, 10+m.nextDynamicIP/200, 20+m.nextDynamicIP%200)
	}
	next[id] = state.IPRecord{
		ID:        id,
		IPAddress: address,
		NCID:      ncID,
		NCVersion: m.ncRevisions[ncID] + 1,
	}
	m.applyNetworkContainer(ncID, next)
}

func (m *randomizedStateMachine) patchRandomEndpoint() {
	m.t.Helper()
	keys := sortedMapKeys(m.endpoints)
	if len(keys) == 0 {
		m.assignRandom()
		return
	}
	containerID := keys[m.rng.Intn(len(keys))]
	endpoint := cloneEndpoint(m.endpoints[containerID])
	endpoint.IfnameToIPMap["delegated0"].HNSEndpointID = fmt.Sprintf("hns-%d", m.nextContainer)
	require.NoError(m.t, m.db.PatchEndpoint(
		m.ctx,
		containerID,
		endpoint,
		m.now,
		m.intentTTL,
	))
	m.endpoints[containerID] = endpoint
}

func (m *randomizedStateMachine) applyBoot(clearEndpoints, resetReadiness bool) {
	m.t.Helper()
	m.bootSequence++
	bootID := fmt.Sprintf("boot-%d", m.bootSequence)
	changed, err := m.db.ApplyBoot(m.ctx, bootID, state.BootPolicy{
		ClearEndpoints:                 clearEndpoints,
		ResetNetworkContainerReadiness: resetReadiness,
	})
	require.NoError(m.t, err)
	require.True(m.t, changed)
	m.bootID = bootID
	clear(m.deleteIntents)
	if clearEndpoints {
		clear(m.assignments)
		clear(m.owners)
		clear(m.endpoints)
	} else {
		m.rebuildAssignmentsFromEndpoints()
	}
	if resetReadiness {
		for ncID := range m.records {
			record := m.records[ncID]
			record.HostVersion = "-1"
			record.VFPUpdateComplete = false
			m.records[ncID] = record
		}
	}
}

func (m *randomizedStateMachine) applyCurrentBoot() {
	m.t.Helper()
	before, err := m.db.Snapshot(m.ctx)
	require.NoError(m.t, err)
	changed, err := m.db.ApplyBoot(m.ctx, m.bootID, state.BootPolicy{
		ClearEndpoints:                 true,
		ResetNetworkContainerReadiness: true,
	})
	require.NoError(m.t, err)
	require.False(m.t, changed)
	after, err := m.db.Snapshot(m.ctx)
	require.NoError(m.t, err)
	require.Equal(m.t, before, after)
}

func (m *randomizedStateMachine) rebuildAssignmentsFromEndpoints() {
	clear(m.assignments)
	clear(m.owners)
	ipIDByAddress := make(map[string]string)
	for ipID, ip := range m.allIPs() {
		ipIDByAddress[ip.IPAddress] = ipID
	}
	for containerID, endpoint := range m.endpoints {
		ipIDs := make([]string, 0)
		for _, info := range endpoint.IfnameToIPMap {
			if info == nil || !info.NICType.IsInfraOrLegacy() {
				continue
			}
			for _, prefix := range append(info.IPv4, info.IPv6...) {
				ipIDs = append(ipIDs, ipIDByAddress[prefix.IP.String()])
			}
		}
		slices.Sort(ipIDs)
		if len(ipIDs) == 0 {
			continue
		}
		m.assignments[containerID] = state.AssignmentRecord{
			Pod: state.PodIdentity{
				PodKey:           containerID,
				InfraContainerID: containerID,
				InterfaceID:      containerID,
				PodName:          endpoint.PodName,
				PodNamespace:     endpoint.PodNamespace,
			},
			IPIDs: ipIDs,
		}
		for _, ipID := range ipIDs {
			m.owners[ipID] = containerID
		}
	}
}

func (m *randomizedStateMachine) reopen() {
	m.t.Helper()
	before, err := m.db.Snapshot(m.ctx)
	require.NoError(m.t, err)
	require.NoError(m.t, m.db.Close())
	m.db = nil

	reopened, err := state.Open(m.path, state.Options{NoSync: true})
	require.NoError(m.t, err)
	m.db = reopened
	after, err := m.db.Snapshot(m.ctx)
	require.NoError(m.t, err)
	require.Equal(m.t, before, after)
}

func (m *randomizedStateMachine) assertState(stage string) {
	m.t.Helper()
	snapshot, err := m.db.Snapshot(m.ctx)
	require.NoError(m.t, err, stage)
	require.Equal(m.t, m.bootID, snapshot.Metadata.BootID, stage)
	require.Equal(m.t, m.records, snapshot.NetworkContainers, stage)
	require.Equal(m.t, m.allIPs(), snapshot.IPs, stage)
	require.Equal(m.t, normalizeAssignments(m.assignments), normalizeAssignments(snapshot.Assignments), stage)
	require.Equal(m.t, m.owners, snapshot.IPOwners, stage)
	require.Equal(m.t, m.endpoints, snapshot.Endpoints, stage)
	require.Equal(m.t, m.deleteIntents, snapshot.DeleteIntents, stage)
	assertNoAuthorizationTokens(m.t, snapshot)
}

func (m *randomizedStateMachine) availableDualStackPair() []string {
	v4 := make([]string, 0)
	v6 := make([]string, 0)
	for ipID, ip := range m.allIPs() {
		if _, owned := m.owners[ipID]; owned {
			continue
		}
		address, err := netip.ParseAddr(ip.IPAddress)
		require.NoError(m.t, err)
		if address.Is4() {
			v4 = append(v4, ipID)
		} else {
			v6 = append(v6, ipID)
		}
	}
	slices.Sort(v4)
	slices.Sort(v6)
	if len(v4) == 0 || len(v6) == 0 {
		return nil
	}
	return []string{
		v4[m.rng.Intn(len(v4))],
		v6[m.rng.Intn(len(v6))],
	}
}

func (m *randomizedStateMachine) allIPs() map[string]state.IPRecord {
	result := make(map[string]state.IPRecord)
	for _, ips := range m.ipsByNC {
		for ipID, ip := range ips {
			result[ipID] = ip
		}
	}
	return result
}

func initialStateMachineIPs(ncID string, ncOrdinal int) map[string]state.IPRecord {
	result := make(map[string]state.IPRecord, 8)
	for index := range 4 {
		v4ID := fmt.Sprintf("%s-v4-%d", ncID, index)
		v6ID := fmt.Sprintf("%s-v6-%d", ncID, index)
		result[v4ID] = state.IPRecord{
			ID:        v4ID,
			IPAddress: fmt.Sprintf("10.%d.%d.%d", ncOrdinal+1, index+1, index+10),
			NCID:      ncID,
			NCVersion: 1,
		}
		result[v6ID] = state.IPRecord{
			ID:        v6ID,
			IPAddress: fmt.Sprintf("fd00:%x:%x::%x", ncOrdinal+1, index+1, index+10),
			NCID:      ncID,
			NCVersion: 1,
		}
	}
	return result
}

func stateMachineNCRecord(ncID string, ncOrdinal, revision int) state.NetworkContainerRecord {
	return state.NetworkContainerRecord{
		ID:                ncID,
		VMVersion:         strconv.Itoa(revision + 1),
		HostVersion:       strconv.Itoa(revision),
		VFPUpdateComplete: true,
		Request: cns.CreateNetworkContainerRequest{
			NetworkContainerid: ncID,
			Version:            strconv.Itoa(revision + 1),
			AuthorizationToken: stateMachineSecret,
			OrchestratorContext: json.RawMessage(
				fmt.Sprintf(`{"nc":%q}`, ncID),
			),
			IPConfiguration: fuzzIPConfiguration(
				fmt.Sprintf("10.%d.0.0", ncOrdinal+1),
				16,
			),
			IPv6Configuration: fuzzIPConfiguration(
				fmt.Sprintf("fd00:%x::", ncOrdinal+1),
				64,
			),
		},
	}
}

func cloneIPRecords(records map[string]state.IPRecord) map[string]state.IPRecord {
	result := make(map[string]state.IPRecord, len(records))
	for ipID, record := range records {
		result[ipID] = record
	}
	return result
}

func cloneEndpoint(endpoint state.EndpointRecord) state.EndpointRecord {
	result := state.EndpointRecord{
		PodName:       endpoint.PodName,
		PodNamespace:  endpoint.PodNamespace,
		IfnameToIPMap: make(map[string]*state.IPInfoRecord, len(endpoint.IfnameToIPMap)),
	}
	for ifName, info := range endpoint.IfnameToIPMap {
		if info == nil {
			result.IfnameToIPMap[ifName] = nil
			continue
		}
		cloned := *info
		cloned.IPv4 = slices.Clone(info.IPv4)
		cloned.IPv6 = slices.Clone(info.IPv6)
		result.IfnameToIPMap[ifName] = &cloned
	}
	return result
}

func normalizeAssignments(
	assignments map[string]state.AssignmentRecord,
) map[string]state.AssignmentRecord {
	result := make(map[string]state.AssignmentRecord, len(assignments))
	for podKey, assignment := range assignments {
		assignment.IPIDs = slices.Clone(assignment.IPIDs)
		slices.Sort(assignment.IPIDs)
		result[podKey] = assignment
	}
	return result
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
