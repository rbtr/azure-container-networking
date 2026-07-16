// Package state owns the transactional persistent state used by CNS.
//
// One bbolt database stores durable NC/network state and, when CNS manages
// endpoint state, pod assignments, endpoint records, and delete intents. Typed
// operations update related buckets in one transaction so an IP cannot be
// persisted as both available and endpoint-owned.
//
// The database is durable across node reboots. A per-boot identifier controls
// which session-scoped records are cleared or reconciled while NC and network
// seed state remains available to all CNS channel modes.
package state
