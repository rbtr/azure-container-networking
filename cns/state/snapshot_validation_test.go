// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//nolint:goconst // Repeated values keep state fixtures readable.
package state_test

import (
	"net"
	"testing"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/state"
	"github.com/stretchr/testify/require"
)

func validStateSnapshot() state.Snapshot {
	snapshot := state.NewSnapshot()
	snapshot.Metadata = state.Metadata{
		SchemaVersion: state.SchemaVersion,
		Authority:     state.AuthorityBolt,
	}
	snapshot.NetworkContainers[testNCID] = sampleNCRecord()
	snapshot.IPs[testIPID] = sampleIPs()[testIPID]
	snapshot.Endpoints[testContainerID] = sampleEndpoint(testIPAddress)
	snapshot.Assignments[testContainerID] = state.AssignmentRecord{
		Pod: state.PodIdentity{
			PodKey:           testContainerID,
			InfraContainerID: testContainerID,
		},
		IPIDs: []string{testIPID},
	}
	snapshot.IPOwners[testIPID] = testContainerID
	return snapshot
}

func TestSnapshotValidateRejectsInvalidState(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*state.Snapshot)
		wantErr     error
		wantMessage string
	}{
		{
			name: "schema version",
			mutate: func(snapshot *state.Snapshot) {
				snapshot.Metadata.SchemaVersion++
			},
			wantErr:     state.ErrSchemaMismatch,
			wantMessage: "schema version mismatch",
		},
		{
			name: "empty NC ID",
			mutate: func(snapshot *state.Snapshot) {
				record := snapshot.NetworkContainers[testNCID]
				delete(snapshot.NetworkContainers, testNCID)
				record.ID = ""
				snapshot.NetworkContainers[""] = record
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "empty NC ID",
		},
		{
			name: "NC key does not match record ID",
			mutate: func(snapshot *state.Snapshot) {
				record := snapshot.NetworkContainers[testNCID]
				delete(snapshot.NetworkContainers, testNCID)
				snapshot.NetworkContainers["different"] = record
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "does not match record ID",
		},
		{
			name: "invalid NC prefix",
			mutate: func(snapshot *state.Snapshot) {
				record := snapshot.NetworkContainers[testNCID]
				record.Request.IPConfiguration.IPSubnet.IPAddress = "10.0.0.0"
				record.Request.IPConfiguration.IPSubnet.PrefixLength = 64
				snapshot.NetworkContainers[testNCID] = record
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "invalid prefix",
		},
		{
			name: "invalid cnet address prefix",
			mutate: func(snapshot *state.Snapshot) {
				record := snapshot.NetworkContainers[testNCID]
				record.Request.CnetAddressSpace = []cns.IPSubnet{{
					IPAddress:    "10.0.0.0",
					PrefixLength: 64,
				}}
				snapshot.NetworkContainers[testNCID] = record
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "cnet address space",
		},
		{
			name: "IP key does not match record ID",
			mutate: func(snapshot *state.Snapshot) {
				record := snapshot.IPs[testIPID]
				delete(snapshot.IPs, testIPID)
				snapshot.IPs["different"] = record
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "does not match record ID",
		},
		{
			name: "IP references missing NC",
			mutate: func(snapshot *state.Snapshot) {
				delete(snapshot.NetworkContainers, testNCID)
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "references missing NC",
		},
		{
			name: "invalid IP address",
			mutate: func(snapshot *state.Snapshot) {
				record := snapshot.IPs[testIPID]
				record.IPAddress = "not-an-ip"
				snapshot.IPs[testIPID] = record
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "invalid address",
		},
		{
			name: "empty endpoint ID",
			mutate: func(snapshot *state.Snapshot) {
				snapshot.Endpoints[""] = sampleEndpoint("10.0.0.5")
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "empty endpoint ID",
		},
		{
			name: "invalid IPv4 prefix",
			mutate: func(snapshot *state.Snapshot) {
				endpoint := sampleEndpoint("10.0.0.5")
				endpoint.IfnameToIPMap[testIfName].IPv4[0].Mask = net.IPMask{0xff, 0, 0xff, 0}
				snapshot.Endpoints["invalid-prefix"] = endpoint
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "invalid IPv4 prefix",
		},
		{
			name: "invalid IPv4 address",
			mutate: func(snapshot *state.Snapshot) {
				snapshot.Endpoints["invalid-address"] = state.EndpointRecord{
					IfnameToIPMap: map[string]*state.IPInfoRecord{
						testIfName: {
							IPv4: []net.IPNet{{
								IP:   net.IP{1, 2},
								Mask: net.CIDRMask(24, 32),
							}},
						},
					},
				}
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "invalid IPv4 address",
		},
		{
			name: "IPv6 address in IPv4 collection",
			mutate: func(snapshot *state.Snapshot) {
				endpoint := sampleEndpoint("10.0.0.5")
				endpoint.IfnameToIPMap[testIfName].IPv4[0] = net.IPNet{
					IP:   net.ParseIP("2001:db8::5"),
					Mask: net.CIDRMask(64, 128),
				}
				snapshot.Endpoints["wrong-family"] = endpoint
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "IPv4 collection",
		},
		{
			name: "invalid IPv6 prefix",
			mutate: func(snapshot *state.Snapshot) {
				endpoint := state.EndpointRecord{
					IfnameToIPMap: map[string]*state.IPInfoRecord{
						testIfName: {
							IPv6: []net.IPNet{{
								IP:   net.ParseIP("2001:db8::5"),
								Mask: net.IPMask{0xff, 0, 0xff},
							}},
						},
					},
				}
				snapshot.Endpoints["invalid-v6-prefix"] = endpoint
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "invalid IPv6 prefix",
		},
		{
			name: "invalid IPv6 address",
			mutate: func(snapshot *state.Snapshot) {
				snapshot.Endpoints["invalid-v6-address"] = state.EndpointRecord{
					IfnameToIPMap: map[string]*state.IPInfoRecord{
						testIfName: {
							IPv6: []net.IPNet{{
								IP:   net.IP{1, 2},
								Mask: net.CIDRMask(64, 128),
							}},
						},
					},
				}
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "invalid IPv6 address",
		},
		{
			name: "IPv4 address in IPv6 collection",
			mutate: func(snapshot *state.Snapshot) {
				snapshot.Endpoints["wrong-v6-family"] = state.EndpointRecord{
					IfnameToIPMap: map[string]*state.IPInfoRecord{
						testIfName: {
							IPv6: []net.IPNet{{
								IP:   net.ParseIP("10.0.0.5"),
								Mask: net.CIDRMask(24, 32),
							}},
						},
					},
				}
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "IPv4 address in IPv6 collection",
		},
		{
			name: "assignment key does not match pod key",
			mutate: func(snapshot *state.Snapshot) {
				assignment := snapshot.Assignments[testContainerID]
				delete(snapshot.Assignments, testContainerID)
				snapshot.Assignments["different"] = assignment
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "does not match pod key",
		},
		{
			name: "empty infra container ID",
			mutate: func(snapshot *state.Snapshot) {
				assignment := snapshot.Assignments[testContainerID]
				assignment.Pod.InfraContainerID = ""
				snapshot.Assignments[testContainerID] = assignment
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "empty infra container ID",
		},
		{
			name: "assignment references missing endpoint",
			mutate: func(snapshot *state.Snapshot) {
				delete(snapshot.Endpoints, testContainerID)
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "references missing endpoint",
		},
		{
			name: "duplicate assignment IP ID",
			mutate: func(snapshot *state.Snapshot) {
				assignment := snapshot.Assignments[testContainerID]
				assignment.IPIDs = append(assignment.IPIDs, testIPID)
				snapshot.Assignments[testContainerID] = assignment
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "duplicate IP ID",
		},
		{
			name: "assignment references missing IP",
			mutate: func(snapshot *state.Snapshot) {
				assignment := snapshot.Assignments[testContainerID]
				assignment.IPIDs = []string{"missing"}
				snapshot.Assignments[testContainerID] = assignment
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "references missing IP",
		},
		{
			name: "IP owner does not match assignment",
			mutate: func(snapshot *state.Snapshot) {
				snapshot.IPOwners[testIPID] = "different"
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "expected",
		},
		{
			name: "endpoint does not contain assigned IP",
			mutate: func(snapshot *state.Snapshot) {
				snapshot.Endpoints[testContainerID] = sampleEndpoint("10.0.0.5")
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "does not contain assigned IP",
		},
		{
			name: "IP owner references missing assignment",
			mutate: func(snapshot *state.Snapshot) {
				delete(snapshot.Assignments, testContainerID)
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "references missing assignment",
		},
		{
			name: "assignment does not contain owned IP",
			mutate: func(snapshot *state.Snapshot) {
				assignment := snapshot.Assignments[testContainerID]
				assignment.IPIDs = nil
				snapshot.Assignments[testContainerID] = assignment
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "does not contain owned IP",
		},
		{
			name: "endpoint IP has multiple owners",
			mutate: func(snapshot *state.Snapshot) {
				snapshot.Endpoints["container-2"] = sampleEndpoint(testIPAddress)
			},
			wantErr:     state.ErrInconsistentState,
			wantMessage: "owned by",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := validStateSnapshot()
			tt.mutate(&snapshot)

			err := snapshot.Validate()
			require.ErrorIs(t, err, tt.wantErr)
			require.ErrorContains(t, err, tt.wantMessage)
		})
	}
}

func TestSnapshotValidateAllowsNilAndIPv6EndpointData(t *testing.T) {
	snapshot := state.NewSnapshot()
	snapshot.Metadata = state.Metadata{
		SchemaVersion: state.SchemaVersion,
		Authority:     state.AuthorityBolt,
	}
	snapshot.NetworkContainers[testNCID] = sampleNCRecord()
	snapshot.IPs[testIPID] = state.IPRecord{
		ID:        testIPID,
		IPAddress: "2001:db8::4",
		NCID:      testNCID,
	}
	snapshot.Endpoints[testContainerID] = state.EndpointRecord{
		IfnameToIPMap: map[string]*state.IPInfoRecord{
			"nil": nil,
			testIfName: {
				IPv6: []net.IPNet{{
					IP:   net.ParseIP("2001:db8::4"),
					Mask: net.CIDRMask(64, 128),
				}},
			},
		},
	}
	snapshot.Assignments[testContainerID] = testAssignment(testIPID)
	snapshot.IPOwners[testIPID] = testContainerID

	require.NoError(t, snapshot.Validate())
}
