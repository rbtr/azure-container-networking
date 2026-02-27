// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package store

// Migration from the legacy JSON statefiles to boltdb.
//
// Design principles:
//   - Migration is idempotent: a second run is a no-op once the boltdb already
//     has data (detected via schema version in the meta bucket).
//   - On success the old JSON file is renamed to "<path>.migrated" as a backup;
//     it is not deleted.  Operators may prune it at their discretion.
//   - Migration failures return an error and leave the source file untouched.
//   - The migration code uses local compatibility structs (jsonCNSState, etc.)
//     that mirror the restserver wire format exactly, avoiding an import cycle
//     with the restserver package.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-container-networking/cns"
)

const (
	migrationKeyCNSStateComplete      = "migration_cns_state_complete"
	migrationKeyEndpointStateComplete = "migration_endpoint_state_complete"
)

// ---- JSON compatibility structs ----
// These exactly replicate the on-disk shapes written by store/json.go so we
// can deserialise them without depending on the restserver package.

// jsonFileEnvelope is the top-level wrapper written by jsonFileStore:
//
//	{ "<storeKey>": <rawValue> }
type jsonFileEnvelope map[string]json.RawMessage

// jsonCNSState mirrors httpRestServiceState in cns/restserver/restserver.go.
type jsonCNSState struct {
	Location                         string
	NetworkType                      string
	OrchestratorType                 string
	NodeID                           string
	Initialized                      bool
	ContainerIDByOrchestratorContext map[string]jsonNCList
	ContainerStatus                  map[string]jsonContainerStatus
	Networks                         map[string]*jsonNetworkInfo
	TimeStamp                        time.Time
	PnpIDByMacAddress                map[string]string
}

// jsonNCList is the comma-separated NC list stored per orchestrator context key.
// The original type is a private *ncList struct; the JSON encoding is a plain
// string containing a comma-separated list of NC IDs.
type jsonNCList string

// ncIDs splits the comma-separated string into a slice.
func (l jsonNCList) ncIDs() []string {
	s := string(l)
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// jsonContainerStatus mirrors the containerstatus struct.
type jsonContainerStatus struct {
	ID                            string
	VMVersion                     string
	HostVersion                   string
	CreateNetworkContainerRequest jsonCreateNCRequest
	VfpUpdateComplete             bool
}

// jsonCreateNCRequest is a minimal projection of CreateNetworkContainerRequest
// containing only the fields we need for migration.  The full struct is in
// cns/NetworkContainerContract.go.
type jsonCreateNCRequest struct {
	HostPrimaryIP              string
	Version                    string
	NetworkContainerType       string
	NetworkContainerid         string
	PrimaryInterfaceIdentifier string
	LocalIPConfiguration       cns.IPConfiguration
	OrchestratorContext        json.RawMessage
	IPConfiguration            cns.IPConfiguration
	SecondaryIPConfigs         map[string]cns.SecondaryIPConfig
	MultiTenancyInfo           cns.MultiTenancyInfo
	CnetAddressSpace           []cns.IPSubnet
	Routes                     []cns.Route
	AllowHostToNCCommunication bool
	AllowNCToHostCommunication bool
	SkipDefaultRoutes          bool
	NetworkInterfaceInfo       cns.NetworkInterfaceInfo
}

// jsonNetworkInfo mirrors the networkInfo struct.
type jsonNetworkInfo struct {
	NetworkName string
	NicInfo     *jsonNicInfo
	Options     map[string]interface{}
}

// jsonNicInfo mirrors wireserver.InterfaceInfo.
type jsonNicInfo struct {
	Subnet       string
	Gateway      string
	IsPrimary    bool
	PrimaryIP    string
	SecondaryIPs []string
}

// jsonEndpointState mirrors the map[string]*EndpointInfo persisted by the
// endpoint store.  Key is container ID.
type jsonEndpointState map[string]*jsonEndpointInfo

// jsonEndpointInfo mirrors restserver.EndpointInfo.
type jsonEndpointInfo struct {
	PodName       string
	PodNamespace  string
	IfnameToIPMap map[string]*jsonIPInfo
}

// jsonIPInfo mirrors restserver.IPInfo.
type jsonIPInfo struct {
	IPv4               []net.IPNet
	IPv6               []net.IPNet `json:",omitempty"`
	HnsEndpointID      string      `json:",omitempty"`
	HnsNetworkID       string      `json:",omitempty"`
	HostVethName       string      `json:",omitempty"`
	MacAddress         string      `json:",omitempty"`
	NetworkContainerID string      `json:",omitempty"`
	NICType            cns.NICType
}

// ---- MigrateCNSState ----

// MigrateCNSState reads the legacy azure-cns.json file at jsonPath and writes
// the NC, IP, network and metadata records into dst.
//
// If jsonPath does not exist the function returns nil (clean-slate node).
// If dst already contains data (schema version present and NC count > 0) the
// function returns nil without modifying dst (idempotent).
//
// On success jsonPath is renamed to jsonPath+".migrated".
func MigrateCNSState(ctx context.Context, jsonPath string, dst NCStore) error {
	brw := NCStoreBucketReadWriter(dst)
	if done, err := migrationComplete(brw, migrationKeyCNSStateComplete); err != nil {
		return fmt.Errorf("cns/store: read CNS migration marker: %w", err)
	} else if done {
		return nil
	}

	raw, err := os.ReadFile(jsonPath)
	if os.IsNotExist(err) {
		return nil // fresh node, nothing to migrate
	}
	if err != nil {
		return fmt.Errorf("cns/store: read %q: %w", jsonPath, err)
	}

	// Parse the JSON envelope.
	var envelope jsonFileEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("cns/store: parse %q envelope: %w", jsonPath, err)
	}

	stateRaw, ok := envelope["ContainerNetworkService"]
	if !ok {
		// File exists but has no CNS key — treat as empty.
		if err := renameOldFile(jsonPath); err != nil {
			return err
		}
		return setMigrationComplete(brw, migrationKeyCNSStateComplete)
	}

	var state jsonCNSState
	if err := json.Unmarshal(stateRaw, &state); err != nil {
		return fmt.Errorf("cns/store: parse CNS state: %w", err)
	}

	// Write metadata.
	meta := StoreMeta{
		Version:          SchemaVersion,
		OrchestratorType: state.OrchestratorType,
		NodeID:           state.NodeID,
		Location:         state.Location,
		NetworkType:      state.NetworkType,
		Initialized:      state.Initialized,
		TimeStamp:        state.TimeStamp,
	}
	if err := dst.PutMeta(ctx, meta); err != nil {
		return fmt.Errorf("cns/store: write meta: %w", err)
	}

	// Write NCs and their secondary IPs.
	for _, cs := range state.ContainerStatus {
		req := cs.CreateNetworkContainerRequest

		nc := NCRecord{
			ID:                         cs.ID,
			VMVersion:                  cs.VMVersion,
			HostVersion:                cs.HostVersion,
			VfpUpdateComplete:          cs.VfpUpdateComplete,
			HostPrimaryIP:              req.HostPrimaryIP,
			Version:                    req.Version,
			NetworkContainerType:       req.NetworkContainerType,
			PrimaryInterfaceIdentifier: req.PrimaryInterfaceIdentifier,
			LocalIPConfiguration:       req.LocalIPConfiguration,
			OrchestratorContext:        []byte(req.OrchestratorContext),
			IPConfiguration:            req.IPConfiguration,
			MultiTenancyInfo:           req.MultiTenancyInfo,
			CnetAddressSpace:           req.CnetAddressSpace,
			Routes:                     req.Routes,
			AllowHostToNCCommunication: req.AllowHostToNCCommunication,
			AllowNCToHostCommunication: req.AllowNCToHostCommunication,
			SkipDefaultRoutes:          req.SkipDefaultRoutes,
			NetworkInterfaceInfo:       req.NetworkInterfaceInfo,
		}
		if err := dst.PutNC(ctx, nc); err != nil {
			return fmt.Errorf("cns/store: write NC %q: %w", cs.ID, err)
		}

		for _, secIP := range req.SecondaryIPConfigs {
			ipRec := IPRecord{
				IPAddress: secIP.IPAddress,
				NCID:      cs.ID,
				NCVersion: secIP.NCVersion,
			}
			if err := dst.PutIP(ctx, ipRec); err != nil {
				return fmt.Errorf("cns/store: write IP %q: %w", secIP.IPAddress, err)
			}
		}
	}

	// Write networks.
	for name, ni := range state.Networks {
		if ni == nil {
			continue
		}
		nr := NetworkRecord{
			NetworkName: name,
			Options:     ni.Options,
		}
		if ni.NicInfo != nil {
			nr.NicInfo = &NicInfoRecord{
				Subnet:       ni.NicInfo.Subnet,
				Gateway:      ni.NicInfo.Gateway,
				IsPrimary:    ni.NicInfo.IsPrimary,
				PrimaryIP:    ni.NicInfo.PrimaryIP,
				SecondaryIPs: ni.NicInfo.SecondaryIPs,
			}
		}
		if err := dst.PutNetwork(ctx, nr); err != nil {
			return fmt.Errorf("cns/store: write network %q: %w", name, err)
		}
	}

	// Write orchestrator context mappings.
	for key, ncList := range state.ContainerIDByOrchestratorContext {
		ids := ncList.ncIDs()
		if len(ids) == 0 {
			continue
		}
		if err := dst.PutOrchestratorContext(ctx, key, ids); err != nil {
			return fmt.Errorf("cns/store: write orchestrator context %q: %w", key, err)
		}
	}

	// Write PnP/MAC mappings.
	for mac, pnpID := range state.PnpIDByMacAddress {
		if err := dst.PutPnpIDByMAC(ctx, mac, pnpID); err != nil {
			return fmt.Errorf("cns/store: write PnP MAC %q: %w", mac, err)
		}
	}

	if err := renameOldFile(jsonPath); err != nil {
		return err
	}

	return setMigrationComplete(brw, migrationKeyCNSStateComplete)
}

// ---- MigrateEndpointState ----

// MigrateEndpointState reads the legacy azure-endpoints.json file at jsonPath
// and writes endpoint records into dst.
//
// If jsonPath does not exist the function returns nil.
// If dst already has endpoints the function returns nil (idempotent).
//
// On success jsonPath is renamed to jsonPath+".migrated".
func MigrateEndpointState(ctx context.Context, jsonPath string, dst EndpointStore) error {
	brw := EndpointStoreBucketReadWriter(dst)
	if done, err := migrationComplete(brw, migrationKeyEndpointStateComplete); err != nil {
		return fmt.Errorf("cns/store: read endpoint migration marker: %w", err)
	} else if done {
		return nil
	}

	raw, err := os.ReadFile(jsonPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cns/store: read %q: %w", jsonPath, err)
	}

	var envelope jsonFileEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("cns/store: parse %q envelope: %w", jsonPath, err)
	}

	stateRaw, ok := envelope["Endpoints"]
	if !ok {
		if err := renameOldFile(jsonPath); err != nil {
			return err
		}
		return setMigrationComplete(brw, migrationKeyEndpointStateComplete)
	}

	var state jsonEndpointState
	if err := json.Unmarshal(stateRaw, &state); err != nil {
		return fmt.Errorf("cns/store: parse endpoint state: %w", err)
	}

	for containerID, ep := range state {
		if ep == nil {
			continue
		}
		rec := EndpointRecord{
			PodName:       ep.PodName,
			PodNamespace:  ep.PodNamespace,
			IfnameToIPMap: make(map[string]*IPInfoRecord, len(ep.IfnameToIPMap)),
		}
		for ifname, ipInfo := range ep.IfnameToIPMap {
			if ipInfo == nil {
				continue
			}
			rec.IfnameToIPMap[ifname] = &IPInfoRecord{
				IPv4:               ipInfo.IPv4,
				IPv6:               ipInfo.IPv6,
				HnsEndpointID:      ipInfo.HnsEndpointID,
				HnsNetworkID:       ipInfo.HnsNetworkID,
				HostVethName:       ipInfo.HostVethName,
				MacAddress:         ipInfo.MacAddress,
				NetworkContainerID: ipInfo.NetworkContainerID,
				NICType:            ipInfo.NICType,
			}
		}
		if err := dst.PutEndpoint(ctx, containerID, rec); err != nil {
			return fmt.Errorf("cns/store: write endpoint %q: %w", containerID, err)
		}
	}

	if err := renameOldFile(jsonPath); err != nil {
		return err
	}

	return setMigrationComplete(brw, migrationKeyEndpointStateComplete)
}

// ---- helpers ----

// renameOldFile renames f to f+".migrated".
func renameOldFile(path string) error {
	if err := os.Rename(path, path+".migrated"); err != nil {
		return fmt.Errorf("cns/store: rename %q to .migrated: %w (migration data written successfully)", path, err)
	}
	return nil
}

func migrationComplete(brw BucketReadWriter, markerKey string) (bool, error) {
	if brw == nil {
		return false, nil
	}
	v, err := brw.Get([]byte(bucketMeta), []byte(markerKey))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return string(v) == "1", nil
}

func setMigrationComplete(brw BucketReadWriter, markerKey string) error {
	if brw == nil {
		return nil
	}
	if err := brw.Put([]byte(bucketMeta), []byte(markerKey), []byte("1")); err != nil {
		return fmt.Errorf("set migration marker %q: %w", markerKey, err)
	}
	return nil
}
