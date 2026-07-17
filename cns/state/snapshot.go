// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package state

import (
	"context"
	"fmt"
	"net"
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

	for ncID := range s.NetworkContainers {
		if err := validateNetworkContainerRecord(ncID, s.NetworkContainers[ncID], ErrInconsistentState); err != nil {
			return err
		}
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

	seenEndpointIPs := make(map[string]string)
	for containerID, endpoint := range s.Endpoints {
		if err := validateEndpointRecord(containerID, endpoint, ErrInconsistentState); err != nil {
			return err
		}
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

	return nil
}

func validateNetworkContainerRecord(ncID string, record NetworkContainerRecord, sentinel error) error {
	if ncID == "" || record.ID == "" {
		return fmt.Errorf("%w: empty NC ID", sentinel)
	}
	if record.ID != ncID {
		return fmt.Errorf("%w: NC key %q does not match record ID %q", sentinel, ncID, record.ID)
	}

	prefixes := []struct {
		name    string
		address string
		bits    uint8
	}{
		{
			name:    "local IPv4 subnet",
			address: record.Request.LocalIPConfiguration.IPSubnet.IPAddress,
			bits:    record.Request.LocalIPConfiguration.IPSubnet.PrefixLength,
		},
		{
			name:    "local IPv6 subnet",
			address: record.Request.LocalIPConfiguration.IPSubnetV6.IPAddress,
			bits:    record.Request.LocalIPConfiguration.IPSubnetV6.PrefixLength,
		},
		{
			name:    "IPv4 subnet",
			address: record.Request.IPConfiguration.IPSubnet.IPAddress,
			bits:    record.Request.IPConfiguration.IPSubnet.PrefixLength,
		},
		{
			name:    "IPv6 subnet",
			address: record.Request.IPConfiguration.IPSubnetV6.IPAddress,
			bits:    record.Request.IPConfiguration.IPSubnetV6.PrefixLength,
		},
		{
			name:    "secondary IPv6 subnet",
			address: record.Request.IPv6Configuration.IPSubnet.IPAddress,
			bits:    record.Request.IPv6Configuration.IPSubnet.PrefixLength,
		},
		{
			name:    "secondary IPv6 subnet v6",
			address: record.Request.IPv6Configuration.IPSubnetV6.IPAddress,
			bits:    record.Request.IPv6Configuration.IPSubnetV6.PrefixLength,
		},
	}
	for _, prefix := range prefixes {
		if err := validatePrefix(prefix.address, prefix.bits); err != nil {
			return fmt.Errorf("%w: %s has invalid prefix: %w", sentinel, prefix.name, err)
		}
	}
	for i, prefix := range record.Request.CnetAddressSpace {
		if err := validatePrefix(prefix.IPAddress, prefix.PrefixLength); err != nil {
			return fmt.Errorf("%w: cnet address space %d has invalid prefix: %w", sentinel, i, err)
		}
	}
	return nil
}

func validatePrefix(address string, bits uint8) error {
	if address == "" && bits == 0 {
		return nil
	}
	if _, err := netip.ParsePrefix(fmt.Sprintf("%s/%d", address, bits)); err != nil {
		return fmt.Errorf("parse prefix: %w", err)
	}
	return nil
}

func validateEndpointRecord(containerID string, endpoint EndpointRecord, sentinel error) error {
	if containerID == "" {
		return fmt.Errorf("%w: empty endpoint ID", sentinel)
	}
	for ifName, ipInfo := range endpoint.IfnameToIPMap {
		if ipInfo == nil {
			continue
		}
		for _, prefix := range ipInfo.IPv4 {
			if err := validateEndpointPrefix(prefix, true, sentinel); err != nil {
				return fmt.Errorf("endpoint %q interface %q: %w", containerID, ifName, err)
			}
		}
		for _, prefix := range ipInfo.IPv6 {
			if err := validateEndpointPrefix(prefix, false, sentinel); err != nil {
				return fmt.Errorf("endpoint %q interface %q: %w", containerID, ifName, err)
			}
		}
	}
	return nil
}

func validateEndpointPrefix(prefix net.IPNet, ipv4 bool, sentinel error) error {
	if prefix.IP.To16() == nil {
		if ipv4 {
			return fmt.Errorf("%w: invalid IPv4 address", sentinel)
		}
		return fmt.Errorf("%w: invalid IPv6 address", sentinel)
	}
	if ipv4 && prefix.IP.To4() == nil {
		return fmt.Errorf("%w: IPv6 address in IPv4 collection", sentinel)
	}
	if !ipv4 && prefix.IP.To4() != nil {
		return fmt.Errorf("%w: IPv4 address in IPv6 collection", sentinel)
	}

	_, bits := prefix.Mask.Size()
	wantBits := net.IPv6len * 8
	family := "IPv6"
	if ipv4 {
		wantBits = net.IPv4len * 8
		family = "IPv4"
	}
	if bits != wantBits {
		return fmt.Errorf("%w: invalid %s prefix", sentinel, family)
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
