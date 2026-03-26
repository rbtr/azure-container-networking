# cns/store — Bolt State Store & JSON Migration

## Motivation

CNS persists node-level state (Network Containers, secondary IPs, networks, endpoint mappings) to disk so it can recover after restarts, node reboots, and upgrades without re-querying the control plane for the full goal state.

Today this state is stored in flat JSON files:

| File | Contents |
|------|----------|
| `/var/lib/azure-network/azure-cns.json` | NCs, IP pool, networks, orchestrator context, metadata |
| `/var/run/azure-cns/azure-endpoints.json` | Container ID → pod endpoint IP mappings |

The JSON store has well-known reliability problems at scale:

- **Full-file rewrites on every mutation** — a single IP assignment rewrites the entire multi-MB state file, creating a crash-consistency window where a partial write corrupts all state.
- **No concurrent access safety** — the in-memory state and disk state can diverge under concurrent operations; the file mutex serializes all I/O.
- **O(n) reads for point lookups** — the entire file must be deserialized to look up a single NC or IP record.
- **Opaque to debugging** — the JSON blob is an undifferentiated monolith; there's no way to inspect or repair individual records without deserializing the whole file.

## Solution: boltdb

This package (`cns/store`) replaces the JSON store with [bbolt](https://github.com/etcd-io/bbolt), an embedded B+tree key/value database:

- **Per-record writes** — mutations touch only the affected key within a bucket (B+tree page), so a single IP assignment doesn't rewrite the entire database.
- **Crash-consistent** — boltdb uses copy-on-write with an OS-level fsync guarantee; partial writes never corrupt existing data.
- **Concurrent safe** — boltdb supports multiple concurrent readers with a single writer, protected by the database's internal locking.
- **O(log n) point lookups** — B+tree indexing means individual NC, IP, or endpoint lookups don't require scanning.
- **Inspectable** — standard boltdb CLI tools can dump, inspect, and repair individual buckets/keys.

## Package structure

```
cns/store/
├── doc.go              # Package documentation
├── types.go            # Record types (NCRecord, IPRecord, EndpointRecord, etc.)
├── bolt.go             # NCBoltStore & EndpointBoltStore implementations
├── bolt_test.go        # Tests for both stores
├── bolt_bench_test.go  # Benchmarks: JSON whole-map vs bolt per-record
├── migration.go        # MigrateCNSState & MigrateEndpointState
├── migration_test.go   # Migration tests using real JSON fixtures
├── BENCHMARKS.md       # Benchmark methodology, results, and analysis
├── README.md           # This file
└── benchmarks/         # Raw benchmark output files
```

### Stores

| Type | DB file | Buckets |
|------|---------|---------|
| `NCBoltStore` | `azure-cns.db` | `meta`, `network_containers`, `ips`, `networks`, `orchestrator_context`, `pnp_mac` |
| `EndpointBoltStore` | `azure-endpoints.db` | `meta`, `endpoints` |

Both stores are opened with `OpenNCStore` / `OpenEndpointStore` which return concrete types. There are no exported interfaces — consumers that need to mock should define their own interface at the call site (standard Go practice).

### Schema versioning

Every database file has a `meta` bucket containing a uint16 `version` key. On open, the store checks `version == SchemaVersion` and returns `ErrSchemaMismatch` if they differ. Increment `SchemaVersion` (currently `1`) when making incompatible changes to bucket layout or record encoding.

## Migration from JSON

`MigrateCNSState` and `MigrateEndpointState` perform a one-time migration from the legacy JSON files into the bolt stores:

1. Check the migration-complete marker in the `meta` bucket. If set, return immediately (idempotent).
2. Read and parse the JSON file using local compatibility structs (no import dependency on restserver).
3. Write each record individually into the appropriate bucket.
4. Rename the JSON file to `<path>.migrated` as a backup.
5. Set the migration-complete marker.

Migration is **idempotent** — it's safe to call on every startup. If the marker is already set, it's a no-op. If the JSON file doesn't exist (fresh node), it's a no-op.

## Rollout plan

The migration is being rolled out through the E2E pipeline with multiple validation modes to build confidence:

### Validation modes (pipeline)

| Mode | Abbrev | Behavior |
|------|--------|----------|
| `json-baseline` | M0 | Legacy JSON store only. Establishes baseline state validation summaries. |
| `migration-enabled` | M1 | CNS starts with bolt store + migration on first boot. State is validated post-disruption. |
| `migration-enabled-second-start` | M2 | Same as M1 but CNS is restarted a second time to verify bolt-only persistence (no JSON fallback). |
| `rollback-disabled` | M3 | Bolt store only, JSON migration path disabled. Proves the system works end-to-end without legacy state. Depends on M1's cluster. |

### Disruption matrix

Each mode is tested across disruption types (F1=restartCNS, F2=restartNode, F3=restartCNSDuringScale) and topologies (linux_podsubnet, linux_overlay, windows_podsubnet, windows_overlay) to ensure state survives real-world failure scenarios.

### Convergence testing

Post-disruption, CNS state and actual pod IPs may temporarily disagree. The test validation framework supports a convergence retry loop (controlled by `VALIDATE_CONVERGENCE_ATTEMPTS` × `VALIDATE_CONVERGENCE_INTERVAL_SECONDS`) that re-checks state until it stabilizes, with detailed mismatch categorization (missing, unexpected, duplicate IPs) for diagnostics.

### Current progress

| Milestone | Status | Description |
|-----------|--------|-------------|
| Bolt store implementation | ✅ Done | `cns/store/` package with NCBoltStore, EndpointBoltStore, full CRUD |
| Migration from JSON | ✅ Done | `MigrateCNSState`, `MigrateEndpointState` with idempotent marker system |
| Runtime integration | ✅ Done | EndpointBoltStore wired into CNS restserver, replacing JSON endpoint store |
| Per-record async writes | ✅ Done | `endpointWriter` does PutEndpoint/DeleteEndpoint per container (not whole-map) |
| Benchmarks | ✅ Done | Per-record bolt is 11–23× faster than JSON whole-map writes (see `BENCHMARKS.md`) |
| Test hardening (state validation) | ✅ Proposed upstream | Convergence loop, detailed IP comparison, summary emission |
| E2E validation pipeline (M0) | ✅ In follow up | JSON-baseline lanes with state validation after each disruption |
| Pipeline migration lanes (M1-M3) | 🔧 In bolt branch | Full matrix across modes × disruptions × topologies |
| NC state store migration | 🔧 Planned | Wire NCBoltStore into main CNS state path (lower priority — not IPAM hot path) |
| Gradual rollout | ❌ Not started | Feature flag in cns_config.json, staged AKS rollout |

### Remaining work

1. **NC state store migration**: Wire `NCBoltStore` into the main CNS state path (`saveState`/`restoreState` in `cns/restserver/util.go`). This is lower priority because the NC state write is not on the IPAM hot path — it only fires during NC creation/deletion, not per-pod IP assignment.

2. **Pipeline M1-M3 lanes**: The `set-cns-state-mode-template.yaml` patches the CNS configmap to enable migration mode. The `cniv2-template.yaml` has the `state_matrix_guard` job that enforces mode coverage. These are staged in the bolt branch pending the E2E validation.

3. **Feature flag & gradual rollout**: A config knob (`stateStoreBackend: "bolt"`) in cns_config.json to control which nodes use the new store. Initial rollout targets canary rings before broad enablement.

## Design decisions

- **Separate DB files for NC vs Endpoint state**: Mirrors the current JSON split. Endpoint state has different lifecycle characteristics (written by CNI plugin path) and keeping it separate avoids lock contention.

- **Concrete types, no exported interfaces**: The previous design had `NCStore`, `EndpointStore`, and `BucketReadWriter` interfaces declared by the producer package — a Go antipattern. These were removed. Consumers that need to mock should define their own interface at the call site.

- **Record types separate from cns.* API types**: Schema evolution in the store types can happen independently of the public REST API types, preventing coupling between on-disk format and wire format.

- **Migration uses local compatibility structs**: `migration.go` defines its own JSON deserialization structs that mirror the restserver wire format, avoiding an import cycle with the restserver package while precisely matching the on-disk JSON shape.
