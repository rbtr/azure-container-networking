# nodeinit-bench

Reusable test methodology for measuring Node initialization latency on AKS
clusters using Azure CNI / CNS in Overlay mode. Produces the data needed to
render a Gantt chart of every observable network component involved in bringing
a Node to `Ready`.

## What it measures

Per newly-created Node, `nodeinit-bench` records the following spans
(all anchored at `node.metadata.creationTimestamp` unless noted):

1. `vm-provision` — `az aks nodepool scale` invocation → Node
   `creationTimestamp`. Covers ARM accept, VMSS new-VM provisioning,
   OS boot, CSE / cloud-init, and kubelet registering the Node. This
   is upstream of CNS and DNC-RC; it dominates the wall-clock from
   submit to first observable Kubernetes event but is **not** part of
   the Node Ready SLI (which is anchored at `T0 = creationTimestamp`).
2. `node-registered` — Node `Starting` → `RegisteredNode` event.
3. `dnc-rc-create-nnc` — Node `creationTimestamp` → `CreatedNNC` event.
   Pure DNC-RC reaction: see Node watch event, create NNC CRD, post the
   event. Bounded below by Kubernetes' 1s event-time resolution.
4. `dnc-rc-create-nc` — `CreatingNC` event → `UpdatedNC` event. Covers DNC-RC
   dispatch to DNC-agent and NMAgent publication of the NC.
5. `nnc-status-written` — NNC creation → NNC status write by `dnc-rc` (from
   `managedFields` on the NNC object).
6. `cns-pod-schedule-latency` — T0 → Pod `Scheduled`.
7. `cns-init-image-pull` — init container `Pulling` → `Pulled`. Matched
   to the init container by the image reference inside the event message,
   so a separate non-CNS init container's pull won't be misattributed.
8. `cns-init-container-run` — init container `Started` → `finishedAt`.
   Real run time of the one-shot CNI binary installer; ~0-1 s.
9. `cns-init-to-main-gap` — init container `finishedAt` → main container
   `Pulled` event. Kubelet's pod-sync pipeline between phases:
   recognize init done, admission for main, call containerd to verify
   image presence, fire `Pulled`. Under fresh-node daemonset stampede
   this is 6-13 s of pure scheduler/containerd backpressure with no
   real work happening.
10. `cns-image-pull` — main `cns-container` image. Same image-name match
    as `cns-init-image-pull`. When kubelet emits only a `Pulled` event
    with no preceding `Pulling` (the "Container image already present on
    machine" case, common on AKS where the cns image is preloaded in the
    node image), this span renders as zero-duration anchored at the
    Pulled event time, **not Missing**. Genuine pulls render with the
    real Pulling→Pulled duration.
10. `cns-container-start` — cns `Pulled` → `containerStatuses[0].startedAt`.
11. `cns-exec-gap` — cns container `startedAt` → first `[Azure CNS] Using
    config` log line. The kernel/containerd not having actually exec'd
    the Go binary yet — i.e., the daemonset-stampede gap on a fresh node.
    Near-zero on a quiet node; **dominant** during fresh-node startup.
12. `cns-process-bootstrap` — first CNS log → `Reconciling initial CNS
    state` log line. Real CNS-side work between first log and first NNC
    delivery: read `cns_config.json`, init telemetry, build zap logger,
    init nmagent client, acquire process lock, controller-runtime cache
    sync, receive first NNC.
12. `cns-nnc-ingest` — `Retrieved NNC` → `Reconciling CNS IPAM state` log lines.
13. `cns-sync-host-nc-version` — cumulative time CNS spent in
    `syncHostNCVersion`. Derived from the
    `sync_host_nc_version_latency_seconds` histogram **sum**, anchored at
    the end of `cns-nnc-ingest`. The FIRST sync call is always the slow
    path: local HostVersion is initialized to `-1` and DNC publishes `0`,
    so CNS calls NMAgent to confirm v0 before updating HostVersion and
    triggering conflist generation. **In the critical path for Node Ready
    in BOTH overlay and Swift modes.** Subsequent ticker calls
    short-circuit when no NCs are outdated, so the histogram sum is
    dominated by the first call (plus any later DNC version bumps for
    Swift dynamic NCs). Marked **inferred** because it is metric-derived,
    not a single observed transition.
14. `cns-listener-ready` — cns `startedAt` → `[Listener] Started listening` log
    line.
15. `cns-conflist-write` — cns container start → newest `*.conflist` mtime on
    host (observed via the bundled DaemonSet that writes a Node annotation).
16. `cns-pod-ready` — T0 → Pod condition `Ready=True`.
17. `node-ready` **(OKR target)** — T0 → Node condition `Ready=True`.
18. `kubelet-cni-pickup (inferred)` — conflist mtime → Node `Ready=True`.

## Requirements

- `az` CLI logged in against the target subscription.
- `kubectl` context pointing at the target AKS cluster.
- Permission to scale the target nodepool.
- Cluster in Azure Overlay mode (NNC CRD installed, CNS DaemonSet running).

## Usage

```bash
# One-shot: add one node, observe, write outputs to ./out/
nodeinit-bench run \
  --cluster evanbaker-westus2 \
  --resource-group evanbaker-westus2 \
  --nodepool nodepool1 \
  --delta 1 \
  --out ./out

# Five repeated runs + aggregated percentiles:
nodeinit-bench run --cluster ... --runs 5 --cleanup

# Install the conflist-mtime DaemonSet (one-time per cluster):
kubectl apply -f deploy/conflist-mtime-daemonset.yaml
```

Outputs under `--out`:

- `spans.csv` — one row per span per node per run.
- `metrics.csv` — flattened CNS Prometheus series per pod (when scraped).
- `gantt.md` — Mermaid `gantt` block, one lane per span.
- `gantt.html` — self-contained Plotly timeline, one row per node.
- `dashboard.html` — **single-page interactive dashboard** with five linked
  views: cross-run stacked-bar comparison, per-phase distribution box plot,
  critical-path waterfall, per-run interactive Gantt with run selector, and
  CNS internal metrics histogram. Self-contained (Plotly.js loaded from CDN).
- `summary.md` — totals + per-span min/p50/p95/p99/max across runs.

### Re-rendering from existing run directories

```bash
# Combine multiple prior runs into one dashboard:
nodeinit-bench render --out ./out/combined ./out/run1 ./out/run2 ./out/run3
```

The `render` subcommand reads `spans.csv` (and `metrics.csv` when present)
from each given directory, renumbers run IDs sequentially to avoid
collisions, and re-emits the full artifact set (including `dashboard.html`)
into `--out`.

## Observability sources

| Source | Kind | What it gives us |
|---|---|---|
| `Node` object + conditions | informer | T0, Ready transition |
| `Node` events (`CreatedNNC`, `CreatingNC`, `UpdatedNC`, `RegisteredNode`) | informer | DNC-RC + NMAgent timings |
| `Pod` object + events (CNS DaemonSet) | informer | schedule, pulls, container start, Pod Ready |
| `NodeNetworkConfig` + `managedFields` | informer | NNC create / NC status-write times |
| CNS stdout logs | `kubectl logs` | CNS bootstrap, NNC ingest, listener ready |
| CNS `:10092/metrics` | port-forward | `syncHostNCVersionLatency` histogram |
| Conflist mtime DaemonSet | Node annotation | exact time conflist hit disk |

## Known observability gaps (and how this tool handles them)

- **CNS does not log "conflist written"** — bridged by the bundled DaemonSet
  that writes `nodeinit-bench/cni-conflist-mtime=<RFC3339Nano>` onto the Node
  object when it detects a new conflist. The DaemonSet watches `/etc/cni/net.d`
  and reports the newest `*.conflist` mtime, so it works for both the overlay
  scenario (`15-azure-swift-overlay.conflist`) and cilium-overlay
  (`05-cilium.conflist`). A stretch PR against CNS adds one
  `logger.Printf` in `MustGenerateCNIConflistOnce` to close this natively.
- **No direct kubelet "CNI plugin accepted" timestamp** — tool reports the
  `kubelet-cni-pickup` span as **inferred** (conflist-mtime → Node Ready)
  with a clearly flagged column in the CSV.
- **NMAgent NC programming is cross-referenced** from the `UpdatedNC` Node
  event (DNC-RC side) and the `cns-sync-host-nc-version` span (CNS side,
  derived from the `sync_host_nc_version_latency_seconds` histogram sum).
  For static NCs (overlay) this span is near-zero; for dynamic NCs (Swift)
  it captures the wait-for-NMAgent-program latency.

## Scope

v1 targets Linux + Azure Overlay. Windows and Swift v2 scenarios are reserved
behind the `--scenario` flag.
