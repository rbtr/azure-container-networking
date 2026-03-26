# Per-Record Bolt vs Whole-Map JSON: Endpoint Store Benchmarks

## Summary

The CNS endpoint store was migrated from a JSON whole-map model (serialize and
rewrite the entire `map[string]*EndpointInfo` on every mutation) to a boltdb
per-record model (write only the affected container's `EndpointRecord` on each
put/delete). This eliminates O(n) write amplification — bolt writes are O(1)
regardless of how many endpoints already exist.

## Environment

- **CPU**: AMD EPYC 7763 64-Core Processor
- **OS**: Linux (amd64)
- **Go**: 1.24.1
- **Disk**: Local SSD
- **Benchmark**: `go test ./cns/store/ -bench=. -benchmem -count=3`

## Results

### Adding One Endpoint to Existing State

This is the IPAM hot path — every `RequestIPConfigs` call writes one endpoint.

| Existing Endpoints | JSON (µs/op) | Bolt (µs/op) | Speedup | JSON Allocs | Bolt Allocs |
|-------------------:|-------------:|-------------:|--------:|------------:|------------:|
| 50                 |           91 |           33 |  **2.8×** |         259 |         113 |
| 100                |          165 |           33 |  **5.0×** |         509 |         113 |
| 250                |          387 |           34 |  **11×**  |       1,259 |         114 |
| 500                |          753 |           33 |  **23×**  |       2,509 |         114 |

**Key insight**: Bolt per-record writes take a constant ~33 µs regardless of
total state size. JSON write time grows linearly because it re-serializes and
rewrites the entire map every time.

At 250 endpoints (typical AKS node), bolt is **11× faster** and uses
**11× fewer allocations** per write.

### Whole-Map Write (Baseline — Old Pattern)

This measures the old pattern: serialize the full endpoint map and write to disk.

| Endpoints | JSON (µs/op) | Bolt per-record (µs/op) | Speedup |
|----------:|-------------:|------------------------:|--------:|
| 50        |           87 |                      34 | **2.6×** |
| 100       |          157 |                      34 | **4.6×** |
| 250       |          367 |                      34 | **11×**  |
| 500       |          727 |                      34 | **21×**  |

### Delete Endpoint

Releasing an IP and removing its endpoint state:

| Operation       | Latency (µs/op) | Allocs |
|-----------------|----------------:|-------:|
| Bolt PutEndpoint  |            34 |     98 |
| Bolt DeleteEndpoint |          26 |     89 |

### Concurrent Writes (250 Pre-Existing Endpoints)

Multiple IPAM requests arriving simultaneously (simulates pod burst scheduling):

| Goroutines | JSON + Mutex (µs/op) | Bolt per-record (µs/op) | Speedup |
|-----------:|---------------------:|------------------------:|--------:|
| 4          |                  432 |                      28 |  **15×** |
| 8          |                  430 |                      31 |  **14×** |
| 16         |                  430 |                      35 |  **12×** |
| 32         |                  433 |                      33 |  **13×** |

JSON requires an external mutex, so concurrent writers serialize completely.
Bolt handles its own concurrency internally. Under 32-goroutine contention,
bolt is **13× faster**.

### Memory Efficiency

| Metric             | JSON (250 eps) | Bolt per-record |
|--------------------|---------------:|----------------:|
| Bytes/op           |        107 KB  |          18 KB  |
| Allocs/op          |         1,259  |            114  |
| Write amplification| Entire map     |     Single key  |

## Scaling Characteristics

```
Write latency vs endpoint count:

  JSON:  O(n) — 87µs at 50, 727µs at 500 (linear growth)
  Bolt:  O(1) — 33-34µs constant at any size

            800 µs ┤                                          ╭──── JSON
                   │                                     ╭────╯
            600 µs ┤                                ╭────╯
                   │                           ╭────╯
            400 µs ┤                      ╭────╯
                   │                 ╭────╯
            200 µs ┤            ╭────╯
                   │       ╭────╯
             33 µs ┤───────────────────────────────────────── Bolt (constant)
                   └────┬────┬────┬────┬────┬────┬────┬────┬─
                       50  100  150  200  250  300  400  500
                                  Existing endpoints
```

## What This Means for CNS

On a typical AKS node with 250 endpoints:
- Every IP assignment saves **353 µs** of store write time (387→34 µs)
- Every IP release saves a similar amount
- Under burst scheduling (32 concurrent pods), saves **400 µs per request**
- Total memory allocation per write drops from **107 KB to 18 KB**

While the store write is a small fraction of total pod startup time (~2-5 ms
out of 5-15 seconds — see `test/integration/storebench/results/FINDINGS.md`),
the per-record model eliminates a source of write amplification, reduces GC
pressure, and removes the need for external mutexes around the endpoint store.

## Reproduction

```bash
# Run all store benchmarks
go test ./cns/store/ -bench=. -benchmem -count=3 -timeout=180s

# Run just the JSON vs bolt comparison
go test ./cns/store/ -bench='BenchmarkJSON_vs_Bolt_AddEndpoint' -benchmem -count=3

# Run concurrent benchmarks
go test ./cns/store/ -bench='Concurrent' -benchmem -count=3
```

## Raw Output

See `benchmarks/per_record_bolt_vs_json.txt` for full `go test -bench` output.
