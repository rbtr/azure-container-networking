# Node Readiness Workstream

**Status:** Active investigation; bottleneck identified; mitigations
proposed but most not yet validated end-to-end. Open PR for
observability prerequisite (#4398). Static-pod-at-boot experiment
empirically blocked on AKS-managed VMSSes via VM extensions.

## Question

> How fast can a new AKS node go from `Node creationTimestamp` to
> `Node Ready=True`?

This is the **node-readiness** workstream. A *separate*
[pod-slo](https://github.com/rbtr/azure-container-networking/tree/experiment/pod-slo)
workstream tracks the **pod startup latency** question (how fast
can pods get IPs and reach Ready). The two are related — pod
startup contributes to overall cluster scale-up — but the bottleneck
analysis, mitigation surface, and experiment design are different
enough that they're tracked separately.

## TL;DR

For an Azure CNI overlay (Cilium) AKS cluster on `Standard_B12ms` nodes:

| Phase | p50 | What's actually happening | Real work? |
|-------|----:|---------------------------|:----------:|
| `cns-pod-schedule-latency` | 0.8 s | kube-scheduler | yes |
| `cns-init-image-pull` | 3 s | pull `azure-ipam` (~27 MB) | yes |
| `cns-init-container-run` | 1 s | one-shot CNI binary install | yes |
| **`cns-init-to-main-gap`** | **12 s** | kubelet pod-sync waiting for containerd | **no** |
| `cns-image-pull` | 0 s | `azure-cns` already preloaded | n/a |
| `cns-container-start` | 2 s | containerd `Created` + `Started` events | mostly no |
| **`cns-exec-gap`** | **14 s** | kernel hasn't exec'd the Go binary (stampede) | **no** |
| `cns-process-bootstrap` | 1 s | parse config, init telemetry, cache sync | yes |
| `cns-nnc-ingest` | <0.01 s | apply NNC into local state | yes |
| `cns-sync-host-nc-version` | 0.3 s | NMAgent confirms NC v0 programmed | yes |
| `cns-conflist-write` | ~6 s after container start | atomic write + fsync | yes (~ms) |
| `kubelet-cni-pickup` | <1 s | kubelet sees conflist → marks Ready | yes |

**~26 s of the median 32 s is pure scheduler/runtime wait with no
useful work happening.** Actual work: ~5–6 s. The gap is preventable.

Full breakdown:
[`docs/node-readiness-investigation.md`](docs/node-readiness-investigation.md).

## What's in this branch

### `tools/nodeinit-bench/` — measurement tool

CLI that scales an AKS nodepool, observes each new Node, and emits
a per-Node Gantt-shaped span dataset to CSV / Mermaid / Plotly /
Markdown. Joins evidence from Node + Pod objects, K8s events, NNC
`managedFields`, CNS stdout logs (regex anchors), CNS
`:10092/metrics` scrape, and a conflist-mtime DaemonSet annotation.

Build:
```bash
make nodeinit-bench
```

Run (against your default kubeconfig context):
```bash
./bin/nodeinit-bench run --runs 5 --node-pool nodepool1
./bin/nodeinit-bench render --out tools/nodeinit-bench/out/dashboard
```

### `tools/nodeinit-bench/deploy/conflist-mtime-daemonset.yaml`

Hostpath DaemonSet that watches `/etc/cni/net.d/` and writes the
newest `*.conflist` mtime as a Node annotation
`azure-container-networking.io/conflist-mtime`. nodeinit-bench
prefers the CNS log anchor when present and falls back to this.

### `docs/node-readiness-investigation.md`

Empirical baseline: phase-by-phase decomposition of `node-ready`
across 5–8 iterations on `evanbaker-eastus2`. Documents the tool,
the dataset, the bug fixes landed during the investigation, and the
known unknowns.

### `docs/node-readiness-improvements.md`

Catalog of mitigations ranked by impact × feasibility:

- **Tier 1 (cheap):** logging cleanup, conflist write deferral
  trade-off analysis, image-preload audit
- **Tier 2 (architectural):** T2.1 — CNS as a static pod; T2.2 —
  CNS as a systemd unit. **T2.2 is the cleaner architectural
  target** given the empirical findings in
  `static-pod-test-findings.md`.
- **Tier 3 (foundational):** custom AKS VHD with CNS pre-baked,
  kernel/kubelet/containerd tuning, etc.

### `docs/static-pod-test-findings.md`

Investigation of the T2.1 (static-pod) approach. Empirically
**blocked**: extension-based injection of a static-pod manifest at
boot is not viable on AKS-managed VMSSes. Tested four extension
types across three publishers:

| Extension | Result |
|---|---|
| `Microsoft.Azure.Extensions.CustomScript` v2.0 | Replaces vmssCSE (publisher.type collision) → kubelet never installed |
| `Microsoft.CPlat.Core.RunCommandLinux` v1.0 | Coexists in VMSS model but new instance stuck `Creating → Failed` |
| `Microsoft.CPlat.Core.RunCommandHandlerLinux` v1.3 | Per-instance only; not a VMSS-template extension |
| `Microsoft.OSTCExtensions.CustomScriptForLinux` v1.5 | Different publisher; vmssCSE intact in model. New instance still went `Creating → Deleting`. vmAgent never reported. AKS reaped after ~13 min |

The remaining paths for validating the static-pod-at-boot hypothesis
are (a) custom AKS VHD with the manifest pre-baked, or (b)
self-managed VMSS — both substantially more setup than the
extension path.

### `test-staticpod/` — static-pod test artifacts

Manifests + scripts for the A/B static-pod experiment. The A
variants (DaemonSet CNS with image pre-baked vs forced pull) were
completed (10 runs each, N=20 total). The B variants (static pod
with image pre-baked vs forced pull) **could not be run** for the
reasons documented in `static-pod-test-findings.md`. Manifests
preserved for future revival if/when the VHD-bake-in path becomes
available.

### `cns/restserver/restserver.go` — CNS conflist log anchor

Adds two `logger.Printf` lines around `MustGenerateCNIConflistOnce`
so external observers can pick up sub-second timing from CNS logs
directly. nodeinit-bench prefers this anchor over the DaemonSet
mtime annotation. Complementary to PR #4398 (which adds
`cns_conflist_written_seconds` Prometheus metric for the same
boundary).

### Shared dev tooling (also on `experiment/pod-slo`)

- `.github/lsp.json` — gopls Language Server config for Copilot CLI
- `.github/scripts/setup-go-tooling.sh` — one-time gopls installer
- `.mcp.json` — gopls MCP server config (gopls v0.20+)
- `.github/gopls-mcp-instructions.md` — model instructions
- `agents.md` — expanded with Go code-intelligence guidance

## Related work (NOT in this branch)

- **PR #4398** ([`feat/cns-bootstrap-metrics`](https://github.com/Azure/azure-container-networking/pull/4398))
  — adds 16 Prometheus metrics for bootstrap-phase observability
  (`cns_build_info`, `cns_start_time_seconds`, `cns_mode_info`,
  `cns_boot_state{state}`, 7 event-timestamp gauges,
  `cns_time_to_event_seconds` histogram, NNC reconcile health +
  staleness gauges). Designed as the durable replacement for the
  log-anchor approach used by nodeinit-bench today.
- **[`experiment/pod-slo`](https://github.com/rbtr/azure-container-networking/tree/experiment/pod-slo)**
  branch — the sibling workstream investigating pod startup latency.
- `rbtr/evanbaker/node-readiness-investigation` — the WIP branch
  this one was extracted from. **Can be deleted** once this branch
  is established.

## Next steps (resume guide)

1. **Wait for PR #4398 to merge** and rebase this branch. The
   metric-based phase boundaries supersede the log-anchor approach
   in nodeinit-bench; once merged, update
   `tools/nodeinit-bench/internal/observer/build.go` to consume
   `cns_*_seconds` gauges as the primary source.
2. **T2.2 (CNS as systemd unit)** — the recommended architectural
   target. Avoids both blockers we hit on T2.1: no mirror-pod SA
   issue (CNS isn't a pod), no VMSS-extension issue (the unit ships
   in the node image, not as an extension). Larger expected savings
   than T2.1 (CNS bootstraps before kubelet, not alongside it).
   Requires custom AKS VHD; same prerequisite as T2.1 anyway.
3. **Tier 1 quick wins** in
   `docs/node-readiness-improvements.md` — image preload audit,
   conflist deferral analysis. Lower impact, lower risk.
4. **Reproduce the baseline** on current cluster geometry to
   confirm the numbers haven't drifted. `nodeinit-bench` produces
   the canonical artifact (`tools/nodeinit-bench/out/dashboard/`).

## Open / unanswered questions

- Why does `cns-init-to-main-gap` take 12 s? Suspected kubelet
  pod-sync pipeline backpressure under stampede; not directly
  profiled.
- Is the `cns-exec-gap` (14 s) actually kernel exec time, or is it
  containerd waiting for cgroup setup / OCI bundle creation /
  initial process exec? Strace + containerd debug logs would
  disambiguate.
- Would a custom-VHD bake-in of CNS as a systemd unit (T2.2)
  produce a < 10 s `node-ready`? Hypothesis says yes; not yet
  validated.

## Test clusters used during investigation (all torn down)

- `evanbaker-eastus2` — main baseline
- `evanbaker-staticpod-westus2`, `-staticpod2-westus2`,
  `-csedebug-westus2`, `-cseretry-westus2` — extension-injection
  experiments

## Conventions

- All time is Unix-seconds (sub-second precision where measured).
- Spans are anchored at `Node.metadata.creationTimestamp = T0`.
- The Gantt is built per-Node, not aggregated; aggregation lives in
  the dashboard.
