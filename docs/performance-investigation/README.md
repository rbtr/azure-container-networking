# AKS Container Networking Performance Investigation

A multi-month effort to characterize and improve pod and node startup
latency in AKS clusters running Azure CNI. This directory consolidates
the experimental record across two distinct workstreams.

## Reading order

1. **[Executive Summary](./00-executive-summary.md)** — high-level
   findings, trends across all experiments, and current
   recommendations. **Start here.**
2. **[Lab 1 — Pod startup latency](./01-pod-slo.md)** — store
   backends, RTNL contention, and the kernel SLI floor.
3. **[Lab 2 — Node readiness](./02-node-readiness.md)** — phase
   decomposition of node-init, the static-pod blocker, and the
   nodeinit-bench tooling.
4. **[Lab 3 — CNS bootstrap metrics](./03-bootstrap-metrics.md)** —
   PR #4398: 16 Prometheus metrics that replaced log-parsing as the
   primary observability path.
5. **[Lab 4 — Embedded CNI POC](./04-embed-cni-poc.md)** — embedding
   the CNI installer in the CNS image as a `cns deploy` subcommand,
   plus the measured 26 s → 9 s `node-ready` improvement.

Each lab writeup follows the same structure: **hypothesis →
experiment → data → conclusion**, with tables and (where
appropriate) Mermaid charts for visualization.

## Source data

- Source branches on the `rbtr` fork:
  - [`experiment/pod-slo`](https://github.com/rbtr/azure-container-networking/tree/experiment/pod-slo) — pod-SLO workstream index
  - [`experiment/node-readiness`](https://github.com/rbtr/azure-container-networking/tree/experiment/node-readiness) — node-readiness workstream + `tools/nodeinit-bench`
  - [`feat/bolt-store`](https://github.com/rbtr/azure-container-networking/tree/feat/bolt-store) — per-record BoltDB implementation
  - [`performance-research`](https://github.com/rbtr/azure-container-networking/tree/performance-research) — RTNL mitigations + cluster bench harness
  - [`experiment/cns-embed-cni`](https://github.com/rbtr/azure-container-networking/tree/experiment/cns-embed-cni) — embedded CNI POC
- Upstream PRs:
  - **[Azure PR #4398](https://github.com/Azure/azure-container-networking/pull/4398)** — `feat/cns-bootstrap-metrics` (open)
- Tooling:
  - `tools/nodeinit-bench/` on `experiment/node-readiness` — the
    measurement CLI used for all node-init data here.

## Conventions used in these docs

- All durations are **seconds** unless otherwise noted.
- Phase boundaries are anchored at `Node.metadata.creationTimestamp = T0`.
- "p50 / p95 / max" refer to percentiles across N observations
  unless explicitly labeled otherwise.
- "Cold start" = no prior CNS run on the node (no `/var/lib/azure-network`
  persistence). "Warm restart" = CNS pod restart on an existing node.
- "Stock CNS" = the Azure CNI Overlay (Cilium) DaemonSet deployed by AKS
  managed clusters. "BYOCNI" = `--network-plugin none --network-plugin-mode overlay`
  via `make -C hack/aks overlay-byocni-up`.
