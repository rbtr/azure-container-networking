# Pod Startup Latency (Pod-SLO) Workstream

**Status:** Investigation complete; root cause identified as kernel
RTNL contention. Numerous mitigations explored. **Result: nothing
improves pod startup SLI beyond baseline.** The RTNL mutex is the
floor.

## Question

> How fast can a pod go from `Pod.metadata.creationTimestamp` to
> `Pod Ready=True` in an Azure CNI overlay AKS cluster, and what
> can we do to make it faster?

This is the **pod-slo** workstream. A *separate*
[node-readiness](https://github.com/rbtr/azure-container-networking/tree/experiment/node-readiness)
workstream tracks the **node-readiness** question (how fast a new
node can go from `creationTimestamp` to `Node Ready=True`). The two
workstreams share tooling but have different bottlenecks and
mitigation surfaces.

## TL;DR

**Original hypothesis:** Replacing the CNS JSON file store with an
embedded transactional database (BoltDB or SQLite) would improve pod
startup latency.

**Outcome:**
1. **Hypothesis correct at micro-benchmark scale**: BoltDB is 4–23×
   faster than JSON for individual store transactions; per-record
   BoltDB achieves O(1) writes vs O(n) for JSON.
2. **Hypothesis incorrect at cluster scale**: store backend has
   **no measurable impact** on pod startup SLI. Store writes are
   0.01–0.06% of end-to-end pod startup time.
3. **True bottleneck identified**: kernel RTNL mutex contention.
   Every netlink operation (veth create, link state, MTU, routes,
   netns move) acquires the global RTNL lock. With N concurrent CNI
   processes, each pod's network setup degrades from ~130 ms
   (uncontended) to ~6.5 s (150 pods contending).
4. **RTNL bottleneck is fundamental**: Flannel (vxlan) on the same
   cluster produces statistically identical pod startup latencies.
   The bottleneck is beneath the CNI layer.
5. **Mitigations explored but ineffective**: per-record BoltDB,
   netlink message batching, cross-process CNI semaphore, CNS-side
   veth pool, CNS-managed endpoint creation, kernel/kubelet/
   containerd tuning, kernel upgrades. None broke the floor.
6. **BoltDB migration is still recommended for code quality**: per-
   record CRUD, O(1) writes, eliminates external mutexes, reduces
   GC pressure ~11×, future-proofs against larger node sizes. Just
   don't expect a perf win.

Full report:
[`docs/cns-ipam-performance-investigation.md`](docs/cns-ipam-performance-investigation.md)
(611 lines).

## What's in this branch

This branch is an **index** for the pod-SLO workstream, not a code
branch. The actual experimental code lives in separate branches on
the same fork (`rbtr/azure-container-networking`):

| Branch | Contents |
|---|---|
| [`feat/bolt-store`](https://github.com/rbtr/azure-container-networking/tree/feat/bolt-store) | Per-record BoltDB store implementation in `cns/store/`. KeyValueStore interface adapter + per-record CRUD model. The path of least disruption to ship persistence improvements. |
| [`feat/bolt-migration`](https://github.com/rbtr/azure-container-networking/tree/feat/bolt-migration) | JSON → Bolt migration tooling. One-shot reads the existing `azure-cns.json` / `azure-endpoints.json` blobs and writes per-record bolt buckets. Idempotent; safe to run on each startup. |
| [`performance-research`](https://github.com/rbtr/azure-container-networking/tree/performance-research) | The comprehensive perf investigation: netlink batching (`netlink/batch_linux.go`), CNI semaphore (`cni/network/endpoint_semaphore_linux.go`), CNS veth pool (`cns/vethpool/pool_linux.go`), cluster benchmark harness (`test/integration/storebench/`), and the cluster-bench result corpus. **Also has a copy of the comprehensive investigation doc** that this branch is curated around. |
| [`localdb-experiment`](https://github.com/rbtr/azure-container-networking/tree/localdb-experiment) | SQLite store experiments and a runtime test plane. Earlier exploration; superseded by `feat/bolt-store` for production work but useful as a reference for the SQLite path. |
| [`ipam-math-v2`](https://github.com/rbtr/azure-container-networking/tree/ipam-math-v2) | IPAM pool scaling math redesign. Tangentially related — about right-sizing the IP pool, not about per-pod latency. |
| [`simplify-ipam`](https://github.com/rbtr/azure-container-networking/tree/simplify-ipam) | IPAM code-path simplification. Quality-of-life refactor; touches some of the same files as `feat/bolt-store`. |

### `docs/cns-ipam-performance-investigation.md`

The canonical research report. Covers:

1. Experimental infrastructure (cluster, harness, methodology)
2. Phase 1 — store-backend micro and cluster benchmarks (JSON / Bolt
   / SQLite, three pod scales, multiple SKUs)
3. Phase 2 — per-record BoltDB integration (cluster results
   matching the baseline within noise)
4. Phase 3 — RTNL contention identification (profiling, lock
   tracing, per-syscall budgets)
5. Phase 4 — RTNL mitigation attempts (netlink batching, CNI
   semaphore, veth pool, CNS-side netlink). None broke the floor.
6. Phase 5 — reference-CNI comparison (Flannel on the same cluster
   reproduces our latency floor).
7. Recommendation: ship BoltDB for code quality, accept the RTNL
   floor for SLI.

### Shared dev tooling (also on `experiment/node-readiness`)

- `.github/lsp.json` — gopls Language Server config for Copilot CLI
- `.github/scripts/setup-go-tooling.sh` — one-time gopls installer
- `.mcp.json` — gopls MCP server config (gopls v0.20+)
- `.github/gopls-mcp-instructions.md` — model instructions
- `agents.md` — expanded with Go code-intelligence guidance

## Resume guide

If you're picking this back up later:

1. **Read `docs/cns-ipam-performance-investigation.md`** end-to-end.
   It's the canonical writeup with experimental details, data
   tables, and exact methodology.
2. **Decide on the BoltDB migration**. The branches
   `feat/bolt-store` and `feat/bolt-migration` are mergeable as-is
   (modulo rebase) and ship the code-quality win independently of
   any perf hypothesis. If you want it for the right reasons
   (correctness, debuggability, GC pressure) — merge them.
3. **Decide on the RTNL mitigation strategy**. The mitigations on
   `performance-research` (netlink batching, CNI semaphore, veth
   pool) do reduce some metrics under specific conditions but
   none broke the SLI floor. Whether to merge them depends on
   whether you want the marginal gains or whether you want to
   focus elsewhere.
4. **Cluster benchmark harness is reusable**. Lives at
   `test/integration/storebench/` on `performance-research`. If
   you want to validate any change against pod startup SLI in a
   real cluster, run it.
5. **Consider revisiting in 1–2 kernel releases**. RTNL has
   ongoing upstream work (per-link locking, lockless paths in
   `nf_tables`). The conclusion "RTNL is the floor" is true for
   kernel 6.8; future kernels may move the floor.

## Open / abandoned questions

- **Why doesn't Bolt buy us anything at cluster scale?** We have a
  satisfying answer (store writes are < 0.1% of pod startup) but
  it's worth re-validating on a different workload geometry where
  store throughput might matter — e.g. very high pod churn,
  pre-restart endpoint replay, etc.
- **Is the RTNL floor actually fundamental?** We confirmed it via
  Flannel comparison, but more reference CNIs (Calico-vxlan,
  Cilium without eBPF datapath) would strengthen the claim.
- **What's the actual breakdown of the ~130 ms uncontended pod
  network setup?** Pulled out by phase in
  `cns-ipam-performance-investigation.md` but not profiled at
  syscall granularity.

## Related workstream

- **[`experiment/node-readiness`](https://github.com/rbtr/azure-container-networking/tree/experiment/node-readiness)**
  — sibling workstream investigating node-readiness (not
  pod-Ready) latency. Different bottleneck (kubelet pod-sync
  pipeline + container exec gap rather than RTNL); different
  mitigation surface (static-pod / systemd-unit / VHD bake-in
  rather than persistence backend / netlink batching).
- **PR #4398** (`feat/cns-bootstrap-metrics`) — adds Prometheus
  metrics for CNS bootstrap phases. Started on the
  node-readiness path but the metrics are also useful for
  pod-SLO dashboards (especially `cns_state_persist_*` follow-up
  metrics — *deferred* in the open PR but applicable here).

## Test clusters used during investigation (all torn down)

`Standard_B2s`, `Standard_D8ads_v6`, `Standard_D8ads_v7` clusters
across multiple regions. Specifics are in the investigation doc.
