// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package store provides a boltdb-backed persistent datastore for CNS state.
// These types are the wire representations stored in boltdb — they are
// intentionally separate from the cns.* API types so that schema evolution
// can happen independently of the public API surface.
package store

import (
	"net"
	"time"

	"github.com/Azure/azure-container-networking/cns"
)

// SchemaVersion is the current on-disk schema version. Increment when making
// incompatible changes to the bucket layout or record shapes.
const SchemaVersion uint16 = 1

// metaKey* constants are keys inside the "meta" bucket.
const (
	metaKeyVersion          = "version"
	metaKeyOrchestratorType = "orchestratorType"
	metaKeyNodeID           = "nodeID"
	metaKeyLocation         = "location"
	metaKeyNetworkType      = "networkType"
	metaKeyInitialized      = "initialized"
	metaKeyTimestamp        = "timestamp"
)

// bucket name constants — used as bucket keys in both DB files.
const (
	bucketMeta                = "meta"
	bucketNetworkContainers   = "network_containers"
	bucketIPs                 = "ips"
	bucketNetworks            = "networks"
	bucketOrchestratorContext = "orchestrator_context"
	bucketPnpMAC              = "pnp_mac"
	bucketEndpoints           = "endpoints"
)

// StoreMeta captures node-level metadata persisted alongside NC/IP state.
type StoreMeta struct {
	Version          uint16
	OrchestratorType string
	NodeID           string
	Location         string
	NetworkType      string
	Initialized      bool
	TimeStamp        time.Time
}

// NCRecord is the persistent representation of a Network Container.
// SecondaryIPConfigs are stored separately in the IPs bucket (see IPRecord).
type NCRecord struct {
	// ID is the Network Container UUID.
	ID string
	// VMVersion is the version last seen from the VM side.
	VMVersion string
	// HostVersion is the version last acknowledged by host agent.
	HostVersion string
	// VfpUpdateComplete is true once VFP dataplane programming is done.
	VfpUpdateComplete bool

	// Fields sourced from CreateNetworkContainerRequest:
	HostPrimaryIP              string
	Version                    string
	NetworkContainerType       string
	PrimaryInterfaceIdentifier string
	LocalIPConfiguration       cns.IPConfiguration
	OrchestratorContext        []byte // raw JSON blob
	IPConfiguration            cns.IPConfiguration
	MultiTenancyInfo           cns.MultiTenancyInfo
	CnetAddressSpace           []cns.IPSubnet
	Routes                     []cns.Route
	AllowHostToNCCommunication bool
	AllowNCToHostCommunication bool
	SkipDefaultRoutes          bool
	NetworkInterfaceInfo       cns.NetworkInterfaceInfo
}

// IPRecord is the persistent representation of a single secondary IP in the
// NC IP pool. Each IP gets its own row in the "ips" bucket so that pod
// assignment/release can be recorded with a single bucket Put rather than
// rewriting the entire NC blob.
type IPRecord struct {
	// IPAddress is also the bucket key (e.g. "192.168.0.5").
	IPAddress string
	// NCID is the foreign key linking this IP to its NCRecord.
	NCID string
	// NCVersion mirrors SecondaryIPConfig.NCVersion for readiness gating.
	NCVersion int
}

// NetworkRecord is the persistent form of an in-memory networkInfo entry.
type NetworkRecord struct {
	NetworkName string
	NicInfo     *NicInfoRecord
	Options     map[string]interface{}
}

// NicInfoRecord captures the wireserver InterfaceInfo fields that CNS needs to
// reconstruct network state. A separate (flat) struct avoids a cross-package
// import in the types file.
type NicInfoRecord struct {
	Subnet       string
	Gateway      string
	IsPrimary    bool
	PrimaryIP    string
	SecondaryIPs []string
}

// EndpointRecord is the persistent form of a single container endpoint.
// The bucket key is the container ID (64-char hex string).
type EndpointRecord struct {
	PodName       string
	PodNamespace  string
	IfnameToIPMap map[string]*IPInfoRecord
}

// IPInfoRecord is the persistent form of the per-interface IP information
// attached to an endpoint.
type IPInfoRecord struct {
	IPv4               []net.IPNet
	IPv6               []net.IPNet `json:",omitempty"`
	HnsEndpointID      string      `json:",omitempty"`
	HnsNetworkID       string      `json:",omitempty"`
	HostVethName       string      `json:",omitempty"`
	MacAddress         string      `json:",omitempty"`
	NetworkContainerID string      `json:",omitempty"`
	NICType            cns.NICType
}
