# CNS Store Backend Benchmark: JSON vs BoltDB vs SQLite

## Background

CNS (Container Networking Service) persists IPAM state to local disk via the
`store.KeyValueStore` interface. The current implementation (`jsonFileStore` in
`store/json.go`) writes the **entire** state as a single JSON file on every
mutation, protected by a `sync.Mutex` and an OS-level process lock.

During IPAM operations (IP assignment and release), the `HTTPRestService` holds
its `sync.RWMutex` across the full store write path:

```
RequestIPConfigs → updateEndpointState → service.Lock() → EndpointStateStore.Write() → service.Unlock()
ReleaseIPConfigs → removeEndpointState → service.Lock() → EndpointStateStore.Write() → service.Unlock()
```

Every `Write()` call in the JSON store:
1. JSON-marshals the entire state map (`json.Marshal`)
2. Pretty-prints it with `json.MarshalIndent`
3. Creates a temp file, writes, closes, then atomically renames over the state file
4. Holds the store's `sync.Mutex` for the entire duration

On a typical node with 250–500 pod IPs, the serialized JSON state ranges from
500 KB to 5 MB. This creates an O(n) write amplification bottleneck on every
single IP allocate/release operation, serializing all concurrent IPAM requests.

## Hypothesis

Replacing the JSON file store with an embedded transactional database — one that
supports incremental key-value writes and provides its own thread safety — would:

1. **Reduce write latency** by eliminating full-state serialization and temp-file I/O
2. **Improve concurrent throughput** by removing the external `sync.Mutex` / `processlock` bottleneck
3. **Simplify the code** by eliminating manual locking in the store layer

## Candidates Evaluated

| Backend | Package | CGO | Description |
|---------|---------|-----|-------------|
| **JSON file** (baseline) | `store/json.go` | No | Current implementation. Full-state marshal + atomic file replace. |
| **BoltDB** | `go.etcd.io/bbolt` | No | B+ tree key-value store. MVCC with serialized writes. Used by etcd. |
| **SQLite** | `modernc.org/sqlite` | No | Pure-Go SQLite. WAL mode for concurrent reads. |

All three implement the same `store.KeyValueStore` interface, making them
drop-in replacements.

## Test Data

The benchmarks use a synthetic endpoint state map (`map[string]*EndpointInfo`)
that mirrors the real CNS `EndpointState` structure:

- Each endpoint has a container ID key, pod name, namespace, and interface-to-IP map
- Each interface carries IPv4 addresses, and ~30% have dual-stack IPv6
- Fields include `HostVethName`, `NetworkContainerID`, `NICType` (matching real `IPInfo`)
- State sizes of **50**, **250**, and **500** endpoints represent small, typical, and large nodes

## Benchmarks

### How to Run

```bash
# Run all benchmarks (takes ~10 minutes)
go test ./store/ -bench=BenchmarkStore -benchmem -count=3 -timeout=600s

# Run correctness + concurrency tests
go test ./store/ -run=TestStoreBackend -v

# Run a specific benchmark
go test ./store/ -bench=BenchmarkStoreWrite -benchmem
```

### Benchmark Descriptions

| Benchmark | What it Measures | CNS Scenario |
|-----------|-----------------|--------------|
| `BenchmarkStoreWrite` | Single-threaded write of full endpoint state at varying sizes (50/250/500 endpoints) | `updateEndpointState()` / `removeEndpointState()` hot path |
| `BenchmarkStoreRead` | Single-threaded read + unmarshal of full endpoint state | `restoreState()` at CNS startup |
| `BenchmarkStoreConcurrentWrite` | Parallel writes from 4/16/64 goroutines against a 250-endpoint state | Concurrent pod scheduling — multiple kubelet IPAM requests hitting CNS simultaneously |
| `BenchmarkStoreWriteRead` | Interleaved parallel ops: 80% reads, 20% writes against 250-endpoint state | Steady-state IPAM workload (frequent status queries, periodic allocations) |
| `BenchmarkStoreIncrementalWrite` | Sequential add-one-endpoint-then-write-all, growing from 0 to b.N endpoints | Pod scheduling burst — the O(n) write amplification problem |

### Correctness Tests

| Test | What it Verifies |
|------|-----------------|
| `TestStoreBackendCorrectness` | Write→Read round-trip, data integrity, key-not-found semantics, overwrite behavior, Exists() |
| `TestStoreBackendConcurrency` | 10 concurrent writers + 10 concurrent readers with no data corruption or errors |

## Results

**Environment:** AMD EPYC 7763 64-Core, Linux, Go 1.24.1, local SSD

### Write Latency (single-threaded, ns/op — lower is better)

| Endpoints | JSON | BoltDB | SQLite | BoltDB vs JSON | SQLite vs JSON |
|-----------|------|--------|--------|----------------|----------------|
| 50 | 367 µs | 88 µs | 118 µs | **4.2× faster** | **3.1× faster** |
| 250 | 1,582 µs | 381 µs | 764 µs | **4.2× faster** | **2.1× faster** |
| 500 | 3,116 µs | 799 µs | 1,294 µs | **3.9× faster** | **2.4× faster** |

BoltDB is consistently **~4× faster** than JSON for writes. The JSON store's
cost scales linearly because it must re-serialize and rewrite the entire state;
BoltDB only needs to update the affected B+ tree page and fsync.

### Read Latency (ns/op)

| Endpoints | JSON | BoltDB | SQLite |
|-----------|------|--------|--------|
| 50 | 299 µs | 248 µs | 294 µs |
| 250 | 1,493 µs | 1,237 µs | 1,510 µs |
| 500 | 2,999 µs | 2,488 µs | 2,951 µs |

Reads are dominated by `json.Unmarshal` cost (all backends still store JSON
blobs), so improvements are modest. BoltDB is ~17% faster; SQLite is roughly
the same as JSON. Note: the JSON store's Read uses an in-memory cache after
the first load, so this measures the hot-cache deserialization path.

### Concurrent Write Throughput (250 endpoints, ns/op — lower is better)

| Goroutines | JSON | BoltDB | SQLite |
|------------|------|--------|--------|
| 4 | 1,654 µs | 218 µs | 633 µs |
| 16 | 1,657 µs | 198 µs | 622 µs |
| 64 | 1,700 µs | 200 µs | 625 µs |

This is the most important benchmark for IPAM performance. Under concurrent
write contention:

- **BoltDB: 8.4× faster** than JSON — BoltDB's MVCC and page-level writes
  handle contention far better than the JSON store's mutex + full-file rewrite.
- **SQLite: 2.7× faster** — improved over JSON but limited by `MaxOpenConns=1`
  needed to prevent `SQLITE_BUSY` errors.
- JSON throughput is flat regardless of concurrency because the `sync.Mutex`
  serializes all writers.

### Mixed Read/Write (80% read / 20% write, 250 endpoints, parallel)

| Backend | ns/op | vs JSON |
|---------|-------|---------|
| JSON | 1,420 µs | baseline |
| BoltDB | 430 µs | **3.3× faster** |
| SQLite | 731 µs | **1.9× faster** |

In a realistic mixed workload, BoltDB's MVCC allows readers to proceed without
blocking on writers, giving a **3.3× throughput improvement**.

### Incremental Write (growing state, amortized ns/op)

| Backend | ns/op | Allocs/op | Memory/op |
|---------|-------|-----------|-----------|
| JSON | 9,109 µs | 8,361 | 1.83 MB |
| BoltDB | 7,305 µs | 20,513 | 3.00 MB |
| SQLite | 6,650 µs | 12,368 | 1.13 MB |

This models a pod scheduling burst: each iteration adds one endpoint and writes
the full state. All backends exhibit O(n) growth since they still write the
entire state blob, but SQLite and BoltDB amortize the I/O cost better. A future
optimization (per-key writes rather than full-state blobs) would benefit the
database backends disproportionately.

## Analysis

### BoltDB is the clear winner

| Dimension | BoltDB Advantage |
|-----------|-----------------|
| Single-threaded writes | **4× faster** at all state sizes |
| Concurrent writes (real-world IPAM) | **8× faster** under contention |
| Mixed read/write | **3.3× faster** |
| Reads | ~17% faster |
| Thread safety | Built-in (MVCC transactions) — no external mutex needed |
| Lock()/Unlock() | No-op — transactions provide isolation |
| Dependencies | Single package (`go.etcd.io/bbolt`), no CGO, battle-tested by etcd |

### SQLite is a viable second choice

- 2–3× faster writes than JSON, but consistently half the improvement of BoltDB
- Read performance identical to JSON (both dominated by `json.Unmarshal` of a BLOB)
- Requires `MaxOpenConns=1` and explicit transaction management to avoid `SQLITE_BUSY`
- Heavier dependency footprint (`modernc.org/sqlite` pulls in `modernc.org/libc`, `modernc.org/cc`, etc.)

### Both alternatives eliminate locking complexity

The JSON store requires two layers of locking:
1. `sync.Mutex` in `jsonFileStore` — protects the in-memory data map
2. `processlock.Interface` — OS-level file lock for multi-process safety

Both BoltDB and SQLite handle concurrency internally, so their `Lock()`/`Unlock()`
methods are no-ops. This simplifies the CNS code that calls the store — the
`HTTPRestService.RWMutex` could potentially be scoped more narrowly if the store
itself guarantees thread safety.

### Where JSON marshal/unmarshal still dominates

All three backends currently store values as JSON blobs. The read path
(`json.Unmarshal`) accounts for the vast majority of read latency, which is why
read improvements are modest. A future optimization could store structured data
natively (BoltDB nested buckets, SQLite columns), eliminating the marshal/unmarshal
overhead and unlocking per-field queries.

## Files

| File | Description |
|------|-------------|
| `store/store.go` | `KeyValueStore` interface definition |
| `store/json.go` | JSON file store (baseline) — existing implementation |
| `store/bolt.go` | BoltDB backend — `NewBoltStore(filePath)` |
| `store/sqlite.go` | SQLite backend — `NewSQLiteStore(filePath)` |
| `store/store_bench_test.go` | All benchmarks and correctness tests |

## Recommendation

Adopt **BoltDB** (`go.etcd.io/bbolt`) as the CNS persistent store backend. The
benchmark data strongly validates the hypothesis: an embedded transactional
datastore significantly outperforms the JSON file model, especially under the
concurrent write contention that characterizes real-world pod scheduling.

The migration path is straightforward since BoltDB implements the same
`KeyValueStore` interface — it is a drop-in replacement at the constructor level.

---

## Update: Per-Record Bolt Store (cns/store)

> **This document covers the old KV-wrapper benchmarks** where bolt still
> serialized the entire endpoint map as a single value. A new per-record model
> in `cns/store/` goes further — each endpoint is an independent key in a bolt
> bucket, so mutations touch only the affected record.
>
> Per-record bolt results at 250 endpoints:
> - **11× faster** than JSON whole-map writes (34 µs vs 387 µs)
> - **O(1) write time** regardless of state size (constant ~33 µs)
> - **11× fewer allocations** per write (114 vs 1,259)
>
> See **`cns/store/BENCHMARKS.md`** for full per-record benchmark data.
