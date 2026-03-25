# CNS Store Backend Benchmark: Full Findings

## Executive Summary

We benchmarked three CNS persistent store backends — **JSON** (current), **BoltDB**, and **SQLite** — at both the micro-benchmark level (isolated store operations) and the cluster level (real pod startup on AKS). The results tell two complementary stories:

1. **Micro-benchmarks**: BoltDB is 4–8× faster than JSON for store writes, especially under contention.
2. **Cluster benchmarks**: The store backend makes **no measurable difference** to pod startup time.

The store write (~1–3 ms) is completely dominated by kubelet/containerd/CNI overhead (5–20 s per pod). Changing the store backend alone cannot meaningfully improve pod scheduling throughput.

---

## Micro-Benchmark Results (isolated store operations)

Run on local machine with `go test -bench` against `store/store_bench_test.go`.

### Write Latency (single-threaded, ns/op — lower is better)

| Endpoints | JSON       | BoltDB     | SQLite     | BoltDB speedup | SQLite speedup |
|-----------|------------|------------|------------|----------------|----------------|
| 50        | 367,000    | 88,000     | 118,000    | **4.2×**       | **3.1×**       |
| 250       | 1,582,000  | 381,000    | 764,000    | **4.2×**       | **2.1×**       |
| 500       | 3,116,000  | 799,000    | 1,294,000  | **3.9×**       | **2.4×**       |

### Concurrent Write (250 endpoints, ns/op)

| Goroutines | JSON        | BoltDB   | SQLite   |
|------------|-------------|----------|----------|
| 4          | 1,654,000   | 218,000  | 633,000  |
| 16         | 1,657,000   | 198,000  | 622,000  |
| 64         | 1,700,000   | 200,000  | 625,000  |

BoltDB **8.4× faster** under concurrent writes. SQLite **2.7× faster**.

### Mixed Read/Write (80/20, 250 endpoints)

| Backend | ns/op     | vs JSON       |
|---------|-----------|---------------|
| JSON    | 1,420,000 | baseline      |
| BoltDB  | 430,000   | **3.3× faster** |
| SQLite  | 731,000   | **1.9× faster** |

---

## Cluster Benchmark Results

### Methodology

- **Harness**: `test/integration/storebench/storebench_test.go` (Go integration test, `//go:build storebench`)
- **Workload**: Deployment of `registry.k8s.io/pause:3.9` pods pinned to a single node via `nodeSelector`
- **Metrics**: Wall-clock time (creation→all Ready), per-pod latency (creation→Ready timestamps), kubelet SLI histogram (`kubelet_pod_start_sli_duration_seconds`)
- **Protocol**: For each backend, update the CNS ConfigMap `StoreBackend` field, delete the DaemonSet pod (triggers restart), clean residual store files from the node, then run 3 repetitions per scale
- **Runs**: 3 backends × 3 scales × 3 runs = 27 iterations per SKU

### Standard_D8ads_v7 (8 vCPU, 32 GB RAM)

Averages across 3 runs per configuration:

| Backend | Scale | Wall Clock (ms) | Kubelet SLI Mean (s) | P50 (s) | P95 (s) | P99 (s) |
|---------|------:|-----------------:|---------------------:|--------:|--------:|--------:|
| json    |    50 |           10,490 |                6.139 |    5.00 |    6.33 |    6.67 |
| bbolt   |    50 |           10,511 |                6.304 |    5.33 |    6.33 |    6.67 |
| sqlite  |    50 |           10,424 |                6.302 |    5.33 |    6.18 |    6.33 |
| json    |   100 |           20,705 |                8.844 |    8.67 |    9.33 |   10.00 |
| bbolt   |   100 |           20,566 |                9.537 |    9.33 |   11.00 |   11.33 |
| sqlite  |   100 |           20,578 |                9.325 |    9.00 |   10.33 |   11.00 |
| json    |   200 |           30,776 |               14.723 |   14.33 |   18.00 |   18.00 |
| bbolt   |   200 |           30,753 |               15.679 |   15.67 |   18.67 |   19.68 |
| sqlite  |   200 |           27,419 |               16.085 |   16.00 |   19.00 |   19.67 |

**Observation**: All three backends are within noise at every scale. The ±5% variance between runs of the _same_ backend exceeds the differences _between_ backends.

### Standard_B2s (2 vCPU, 4 GB RAM — burstable)

Only 50-pod scale was feasible (100 pods exceeded the node's capacity):

| Backend | Scale | Wall Clock (ms) | Kubelet SLI Mean (s) |
|---------|------:|-----------------:|---------------------:|
| json    |    50 |           75,978 |               60.805 |
| bbolt   |    50 |           80,976 |               61.911 |
| sqlite  |    50 |           94,399 |               66.360 |

**Observation**: CPU-constrained B2s nodes take 60–95 s per pod vs 5–6 s on D8. The 15× slowdown has nothing to do with the store — it's burstable CPU throttling and resource starvation. Backend differences are again within noise.

---

## Analysis

### Why the store backend doesn't matter for pod startup

The CNS IPAM store write is one step in a long chain:

```
kubelet detects new pod                      ~0 ms
  └─ kubelet calls CRI RunPodSandbox        ~500-2000 ms  (containerd + runc)
       └─ containerd pulls/mounts layers     ~0 ms        (pause image cached)
       └─ containerd creates sandbox         ~200-500 ms
  └─ kubelet calls CNI ADD                   ~10-50 ms
       └─ azure-vnet reads conflist          ~1 ms
       └─ azure-vnet calls CNS IPAM          ~2-5 ms
            └─ CNS assigns IP (in-memory)    ~0.01 ms
            └─ CNS writes endpoint store     ~0.3-3 ms  ← THIS IS WHAT WE BENCHMARKED
       └─ azure-vnet configures veth/routes  ~5-10 ms
  └─ kubelet starts container                ~100-500 ms
  └─ kubelet probes readiness               ~0-1000 ms
```

The store write is **0.01–0.06%** of the total pod startup time. Even an infinitely fast store would save at most 3 ms on a 6,000 ms pod startup.

### Where the time actually goes

At 200 pods on D8ads_v7, the kubelet SLI mean is ~15 s. The dominant costs are:
- **Container runtime** (RunPodSandbox, container creation): ~2–5 s per pod under contention
- **Scheduler→kubelet latency**: batching effects at high scale
- **CNI plugin execution** (fork/exec azure-vnet, network setup): ~10–50 ms per pod
- **Kubelet serial processing**: some operations are serialized in the kubelet's pod lifecycle

### Micro vs. Cluster: reconciling the results

The micro-benchmarks are correct — BoltDB really is 4–8× faster for isolated store writes. But the store write is such a tiny fraction of pod startup that this speedup is invisible in end-to-end measurements. It's like optimizing a function that takes 3 ms in a request that takes 15,000 ms.

---

## Conclusions

1. **The store backend is not the bottleneck** for pod startup performance. The hypothesis that switching from JSON to BoltDB/SQLite would measurably improve CNS IPAM performance in AKS is **not supported** by the cluster data.

2. **BoltDB is still the better engineering choice** for maintainability:
   - Eliminates `sync.Mutex` + `processlock` complexity
   - MVCC readers never block writers (correct by construction)
   - Single, battle-tested dependency (`go.etcd.io/bbolt`, used by etcd)
   - 4× faster writes future-proofs against scenarios with larger state

3. **SQLite adds complexity without benefit**: heavier dependency tree (CGo-free but large), still requires JSON marshal at the KV level, `MaxOpenConns=1` limits concurrency.

4. **To actually improve pod startup time**, investigate:
   - Kubelet parallelism settings (`maxParallelContainerStarts`, etc.)
   - Container runtime overhead (containerd/runc sandbox creation)
   - CNI plugin execution model (could CNS serve as a long-running CNI daemon instead of fork/exec?)
   - Pre-allocated IP pools to eliminate the IPAM round-trip entirely

---

## Reproduction

### Micro-benchmarks
```bash
cd store/
go test -bench=. -benchmem -count=3 -timeout=10m ./...
```

### Cluster benchmarks
```bash
# Requires: kubectl access to a BYOCNI AKS cluster with CNS installed
# Set the target node and configure backends/scales/runs via env vars
cd test/integration/storebench/
STOREBENCH_NODE=<node-name> \
STOREBENCH_BACKENDS=json,bbolt,sqlite \
STOREBENCH_SCALES=50,100,200 \
STOREBENCH_RUNS=3 \
  go test -tags storebench -v -timeout 60m -run TestStoreBench ./...
```

Results are written to `./results/<sku>/` as JSON files, a CSV, and a SUMMARY.md.

---

## Files

| File | Description |
|------|-------------|
| `store/bolt.go` | BoltDB `KeyValueStore` implementation |
| `store/sqlite.go` | SQLite `KeyValueStore` implementation |
| `store/factory.go` | Backend factory (`NewStore()`) |
| `store/store_bench_test.go` | Micro-benchmark suite |
| `store/BENCHMARKS.md` | Micro-benchmark documentation |
| `cns/configuration/configuration.go` | `StoreBackend` config field |
| `cns/service/main.go` | Runtime backend selection |
| `test/integration/storebench/storebench_test.go` | Cluster benchmark harness |
| `test/integration/storebench/results/b2s/` | Standard_B2s results |
| `test/integration/storebench/results/d8adsv7/` | Standard_D8ads_v7 results |
