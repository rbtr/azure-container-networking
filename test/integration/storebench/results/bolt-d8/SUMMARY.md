# CNS Per-Record BoltDB Cluster Benchmark Results

## Test Configuration

- **CNS Image:** `acnpublic.azurecr.io/azure-cns:linux-amd64-v1.8.3-11-gf1f6af34f`
- **Store Backend:** Per-record BoltDB (EndpointBoltStore with async writer)
- **Node:** Standard_D8ads_v6, AKS canadacentral
- **CNI:** Stateless azure-vnet + azure-cns IPAM (overlay mode)
- **Date:** 2026-03-26
- **Runs per scale:** 3

## Pod Startup Latency (Kubelet SLI)

The kubelet `pod_start_sli_duration_seconds` metric measures end-to-end pod startup
from API server pod creation to container running. This is the most reliable metric.

| Scale | Run 1 | Run 2 | Run 3 | **Mean** |
|------:|------:|------:|------:|---------:|
| 50    | 10.21s | 10.17s | 10.12s | **10.16s** |
| 100   | 16.74s | 16.76s | 16.12s | **16.54s** |
| 150   | 23.61s | 23.79s | 23.36s | **23.59s** |

## Pod Startup Percentiles (from pod timestamps)

| Scale | P50 | P95 | P99 | Max |
|------:|----:|----:|----:|----:|
| 50    | 9.3s | 13.2s | 13.3s | 13.7s |
| 100   | 15.5s | 24.7s | 25.3s | 25.7s |
| 150   | 23.0s | 36.0s | 37.2s | 38.0s |

## Comparison with Previous Baseline (D8ads_v7, JSON store)

> **Note:** Different VM SKU and region — not a direct apples-to-apples comparison.
> The D8ads_v7 baseline was run on a different cluster in westus2.

| Scale | Bolt D8ads_v6 (SLI Mean) | JSON D8ads_v7 (SLI Mean) | BBolt D8ads_v7 (SLI Mean) |
|------:|-------------------------:|-------------------------:|--------------------------:|
| 50    | 10.16s                   | 6.14s                    | 6.30s                     |
| 100   | 16.54s                   | 8.84s                    | 9.54s                     |
| 150   | 23.59s                   | —                        | —                         |
| 200   | —                        | 14.72s                   | 15.68s                    |

The D8ads_v6 shows ~65-85% higher latencies than D8ads_v7 at equivalent scales. This is
consistent with VM SKU differences (v6 vs v7 generation) rather than store backend changes.
The v7 baseline showed **no measurable difference** between JSON, BBolt, and SQLite backends.

## Key Observation

**Latency scales linearly with pod count**: ~0.16s per additional pod. This matches the
RTNL lock contention model (each additional concurrent CNI process adds ~one netlink
serialization slot of ~160ms). The store backend (bolt vs JSON) remains immaterial to
pod startup latency — the improvement is in code maintainability and thread-safety, not
end-to-end performance.

## Raw Data

See `wall-clock.csv` and individual `result-bolt-*.json` files for full per-pod latency data.

---
*Generated from storebench_test.go run on 2026-03-26*
