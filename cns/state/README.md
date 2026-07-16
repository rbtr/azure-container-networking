# CNS unified state store

CNS currently writes related state to `azure-cns.json` and, when
`ManageEndpointState=true`, `azure-endpoints.json`. Independent file
replacement cannot atomically update IP ownership, endpoint records, and
delete intents. The unified state store uses one bbolt database so those
changes commit or abort together.

## Database location and reboot behavior

The default database is `azure-cns.db` in the existing durable CNS state
directory (`/var/lib/azure-network` on Linux and the configured CNS store path
on Windows).

The whole database is not placed in `/var/run` because not every CNS mode can
reconstruct all state after reboot:

- CRD, MultiTenantCRD, and AzureHost have authoritative reconciliation paths.
- Managed mode can pull NC state but currently starts serving before its first
  complete sync.
- Direct and legacy flows have no complete pull-based startup source.
- persisted network records are used to restore host networking after reboot.

Instead, CNS records a reliable OS boot identifier. On boot change it
atomically resets or reconciles session-scoped records while retaining durable
NC/network seed state. Readiness observations are reset only in modes with an
authoritative startup reconciliation source; Direct/legacy modes retain their
last durable seed until their producer updates it.

## State model

Durable buckets contain:

- node/service metadata
- complete, sanitized NC records
- IP inventory keyed by the original IP ID/UUID
- networks
- orchestrator mappings
- PnP mappings

When CNS owns endpoint state, session buckets contain:

- pod assignments
- reverse IP-owner index
- endpoint records
- delete-intent tombstones

`AuthorizationToken` is stripped before any NC record is written.

## Invariants

The database and startup validator enforce:

- each IP ID belongs to one NC
- each IP ID has at most one pod owner
- assignment and reverse-owner indexes agree
- CNS-owned assignments reference an endpoint containing the same infra IPs
- assigned IPs exist in the current inventory
- delete intents block late ADD/PATCH operations
- duplicate infra endpoint IPs fail startup

## Migration

`StateStoreBackend` selects `json` (default) or `bolt`. The first Bolt start
reads the applicable legacy CNS and endpoint files, validates the complete
snapshot, and imports all records plus the migration marker in one bbolt
transaction. Dynamic secondary-IP map keys are preserved; migration never
re-keys records by address.

Legacy files remain as backups but are never used as an automatic fallback
after Bolt becomes authoritative.

## Rollback

Set:

```json
{
  "StateStoreBackend": "json",
  "StateStoreMode": "rollback-to-json"
}
```

Before the listener starts, CNS exports one consistent Bolt snapshot to both
legacy JSON shapes using fsync and atomic file replacement. It then marks JSON
authoritative. Repeated rollback starts do not overwrite newer JSON state.
After a successful export, return `StateStoreMode` to `normal`.

## Debugging

When Bolt is enabled, `POST /debug/persistentstate` returns the validated
logical database snapshot. State validation uses this endpoint instead of
parsing bbolt pages directly.
