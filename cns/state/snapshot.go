// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package state

import (
	"context"
	"fmt"
	"net/netip"
)

func (s *DB) Snapshot(ctx context.Context) (Snapshot, error) {
	snapshot := NewSnapshot()
	err := s.View(ctx, func(tx *ReadTx) error {
		var err error
		if snapshot.Metadata, err = tx.Metadata(); err != nil {
			return err
		}
		if snapshot.NetworkContainers, err = tx.NetworkContainers(); err != nil {
			return err
		}
		if snapshot.IPs, err = tx.IPs(); err != nil {
			return err
		}
		if snapshot.Networks, err = tx.Networks(); err != nil {
			return err
		}
		if snapshot.OrchestratorContexts, err = tx.OrchestratorContexts(); err != nil {
			return err
		}
		if snapshot.PnPIDByMAC, err = tx.PnPIDByMAC(); err != nil {
			return err
		}
		if snapshot.Assignments, err = tx.Assignments(); err != nil {
			return err
		}
		if snapshot.IPOwners, err = tx.IPOwners(); err != nil {
			return err
		}
		if snapshot.Endpoints, err = tx.Endpoints(); err != nil {
			return err
		}
		snapshot.DeleteIntents, err = tx.DeleteIntents()
		return err
	})
	if err != nil {
		return Snapshot{}, err
	}
	if err := snapshot.Validate(); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (s Snapshot) Validate() error {
	if s.Metadata.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: database=%d code=%d", ErrSchemaMismatch, s.Metadata.SchemaVersion, SchemaVersion)
	}

	for ipID, ip := range s.IPs {
		if ip.ID != ipID {
			return fmt.Errorf("%w: IP key %q does not match record ID %q", ErrInconsistentState, ipID, ip.ID)
		}
		if _, ok := s.NetworkContainers[ip.NCID]; !ok {
			return fmt.Errorf("%w: IP %q references missing NC %q", ErrInconsistentState, ipID, ip.NCID)
		}
		if _, err := netip.ParseAddr(ip.IPAddress); err != nil {
			return fmt.Errorf("%w: IP %q has invalid address %q: %w", ErrInconsistentState, ipID, ip.IPAddress, err)
		}
	}

	for podKey, assignment := range s.Assignments {
		if assignment.Pod.PodKey != podKey {
			return fmt.Errorf("%w: assignment key %q does not match pod key %q", ErrInconsistentState, podKey, assignment.Pod.PodKey)
		}
		if assignment.Pod.InfraContainerID == "" {
			return fmt.Errorf("%w: assignment %q has empty infra container ID", ErrInconsistentState, podKey)
		}
		endpoint, ok := s.Endpoints[assignment.Pod.InfraContainerID]
		if !ok {
			return fmt.Errorf("%w: assignment %q references missing endpoint %q", ErrInconsistentState, podKey, assignment.Pod.InfraContainerID)
		}

		seen := make(map[string]struct{}, len(assignment.IPIDs))
		for _, ipID := range assignment.IPIDs {
			if _, duplicate := seen[ipID]; duplicate {
				return fmt.Errorf("%w: assignment %q contains duplicate IP ID %q", ErrInconsistentState, podKey, ipID)
			}
			seen[ipID] = struct{}{}

			ip, ok := s.IPs[ipID]
			if !ok {
				return fmt.Errorf("%w: assignment %q references missing IP %q", ErrInconsistentState, podKey, ipID)
			}
			owner, ok := s.IPOwners[ipID]
			if !ok || owner != podKey {
				return fmt.Errorf("%w: IP owner for %q is %q, expected %q", ErrInconsistentState, ipID, owner, podKey)
			}
			if !endpointContainsInfraIP(endpoint, ip.IPAddress) {
				return fmt.Errorf("%w: endpoint %q does not contain assigned IP %q", ErrInconsistentState, assignment.Pod.InfraContainerID, ip.IPAddress)
			}
		}
	}

	for ipID, podKey := range s.IPOwners {
		assignment, ok := s.Assignments[podKey]
		if !ok {
			return fmt.Errorf("%w: IP owner %q references missing assignment %q", ErrInconsistentState, ipID, podKey)
		}
		if !containsString(assignment.IPIDs, ipID) {
			return fmt.Errorf("%w: assignment %q does not contain owned IP %q", ErrInconsistentState, podKey, ipID)
		}
	}

	seenEndpointIPs := make(map[string]string)
	for containerID, endpoint := range s.Endpoints {
		for _, ipInfo := range endpoint.IfnameToIPMap {
			if ipInfo == nil || !ipInfo.NICType.IsInfraOrLegacy() {
				continue
			}
			for _, ipNet := range append(ipInfo.IPv4, ipInfo.IPv6...) {
				ip := ipNet.IP.String()
				if previous, exists := seenEndpointIPs[ip]; exists && previous != containerID {
					return fmt.Errorf("%w: endpoint IP %q owned by %q and %q", ErrInconsistentState, ip, previous, containerID)
				}
				seenEndpointIPs[ip] = containerID
			}
		}
	}

	return nil
}

func endpointContainsInfraIP(endpoint EndpointRecord, address string) bool {
	for _, ipInfo := range endpoint.IfnameToIPMap {
		if ipInfo == nil || !ipInfo.NICType.IsInfraOrLegacy() {
			continue
		}
		for _, ipNet := range ipInfo.IPv4 {
			if ipNet.IP.String() == address {
				return true
			}
		}
		for _, ipNet := range ipInfo.IPv6 {
			if ipNet.IP.String() == address {
				return true
			}
		}
	}
	return false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
