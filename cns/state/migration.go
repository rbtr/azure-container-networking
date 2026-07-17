// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/wireserver"
	"github.com/Azure/azure-container-networking/platform"
)

const (
	legacyCNSStoreKey               = "ContainerNetworkService"
	legacyEndpointStoreKey          = "Endpoints"
	legacyDeleteIntentStoreKey      = "EndpointDeleteIntents"
	defaultDeleteIntentMigrationTTL = 24 * time.Hour
)

var (
	errLegacyMigrationAlreadyComplete = errors.New("legacy migration already complete")
	errLegacyStateNotObject           = errors.New("legacy state must be a JSON object")
	errMalformedLegacyNCList          = errors.New("malformed legacy network container list")
)

type ImportOptions struct {
	CNSJSONPath         string
	EndpointJSONPath    string
	ManageEndpointState bool
	BootID              string
	Now                 time.Time
	DeleteIntentTTL     time.Duration
}

type legacyCNSState struct {
	Location                         string                           `json:"Location"`
	NetworkType                      string                           `json:"NetworkType"`
	OrchestratorType                 string                           `json:"OrchestratorType"`
	NodeID                           string                           `json:"NodeID"`
	Initialized                      bool                             `json:"Initialized"`
	ContainerIDByOrchestratorContext map[string]legacyNCList          `json:"ContainerIDByOrchestratorContext"`
	ContainerStatus                  map[string]legacyContainerStatus `json:"ContainerStatus"`
	Networks                         map[string]*legacyNetworkInfo    `json:"Networks"`
	TimeStamp                        time.Time                        `json:"TimeStamp"`
	PnpIDByMacAddress                map[string]string                `json:"PnpIDByMacAddress"`
}

type legacyNCList string

func (l legacyNCList) IDs() ([]string, error) {
	if l == "" {
		return nil, nil
	}
	ids := strings.Split(string(l), ",")
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			return nil, fmt.Errorf("%w: empty network container ID", errMalformedLegacyNCList)
		}
		if strings.TrimSpace(id) != id {
			return nil, fmt.Errorf("%w: network container ID %q has surrounding whitespace", errMalformedLegacyNCList, id)
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("%w: duplicate network container ID %q", errMalformedLegacyNCList, id)
		}
		seen[id] = struct{}{}
	}
	return ids, nil
}

type legacyContainerStatus struct {
	ID                            string                            `json:"ID"`
	VMVersion                     string                            `json:"VMVersion"`
	HostVersion                   string                            `json:"HostVersion"`
	CreateNetworkContainerRequest cns.CreateNetworkContainerRequest `json:"CreateNetworkContainerRequest"`
	VfpUpdateComplete             bool                              `json:"VfpUpdateComplete"`
}

type legacyNetworkInfo struct {
	NetworkName string                    `json:"NetworkName"`
	NicInfo     *wireserver.InterfaceInfo `json:"NicInfo"`
	Options     map[string]any            `json:"Options"`
}

type legacyEndpointInfo struct {
	PodName       string                   `json:"PodName"`
	PodNamespace  string                   `json:"PodNamespace"`
	IfnameToIPMap map[string]*legacyIPInfo `json:"IfnameToIPMap"`
}

type legacyIPInfo struct {
	IPv4               []net.IPNet `json:"IPv4"`
	IPv6               []net.IPNet `json:"IPv6,omitempty"`
	HnsEndpointID      string      `json:"HnsEndpointID,omitempty"`
	HnsNetworkID       string      `json:"HnsNetworkID,omitempty"`
	HostVethName       string      `json:"HostVethName,omitempty"`
	MacAddress         string      `json:"MacAddress,omitempty"`
	NetworkContainerID string      `json:"NetworkContainerID,omitempty"`
	NICType            cns.NICType `json:"NICType"`
}

type legacyDeleteIntent struct {
	CreatedAt time.Time `json:"createdAt"`
}

func (s *DB) ImportLegacy(ctx context.Context, opts ImportOptions) error {
	alreadyComplete := false
	if viewErr := s.View(ctx, func(tx *ReadTx) error {
		meta, metaErr := tx.Metadata()
		if metaErr != nil {
			return fmt.Errorf("reading migration metadata: %w", metaErr)
		}
		alreadyComplete = tx.MigrationComplete() && meta.Authority == AuthorityBolt
		return nil
	}); viewErr != nil {
		return fmt.Errorf("checking legacy migration state: %w", viewErr)
	}
	if alreadyComplete {
		return nil
	}

	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.DeleteIntentTTL == 0 {
		opts.DeleteIntentTTL = defaultDeleteIntentMigrationTTL
	}

	snapshot, snapshotErr := readLegacySnapshot(opts)
	if snapshotErr != nil {
		return snapshotErr
	}
	if validateErr := snapshot.Validate(); validateErr != nil {
		return fmt.Errorf("validating legacy state: %w", validateErr)
	}

	if updateErr := s.Update(ctx, func(tx *WriteTx) error {
		meta, metaErr := tx.Metadata()
		if metaErr != nil {
			return fmt.Errorf("reading migration metadata: %w", metaErr)
		}
		if tx.MigrationComplete() && meta.Authority == AuthorityBolt {
			return errLegacyMigrationAlreadyComplete
		}
		switch meta.Authority {
		case AuthorityJSON:
			if clearErr := tx.ClearState(); clearErr != nil {
				return clearErr
			}
		case AuthorityBolt:
			empty, emptyErr := txStateEmpty(&tx.ReadTx)
			if emptyErr != nil {
				return emptyErr
			}
			if !empty {
				return fmt.Errorf("%w: refusing to merge legacy JSON into non-empty Bolt state", ErrInconsistentState)
			}
		default:
			return fmt.Errorf("%w: unrecognized state authority %q", ErrInconsistentState, meta.Authority)
		}

		snapshot.Metadata.Authority = AuthorityBolt
		snapshot.Metadata.BootID = opts.BootID
		if writeErr := writeSnapshot(tx, snapshot); writeErr != nil {
			return writeErr
		}
		if clearErr := tx.ClearRollbackComplete(); clearErr != nil {
			return clearErr
		}
		return tx.SetMigrationComplete()
	}); updateErr != nil {
		if errors.Is(updateErr, errLegacyMigrationAlreadyComplete) {
			return nil
		}
		return fmt.Errorf("importing legacy state: %w", updateErr)
	}
	return nil
}

func readLegacySnapshot(opts ImportOptions) (Snapshot, error) {
	snapshot := NewSnapshot()
	snapshot.Metadata.SchemaVersion = SchemaVersion
	snapshot.Metadata.Authority = AuthorityBolt
	snapshot.Metadata.BootID = opts.BootID

	cnsEnvelope, exists, err := readLegacyEnvelope(opts.CNSJSONPath)
	if err != nil {
		return Snapshot{}, err
	}
	if exists {
		if raw, ok := cnsEnvelope[legacyCNSStoreKey]; ok {
			var legacy *legacyCNSState
			if decodeErr := json.Unmarshal(raw, &legacy); decodeErr != nil { //nolint:musttag // Legacy wire type preserves existing field names.
				return Snapshot{}, fmt.Errorf("decoding legacy CNS state: %w", decodeErr)
			}
			if legacy == nil {
				return Snapshot{}, fmt.Errorf("decoding legacy CNS state: %w", errLegacyStateNotObject)
			}
			if addErr := addLegacyCNSState(&snapshot, *legacy); addErr != nil {
				return Snapshot{}, addErr
			}
		}
	}

	endpointEnvelope, endpointExists, err := readLegacyEnvelope(opts.EndpointJSONPath)
	if err != nil {
		return Snapshot{}, err
	}
	if endpointExists {
		if raw, ok := endpointEnvelope[legacyEndpointStoreKey]; ok {
			var endpoints *map[string]*legacyEndpointInfo
			if err := json.Unmarshal(raw, &endpoints); err != nil {
				return Snapshot{}, fmt.Errorf("decoding legacy endpoint state: %w", err)
			}
			if endpoints == nil {
				return Snapshot{}, fmt.Errorf("decoding legacy endpoint state: %w", errLegacyStateNotObject)
			}
			addLegacyEndpoints(&snapshot, *endpoints)
		}
		if raw, ok := endpointEnvelope[legacyDeleteIntentStoreKey]; ok {
			var intents *map[string]legacyDeleteIntent
			if err := json.Unmarshal(raw, &intents); err != nil {
				return Snapshot{}, fmt.Errorf("decoding legacy endpoint delete intents: %w", err)
			}
			if intents == nil {
				return Snapshot{}, fmt.Errorf("decoding legacy endpoint delete intents: %w", errLegacyStateNotObject)
			}
			for containerID, intent := range *intents {
				if deleteIntentExpired(DeleteIntent(intent), opts.Now, opts.DeleteIntentTTL) {
					continue
				}
				snapshot.DeleteIntents[containerID] = DeleteIntent(intent)
			}
		}
	}

	if opts.ManageEndpointState {
		if err := addAssignmentsFromEndpoints(&snapshot); err != nil {
			return Snapshot{}, err
		}
	}
	return snapshot, nil
}

func addLegacyCNSState(snapshot *Snapshot, legacy legacyCNSState) error {
	snapshot.Metadata.OrchestratorType = legacy.OrchestratorType
	snapshot.Metadata.NodeID = legacy.NodeID
	snapshot.Metadata.Location = legacy.Location
	snapshot.Metadata.NetworkType = legacy.NetworkType
	snapshot.Metadata.Initialized = legacy.Initialized
	snapshot.Metadata.TimeStamp = legacy.TimeStamp

	for key, list := range legacy.ContainerIDByOrchestratorContext {
		ids, err := list.IDs()
		if err != nil {
			return fmt.Errorf("%w: orchestrator context %q: %w", ErrInconsistentState, key, err)
		}
		snapshot.OrchestratorContexts[key] = ids
	}
	for mac, pnpID := range legacy.PnpIDByMacAddress {
		snapshot.PnPIDByMAC[mac] = pnpID
	}
	for name, network := range legacy.Networks {
		if network == nil {
			continue
		}
		snapshot.Networks[name] = NetworkRecord{
			NetworkName: name,
			NicInfo:     network.NicInfo,
			Options:     network.Options,
		}
	}

	for ncID := range legacy.ContainerStatus {
		legacyNC := legacy.ContainerStatus[ncID]
		id := legacyNC.ID
		if id == "" {
			id = ncID
		}
		request := legacyNC.CreateNetworkContainerRequest
		secondaryIPs := request.SecondaryIPConfigs
		record := NewNetworkContainerRecord(
			id,
			legacyNC.VMVersion,
			legacyNC.HostVersion,
			legacyNC.VfpUpdateComplete,
			request,
		)
		snapshot.NetworkContainers[id] = record

		for ipID, secondaryIP := range secondaryIPs {
			if ipID == "" {
				return fmt.Errorf("%w: NC %q contains an empty IP ID", ErrInconsistentState, id)
			}
			if _, exists := snapshot.IPs[ipID]; exists {
				return fmt.Errorf("%w: duplicate IP ID %q", ErrInconsistentState, ipID)
			}
			snapshot.IPs[ipID] = IPRecord{
				ID:        ipID,
				IPAddress: secondaryIP.IPAddress,
				NCID:      id,
				NCVersion: secondaryIP.NCVersion,
			}
		}
	}
	return nil
}

func addLegacyEndpoints(snapshot *Snapshot, endpoints map[string]*legacyEndpointInfo) {
	for containerID, endpoint := range endpoints {
		if endpoint == nil {
			continue
		}
		record := EndpointRecord{
			PodName:       endpoint.PodName,
			PodNamespace:  endpoint.PodNamespace,
			IfnameToIPMap: make(map[string]*IPInfoRecord, len(endpoint.IfnameToIPMap)),
		}
		for ifName, ipInfo := range endpoint.IfnameToIPMap {
			if ipInfo == nil {
				continue
			}
			record.IfnameToIPMap[ifName] = &IPInfoRecord{
				IPv4:               ipInfo.IPv4,
				IPv6:               ipInfo.IPv6,
				HNSEndpointID:      ipInfo.HnsEndpointID,
				HNSNetworkID:       ipInfo.HnsNetworkID,
				HostVethName:       ipInfo.HostVethName,
				MACAddress:         ipInfo.MacAddress,
				NetworkContainerID: ipInfo.NetworkContainerID,
				NICType:            ipInfo.NICType,
			}
		}
		snapshot.Endpoints[containerID] = record
	}
}

func addAssignmentsFromEndpoints(snapshot *Snapshot) error {
	ipIDByAddress := make(map[string]string, len(snapshot.IPs))
	for ipID, ip := range snapshot.IPs {
		if previous, exists := ipIDByAddress[ip.IPAddress]; exists {
			return fmt.Errorf("%w: IP address %q maps to IDs %q and %q", ErrInconsistentState, ip.IPAddress, previous, ipID)
		}
		ipIDByAddress[ip.IPAddress] = ipID
	}

	for containerID, endpoint := range snapshot.Endpoints {
		assignment := AssignmentRecord{
			Pod: PodIdentity{
				PodKey:           containerID,
				InfraContainerID: containerID,
				InterfaceID:      containerID,
				PodName:          endpoint.PodName,
				PodNamespace:     endpoint.PodNamespace,
			},
		}
		for _, ipInfo := range endpoint.IfnameToIPMap {
			if ipInfo == nil || !ipInfo.NICType.IsInfraOrLegacy() {
				continue
			}
			for _, ipNet := range append(ipInfo.IPv4, ipInfo.IPv6...) {
				address := ipNet.IP.String()
				ipID, ok := ipIDByAddress[address]
				if !ok {
					return fmt.Errorf("%w: endpoint %q references IP %q outside the CNS inventory", ErrInconsistentState, containerID, address)
				}
				if owner, exists := snapshot.IPOwners[ipID]; exists && owner != containerID {
					return fmt.Errorf("%w: IP %q is owned by %q and %q", ErrInconsistentState, ipID, owner, containerID)
				}
				snapshot.IPOwners[ipID] = containerID
				assignment.IPIDs = append(assignment.IPIDs, ipID)
			}
		}
		if len(assignment.IPIDs) != 0 {
			snapshot.Assignments[containerID] = assignment
		}
	}
	return nil
}

func readLegacyEnvelope(path string) (LegacyEnvelope, bool, error) {
	if path == "" {
		return nil, false, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading legacy state %q: %w", path, err)
	}
	if len(data) == 0 {
		return nil, false, nil
	}

	var envelope LegacyEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, false, fmt.Errorf("decoding legacy state envelope %q: %w", path, err)
	}
	if envelope == nil {
		return nil, false, fmt.Errorf("decoding legacy state envelope %q: %w", path, errLegacyStateNotObject)
	}
	return envelope, true, nil
}

func writeSnapshot(tx *WriteTx, snapshot Snapshot) error {
	if err := tx.PutMetadata(snapshot.Metadata); err != nil {
		return err
	}
	for id := range snapshot.NetworkContainers {
		record := snapshot.NetworkContainers[id]
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
	for _, assignment := range snapshot.Assignments {
		if err := tx.PutAssignment(assignment); err != nil {
			return err
		}
	}
	for ipID, podKey := range snapshot.IPOwners {
		if err := tx.PutIPOwner(ipID, podKey); err != nil {
			return err
		}
	}
	for containerID, endpoint := range snapshot.Endpoints {
		if err := tx.PutEndpoint(containerID, endpoint); err != nil {
			return err
		}
	}
	for containerID, intent := range snapshot.DeleteIntents {
		if err := tx.PutDeleteIntent(containerID, intent); err != nil {
			return err
		}
	}
	return nil
}

func txStateEmpty(tx *ReadTx) (bool, error) {
	checks := []func() (int, error){
		func() (int, error) {
			records, err := tx.NetworkContainers()
			return len(records), err
		},
		func() (int, error) {
			records, err := tx.IPs()
			return len(records), err
		},
		func() (int, error) {
			records, err := tx.Networks()
			return len(records), err
		},
		func() (int, error) {
			records, err := tx.OrchestratorContexts()
			return len(records), err
		},
		func() (int, error) {
			records, err := tx.PnPIDByMAC()
			return len(records), err
		},
		func() (int, error) {
			records, err := tx.Endpoints()
			return len(records), err
		},
		func() (int, error) {
			records, err := tx.Assignments()
			return len(records), err
		},
		func() (int, error) {
			records, err := tx.IPOwners()
			return len(records), err
		},
		func() (int, error) {
			records, err := tx.DeleteIntents()
			return len(records), err
		},
	}
	for _, check := range checks {
		count, err := check()
		if err != nil {
			return false, err
		}
		if count != 0 {
			return false, nil
		}
	}
	return true, nil
}

func (s *DB) ExportLegacy(ctx context.Context, cnsJSONPath, endpointJSONPath string) error {
	alreadyComplete := false
	if viewErr := s.View(ctx, func(tx *ReadTx) error {
		meta, metaErr := tx.Metadata()
		if metaErr != nil {
			return fmt.Errorf("reading rollback metadata: %w", metaErr)
		}
		alreadyComplete = tx.RollbackComplete() && meta.Authority == AuthorityJSON
		return nil
	}); viewErr != nil {
		return fmt.Errorf("checking rollback state: %w", viewErr)
	}
	if alreadyComplete {
		return nil
	}

	snapshot, snapshotErr := s.Snapshot(ctx)
	if snapshotErr != nil {
		return fmt.Errorf("reading rollback snapshot: %w", snapshotErr)
	}

	cnsEnvelope, endpointEnvelope, envelopeErr := legacyEnvelopes(snapshot)
	if envelopeErr != nil {
		return envelopeErr
	}
	if writeErr := atomicWriteJSON(cnsJSONPath, cnsEnvelope); writeErr != nil {
		return writeErr
	}
	if writeErr := atomicWriteJSON(endpointJSONPath, endpointEnvelope); writeErr != nil {
		return writeErr
	}

	if updateErr := s.Update(ctx, func(tx *WriteTx) error {
		meta, metaErr := tx.Metadata()
		if metaErr != nil {
			return fmt.Errorf("reading rollback metadata: %w", metaErr)
		}
		meta.Authority = AuthorityJSON
		if writeErr := tx.PutMetadata(meta); writeErr != nil {
			return writeErr
		}
		return tx.SetRollbackComplete()
	}); updateErr != nil {
		return fmt.Errorf("marking JSON state authoritative: %w", updateErr)
	}
	return nil
}

func legacyEnvelopes(snapshot Snapshot) (cnsEnvelope, endpointEnvelope LegacyEnvelope, resultErr error) {
	legacy := legacyCNSState{
		Location:                         snapshot.Metadata.Location,
		NetworkType:                      snapshot.Metadata.NetworkType,
		OrchestratorType:                 snapshot.Metadata.OrchestratorType,
		NodeID:                           snapshot.Metadata.NodeID,
		Initialized:                      snapshot.Metadata.Initialized,
		ContainerIDByOrchestratorContext: make(map[string]legacyNCList, len(snapshot.OrchestratorContexts)),
		ContainerStatus:                  make(map[string]legacyContainerStatus, len(snapshot.NetworkContainers)),
		Networks:                         make(map[string]*legacyNetworkInfo, len(snapshot.Networks)),
		TimeStamp:                        snapshot.Metadata.TimeStamp,
		PnpIDByMacAddress:                snapshot.PnPIDByMAC,
	}
	for key, ncIDs := range snapshot.OrchestratorContexts {
		legacy.ContainerIDByOrchestratorContext[key] = legacyNCList(strings.Join(ncIDs, ","))
	}
	for name, network := range snapshot.Networks {
		legacy.Networks[name] = &legacyNetworkInfo{
			NetworkName: name,
			NicInfo:     network.NicInfo,
			Options:     network.Options,
		}
	}
	for ncID := range snapshot.NetworkContainers {
		record := snapshot.NetworkContainers[ncID]
		request := record.Request
		request.AuthorizationToken = ""
		request.SecondaryIPConfigs = make(map[string]cns.SecondaryIPConfig)
		for ipID, ip := range snapshot.IPs {
			if ip.NCID == ncID {
				request.SecondaryIPConfigs[ipID] = cns.SecondaryIPConfig{
					IPAddress: ip.IPAddress,
					NCVersion: ip.NCVersion,
				}
			}
		}
		legacy.ContainerStatus[ncID] = legacyContainerStatus{
			ID:                            record.ID,
			VMVersion:                     record.VMVersion,
			HostVersion:                   record.HostVersion,
			CreateNetworkContainerRequest: request,
			VfpUpdateComplete:             record.VFPUpdateComplete,
		}
	}

	legacyEndpoints := make(map[string]*legacyEndpointInfo, len(snapshot.Endpoints))
	for containerID, endpoint := range snapshot.Endpoints {
		legacyEndpoint := &legacyEndpointInfo{
			PodName:       endpoint.PodName,
			PodNamespace:  endpoint.PodNamespace,
			IfnameToIPMap: make(map[string]*legacyIPInfo, len(endpoint.IfnameToIPMap)),
		}
		for ifName, ipInfo := range endpoint.IfnameToIPMap {
			if ipInfo == nil {
				continue
			}
			legacyEndpoint.IfnameToIPMap[ifName] = &legacyIPInfo{
				IPv4:               ipInfo.IPv4,
				IPv6:               ipInfo.IPv6,
				HnsEndpointID:      ipInfo.HNSEndpointID,
				HnsNetworkID:       ipInfo.HNSNetworkID,
				HostVethName:       ipInfo.HostVethName,
				MacAddress:         ipInfo.MACAddress,
				NetworkContainerID: ipInfo.NetworkContainerID,
				NICType:            ipInfo.NICType,
			}
		}
		legacyEndpoints[containerID] = legacyEndpoint
	}
	legacyIntents := make(map[string]legacyDeleteIntent, len(snapshot.DeleteIntents))
	for containerID, intent := range snapshot.DeleteIntents {
		legacyIntents[containerID] = legacyDeleteIntent(intent)
	}

	cnsData, err := json.Marshal(legacy) //nolint:musttag // Legacy wire type preserves existing field names.
	if err != nil {
		return nil, nil, fmt.Errorf("encoding legacy CNS state: %w", err)
	}
	endpointData, err := json.Marshal(legacyEndpoints)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding legacy endpoint state: %w", err)
	}
	intentData, err := json.Marshal(legacyIntents)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding legacy delete intents: %w", err)
	}

	return LegacyEnvelope{legacyCNSStoreKey: cnsData}, LegacyEnvelope{
		legacyEndpointStoreKey:     endpointData,
		legacyDeleteIntentStoreKey: intentData,
	}, nil
}

func atomicWriteJSON(path string, value any) (err error) {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(value, "", "\t")
	if err != nil {
		return fmt.Errorf("encoding legacy state %q: %w", path, err)
	}
	if mkdirErr := os.MkdirAll(filepath.Dir(path), 0o755); mkdirErr != nil {
		return fmt.Errorf("creating legacy state directory %q: %w", filepath.Dir(path), mkdirErr)
	}

	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temporary legacy state file for %q: %w", path, err)
	}
	tempPath := file.Name()
	defer func() {
		_ = file.Close()
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()

	if _, err = file.Write(data); err != nil {
		return fmt.Errorf("writing temporary legacy state file for %q: %w", path, err)
	}
	if err = file.Sync(); err != nil {
		return fmt.Errorf("syncing temporary legacy state file for %q: %w", path, err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("closing temporary legacy state file for %q: %w", path, err)
	}
	if err = platform.ReplaceFile(tempPath, path); err != nil {
		return fmt.Errorf("replacing legacy state file %q: %w", path, err)
	}
	return nil
}
