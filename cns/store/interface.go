// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package store

import "context"

// NCStore is the high-level typed interface for persisting Network Container
// and related CNS state (metadata, IP pool, networks, orchestrator context,
// and PnP/MAC mappings).  All methods are safe to call concurrently.
type NCStore interface {
	// --- Node-level metadata ---

	PutMeta(ctx context.Context, m StoreMeta) error
	GetMeta(ctx context.Context) (StoreMeta, error)

	// --- Network Containers ---

	PutNC(ctx context.Context, nc NCRecord) error
	GetNC(ctx context.Context, ncID string) (NCRecord, error)
	DeleteNC(ctx context.Context, ncID string) error
	ListNCs(ctx context.Context) ([]NCRecord, error)

	// --- Secondary IPs ---

	PutIP(ctx context.Context, ip IPRecord) error
	GetIP(ctx context.Context, ipAddr string) (IPRecord, error)
	DeleteIP(ctx context.Context, ipAddr string) error
	// DeleteIPsByNCID removes all IP records whose NCID field matches ncID.
	// This is used when an NC is removed.
	DeleteIPsByNCID(ctx context.Context, ncID string) error
	ListIPs(ctx context.Context) ([]IPRecord, error)

	// --- Networks ---

	PutNetwork(ctx context.Context, n NetworkRecord) error
	GetNetwork(ctx context.Context, name string) (NetworkRecord, error)
	DeleteNetwork(ctx context.Context, name string) error
	ListNetworks(ctx context.Context) ([]NetworkRecord, error)

	// --- Orchestrator context → NC list ---

	PutOrchestratorContext(ctx context.Context, key string, ncIDs []string) error
	GetOrchestratorContext(ctx context.Context, key string) ([]string, error)
	DeleteOrchestratorContext(ctx context.Context, key string) error
	ListOrchestratorContexts(ctx context.Context) (map[string][]string, error)

	// --- PnP ID by MAC address ---

	PutPnpIDByMAC(ctx context.Context, mac, pnpID string) error
	GetPnpIDByMAC(ctx context.Context, mac string) (string, error)
	ListPnpIDByMAC(ctx context.Context) (map[string]string, error)

	// Close releases the underlying database handle.
	Close() error
}

// EndpointStore is the high-level typed interface for persisting per-container
// endpoint state (container ID → pod name/namespace + interface IPs).
// All methods are safe to call concurrently.
type EndpointStore interface {
	PutEndpoint(ctx context.Context, containerID string, ep EndpointRecord) error
	GetEndpoint(ctx context.Context, containerID string) (EndpointRecord, error)
	DeleteEndpoint(ctx context.Context, containerID string) error
	ListEndpoints(ctx context.Context) (map[string]EndpointRecord, error)

	// Close releases the underlying database handle.
	Close() error
}

// BucketReadWriter exposes low-level, bucket-scoped key-value access for
// callers that need precise control.  Values are raw bytes; encoding is the
// caller's responsibility.
type BucketReadWriter interface {
	Get(bucket, key []byte) ([]byte, error)
	Put(bucket, key, value []byte) error
	Delete(bucket, key []byte) error
	// ForEach iterates every key/value pair in the named bucket, calling fn
	// for each entry.  The iteration stops and the error is returned if fn
	// returns a non-nil error.
	ForEach(bucket []byte, fn func(k, v []byte) error) error
}
