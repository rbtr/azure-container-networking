// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package state

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"
)

type BootPolicy struct {
	ClearEndpoints                 bool
	ResetNetworkContainerReadiness bool
}

func (s *DB) SetManagedEndpointState(ctx context.Context, enabled bool) error {
	if enabled {
		return nil
	}
	return s.Update(ctx, func(tx *WriteTx) error {
		if err := tx.ClearAssignments(); err != nil {
			return err
		}
		if err := tx.ClearIPOwners(); err != nil {
			return err
		}
		if err := tx.ClearEndpoints(); err != nil {
			return err
		}
		return tx.ClearDeleteIntents()
	})
}

func (s *DB) ReplaceDurableState(ctx context.Context, snapshot Snapshot) error {
	return s.Update(ctx, func(tx *WriteTx) error {
		meta, metaErr := tx.Metadata()
		if metaErr != nil {
			return metaErr
		}
		if snapshot.Metadata.Generation != meta.Generation {
			return fmt.Errorf("%w: cache=%d database=%d", ErrStaleGeneration, snapshot.Metadata.Generation, meta.Generation)
		}

		existingIPs, ipsErr := tx.IPs()
		if ipsErr != nil {
			return ipsErr
		}
		for ipID := range existingIPs {
			if _, keep := snapshot.IPs[ipID]; keep {
				continue
			}
			if owner, ownerErr := tx.IPOwner(ipID); ownerErr == nil {
				return fmt.Errorf("%w: cannot remove IP %q owned by %q", ErrInconsistentState, ipID, owner)
			} else if !errors.Is(ownerErr, ErrNotFound) {
				return ownerErr
			}
		}

		if err := tx.ClearDurableState(); err != nil {
			return err
		}
		if err := tx.PutMetadata(snapshot.Metadata); err != nil {
			return err
		}
		for id := range snapshot.NetworkContainers {
			record := snapshot.NetworkContainers[id]
			record = NewNetworkContainerRecord(
				record.ID,
				record.VMVersion,
				record.HostVersion,
				record.VFPUpdateComplete,
				record.Request,
			)
			if err := tx.PutNetworkContainer(record); err != nil {
				return err
			}
		}
		for _, record := range snapshot.IPs {
			if err := tx.PutIP(record); err != nil {
				return err
			}
		}
		for _, record := range snapshot.Networks {
			if err := tx.PutNetwork(record); err != nil {
				return err
			}
		}
		for key, ncIDs := range snapshot.OrchestratorContexts {
			if err := tx.PutOrchestratorContext(key, ncIDs); err != nil {
				return err
			}
		}
		for mac, pnpID := range snapshot.PnPIDByMAC {
			if err := tx.PutPnPIDByMAC(mac, pnpID); err != nil {
				return err
			}
		}
		endpoints, endpointsErr := tx.Endpoints()
		if endpointsErr != nil {
			return endpointsErr
		}
		if len(endpoints) != 0 {
			next := NewSnapshot()
			next.IPs = snapshot.IPs
			next.Endpoints = endpoints
			if err := addAssignmentsFromEndpoints(&next); err != nil {
				return fmt.Errorf("rebuilding assignments after durable state update: %w", err)
			}
			if err := tx.ClearAssignments(); err != nil {
				return err
			}
			if err := tx.ClearIPOwners(); err != nil {
				return err
			}
			for _, assignment := range next.Assignments {
				if err := tx.PutAssignment(assignment); err != nil {
					return err
				}
			}
			for ipID, podKey := range next.IPOwners {
				if err := tx.PutIPOwner(ipID, podKey); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *DB) ReplaceManagedEndpoints(ctx context.Context, endpoints map[string]EndpointRecord) error {
	return s.Update(ctx, func(tx *WriteTx) error {
		ips, err := tx.IPs()
		if err != nil {
			return err
		}
		next := NewSnapshot()
		next.IPs = ips
		next.Endpoints = endpoints
		if len(ips) != 0 {
			if err := addAssignmentsFromEndpoints(&next); err != nil {
				return err
			}
		}

		if err := tx.ClearAssignments(); err != nil {
			return err
		}
		if err := tx.ClearIPOwners(); err != nil {
			return err
		}
		if err := tx.ClearEndpoints(); err != nil {
			return err
		}
		for containerID, endpoint := range next.Endpoints {
			if err := tx.PutEndpoint(containerID, endpoint); err != nil {
				return err
			}
		}
		for _, assignment := range next.Assignments {
			if err := tx.PutAssignment(assignment); err != nil {
				return err
			}
		}
		for ipID, podKey := range next.IPOwners {
			if err := tx.PutIPOwner(ipID, podKey); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *DB) ApplyBoot(ctx context.Context, bootID string, policy BootPolicy) (bool, error) {
	if bootID == "" {
		return false, fmt.Errorf("%w: boot ID is empty", ErrInvalidInput)
	}

	var currentBootID string
	if err := s.View(ctx, func(tx *ReadTx) error {
		meta, err := tx.Metadata()
		if err != nil {
			return err
		}
		currentBootID = meta.BootID
		return nil
	}); err != nil {
		return false, err
	}
	if currentBootID == bootID {
		return false, nil
	}

	changed := false
	err := s.Update(ctx, func(tx *WriteTx) error {
		meta, err := tx.Metadata()
		if err != nil {
			return err
		}
		if meta.BootID == bootID {
			return nil
		}

		changed = true
		meta.BootID = bootID
		if err := tx.PutMetadata(meta); err != nil {
			return err
		}
		if err := tx.ClearAssignments(); err != nil {
			return err
		}
		if err := tx.ClearIPOwners(); err != nil {
			return err
		}
		if err := tx.ClearDeleteIntents(); err != nil {
			return err
		}
		if policy.ClearEndpoints {
			if err := tx.ClearEndpoints(); err != nil {
				return err
			}
		} else {
			ips, err := tx.IPs()
			if err != nil {
				return err
			}
			endpoints, err := tx.Endpoints()
			if err != nil {
				return err
			}
			next := NewSnapshot()
			next.IPs = ips
			next.Endpoints = endpoints
			if err := addAssignmentsFromEndpoints(&next); err != nil {
				return fmt.Errorf("rebuilding boot assignments from retained endpoints: %w", err)
			}
			for _, assignment := range next.Assignments {
				if err := tx.PutAssignment(assignment); err != nil {
					return err
				}
			}
			for ipID, podKey := range next.IPOwners {
				if err := tx.PutIPOwner(ipID, podKey); err != nil {
					return err
				}
			}
		}

		if policy.ResetNetworkContainerReadiness {
			records, err := tx.NetworkContainers()
			if err != nil {
				return err
			}
			for id := range records {
				record := records[id]
				record.HostVersion = "-1"
				record.VFPUpdateComplete = false
				if err := tx.PutNetworkContainer(record); err != nil {
					return fmt.Errorf("resetting NC %q readiness: %w", id, err)
				}
			}
		}
		return nil
	})
	return changed, err
}

func (s *DB) ApplyNetworkContainer(
	ctx context.Context,
	record NetworkContainerRecord,
	ips map[string]IPRecord,
) error {
	if record.ID == "" {
		return fmt.Errorf("%w: NC ID is empty", ErrInvalidInput)
	}
	record = NewNetworkContainerRecord(
		record.ID,
		record.VMVersion,
		record.HostVersion,
		record.VFPUpdateComplete,
		record.Request,
	)

	for ipID, ip := range ips {
		if ipID == "" || ip.ID != ipID {
			return fmt.Errorf("%w: IP key %q does not match record ID %q", ErrInconsistentState, ipID, ip.ID)
		}
		if ip.NCID != record.ID {
			return fmt.Errorf("%w: IP %q belongs to NC %q, expected %q", ErrInconsistentState, ipID, ip.NCID, record.ID)
		}
		if _, err := netip.ParseAddr(ip.IPAddress); err != nil {
			return fmt.Errorf("cns state: parsing IP %q address %q: %w", ipID, ip.IPAddress, err)
		}
	}

	return s.Update(ctx, func(tx *WriteTx) error {
		existing, err := tx.IPs()
		if err != nil {
			return err
		}
		for ipID, ip := range existing {
			if ip.NCID != record.ID {
				continue
			}
			if _, keep := ips[ipID]; keep {
				continue
			}
			if owner, ownerErr := tx.IPOwner(ipID); ownerErr == nil {
				return fmt.Errorf("%w: cannot remove IP %q owned by %q", ErrInconsistentState, ipID, owner)
			} else if !errors.Is(ownerErr, ErrNotFound) {
				return ownerErr
			}
		}

		if err := tx.PutNetworkContainer(record); err != nil {
			return err
		}
		for ipID, ip := range existing {
			if ip.NCID == record.ID {
				if _, keep := ips[ipID]; !keep {
					if err := tx.DeleteIP(ipID); err != nil {
						return err
					}
				}
			}
		}
		for _, ip := range ips {
			if err := tx.PutIP(ip); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *DB) DeleteNetworkContainer(ctx context.Context, ncID string) error {
	return s.Update(ctx, func(tx *WriteTx) error {
		ips, err := tx.IPs()
		if err != nil {
			return err
		}
		for ipID, ip := range ips {
			if ip.NCID != ncID {
				continue
			}
			if owner, ownerErr := tx.IPOwner(ipID); ownerErr == nil {
				return fmt.Errorf("%w: cannot delete NC %q with IP %q owned by %q", ErrInconsistentState, ncID, ipID, owner)
			} else if !errors.Is(ownerErr, ErrNotFound) {
				return ownerErr
			}
		}
		for ipID, ip := range ips {
			if ip.NCID == ncID {
				if err := tx.DeleteIP(ipID); err != nil {
					return err
				}
			}
		}
		return tx.DeleteNetworkContainer(ncID)
	})
}

func (s *DB) AssignEndpoint(
	ctx context.Context,
	assignment AssignmentRecord,
	endpoint EndpointRecord,
	now time.Time,
	intentTTL time.Duration,
) error {
	if assignment.Pod.PodKey == "" || assignment.Pod.InfraContainerID == "" {
		return fmt.Errorf("%w: assignment pod key and infra container ID are required", ErrInvalidInput)
	}
	if len(assignment.IPIDs) == 0 {
		return fmt.Errorf("%w: assignment has no IP IDs", ErrInvalidInput)
	}
	seenIPIDs := make(map[string]struct{}, len(assignment.IPIDs))
	for _, ipID := range assignment.IPIDs {
		if _, duplicate := seenIPIDs[ipID]; duplicate {
			return fmt.Errorf("%w: assignment %q contains duplicate IP ID %q", ErrInconsistentState, assignment.Pod.PodKey, ipID)
		}
		seenIPIDs[ipID] = struct{}{}
	}

	return s.Update(ctx, func(tx *WriteTx) error {
		intent, intentErr := tx.DeleteIntent(assignment.Pod.InfraContainerID)
		switch {
		case intentErr == nil && !deleteIntentExpired(intent, now, intentTTL):
			return fmt.Errorf("%w for infra container %q", ErrDeleteIntent, assignment.Pod.InfraContainerID)
		case intentErr == nil:
			if err := tx.DeleteDeleteIntent(assignment.Pod.InfraContainerID); err != nil {
				return err
			}
		case !errors.Is(intentErr, ErrNotFound):
			return intentErr
		}

		if existing, existingErr := tx.Assignment(assignment.Pod.PodKey); existingErr == nil {
			for _, oldIPID := range existing.IPIDs {
				if !slices.Contains(assignment.IPIDs, oldIPID) {
					if err := tx.DeleteIPOwner(oldIPID); err != nil {
						return err
					}
				}
			}
		} else if !errors.Is(existingErr, ErrNotFound) {
			return existingErr
		}

		for _, ipID := range assignment.IPIDs {
			ip, err := tx.IP(ipID)
			if err != nil {
				return err
			}
			if !endpointContainsInfraIP(endpoint, ip.IPAddress) {
				return fmt.Errorf("%w: endpoint %q does not contain IP %q", ErrInconsistentState, assignment.Pod.InfraContainerID, ip.IPAddress)
			}
			owner, ownerErr := tx.IPOwner(ipID)
			if ownerErr == nil && owner != assignment.Pod.PodKey {
				return fmt.Errorf("%w: IP %q is owned by %q", ErrIPAlreadyAssigned, ipID, owner)
			}
			if ownerErr != nil && !errors.Is(ownerErr, ErrNotFound) {
				return ownerErr
			}
		}

		for _, ipID := range assignment.IPIDs {
			if err := tx.PutIPOwner(ipID, assignment.Pod.PodKey); err != nil {
				return err
			}
		}
		if err := tx.PutAssignment(assignment); err != nil {
			return err
		}
		return tx.PutEndpoint(assignment.Pod.InfraContainerID, endpoint)
	})
}

func (s *DB) ReleaseEndpoint(
	ctx context.Context,
	podKey, infraContainerID string,
	intent DeleteIntent,
	intentTTL time.Duration,
) error {
	if infraContainerID == "" {
		return fmt.Errorf("%w: infra container ID is required", ErrInvalidInput)
	}
	if intent.CreatedAt.IsZero() {
		return fmt.Errorf("%w: delete intent timestamp is required", ErrInvalidInput)
	}

	return s.Update(ctx, func(tx *WriteTx) error {
		intents, intentsErr := tx.DeleteIntents()
		if intentsErr != nil {
			return intentsErr
		}
		for containerID, existing := range intents {
			if deleteIntentExpired(existing, intent.CreatedAt, intentTTL) {
				if err := tx.DeleteDeleteIntent(containerID); err != nil {
					return err
				}
			}
		}
		if err := tx.PutDeleteIntent(infraContainerID, intent); err != nil {
			return err
		}

		assignment, assignmentErr := tx.Assignment(podKey)
		if assignmentErr == nil {
			for _, ipID := range assignment.IPIDs {
				if err := tx.DeleteIPOwner(ipID); err != nil {
					return err
				}
			}
			if err := tx.DeleteAssignment(podKey); err != nil {
				return err
			}
		} else if !errors.Is(assignmentErr, ErrNotFound) {
			return assignmentErr
		}

		return tx.DeleteEndpoint(infraContainerID)
	})
}

func (s *DB) DeleteEndpointRecord(ctx context.Context, infraContainerID string) error {
	return s.Update(ctx, func(tx *WriteTx) error {
		assignments, err := tx.Assignments()
		if err != nil {
			return err
		}
		for podKey, assignment := range assignments {
			if assignment.Pod.InfraContainerID == infraContainerID {
				return fmt.Errorf("%w: endpoint %q is referenced by assignment %q", ErrInconsistentState, infraContainerID, podKey)
			}
		}
		return tx.DeleteEndpoint(infraContainerID)
	})
}

func (s *DB) PatchEndpoint(
	ctx context.Context,
	infraContainerID string,
	endpoint EndpointRecord,
	now time.Time,
	intentTTL time.Duration,
) error {
	return s.Update(ctx, func(tx *WriteTx) error {
		intent, intentErr := tx.DeleteIntent(infraContainerID)
		switch {
		case intentErr == nil && !deleteIntentExpired(intent, now, intentTTL):
			return fmt.Errorf("%w for infra container %q", ErrDeleteIntent, infraContainerID)
		case intentErr == nil:
			if err := tx.DeleteDeleteIntent(infraContainerID); err != nil {
				return err
			}
		case !errors.Is(intentErr, ErrNotFound):
			return intentErr
		}
		return tx.PutEndpoint(infraContainerID, endpoint)
	})
}

func (s *DB) PruneDeleteIntents(ctx context.Context, now time.Time, ttl time.Duration) error {
	return s.Update(ctx, func(tx *WriteTx) error {
		intents, err := tx.DeleteIntents()
		if err != nil {
			return err
		}
		for containerID, intent := range intents {
			if deleteIntentExpired(intent, now, ttl) {
				if err := tx.DeleteDeleteIntent(containerID); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func deleteIntentExpired(intent DeleteIntent, now time.Time, ttl time.Duration) bool {
	return intent.CreatedAt.IsZero() || !now.Before(intent.CreatedAt.Add(ttl))
}
