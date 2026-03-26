// Package store provides a boltdb-backed persistent datastore for CNS state.
//
// # Overview
//
// CNS (Container Networking Service) persists two categories of state to disk:
//
//   - NC state: Network Containers, secondary IPs, networks, orchestrator
//     context, PnP/MAC mappings, and node-level metadata. Managed by
//     [NCBoltStore].
//   - Endpoint state: per-container endpoint records mapping container IDs to
//     pod names, namespaces, and interface IPs. Managed by [EndpointBoltStore].
//
// Each store is a single boltdb file opened with [OpenNCStore] or
// [OpenEndpointStore]. All methods are safe for concurrent use.
//
// # Schema versioning
//
// Every database file contains a "meta" bucket with a uint16 schema version
// ([SchemaVersion]). On open, the store checks that the on-disk version
// matches the code's expected version. A mismatch returns [ErrSchemaMismatch].
// Increment SchemaVersion when making incompatible changes to bucket layout
// or record shapes.
//
// # Record types
//
// The record types in this package ([NCRecord], [IPRecord], [NetworkRecord],
// [EndpointRecord], etc.) are the wire representations stored in boltdb. They
// are intentionally separate from the cns.* API types so that schema evolution
// can happen independently of the public API surface.
//
// # Migration from JSON
//
// [MigrateCNSState] and [MigrateEndpointState] read the legacy JSON state
// files (azure-cns.json and azure-endpoints.json) and write their contents
// into the corresponding boltdb store. Migration is idempotent: a second call
// is a no-op once a completion marker is set in the meta bucket. On success
// the old JSON file is renamed to "<path>.migrated".
//
// # Low-level bucket access
//
// Both store types expose Get, Put, Delete, and ForEach methods for direct
// bucket-level key/value access. These are used internally by the migration
// system for marker keys and are available for any caller that needs precise
// control over the underlying boltdb buckets.
package store
