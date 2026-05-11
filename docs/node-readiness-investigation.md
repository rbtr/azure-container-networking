# AKS Node Readiness Investigation

**Status:** baseline established; bottleneck identified; awaiting decision on mitigations.

## TL;DR

For an Azure CNI overlay (Cilium) AKS cluster on `Standard_B12ms` nodes,
node readiness `T0 (node creationTimestamp) → Node Ready=True` runs
**29-46 s** with **p50=31 s, p95=44 s, max=46 s** across 5 iterations.

Decomposing each second:

| Phase | p50 | p95 | Notes |
|-------|----:|----:|-------|
| `vm-provision` (`az` invocation → Node `creationTimestamp`) | 95 s | 106 s | ARM + VMSS + OS boot + CSE + kubelet register. Upstream of CNS / DNC-RC; **not part of `node-ready` (which anchors at T0 = creationTimestamp)**. |
| `dnc-rc-create-nnc` (T0 → CreatedNNC event) | 0 s | 0 s | DNC-RC reaction time, bounded below by Kubernetes' 1 s event resolution |
| `dnc-rc-create-nc` (CreatingNC → UpdatedNC events) | 1 s | 1 s | event-resolution floor; sub-second managedFields needed for real value |
| `cns-pod-schedule-latency` | 0.4 s | 1.1 s | kube-scheduler |
| `cns-init-image-pull` (azure-ipam Pulling → Pulled, matched by image name) | 2.5 s | 3 s | always pulled (~27 MB) |
| `cns-init-container-run` (init container Started → finishedAt) | ≤1 s | 1 s | one-shot CNI binary install; near-instant |
| **`cns-init-to-main-gap`** (init `finishedAt` → main `Pulled` event) | **11.5 s** | **12.65 s** | **kubelet pod-sync pipeline backpressure under stampede** — kubelet has to recognize init done, run admission for the main container, and call containerd to verify image. With 28 concurrent containers in flight on a fresh node, this is dominated by sync-loop contention. |
| `cns-image-pull` (azure-cns image; matched by image name) | 0 s | 0 s | image preloaded in AKS node image; rendered as zero-duration since kubelet emits only Pulled (no Pulling) for already-present images |
| `cns-container-start` (Pulled event → containerd startedAt) | 2 s | 4.5 s | grows with stampede |
| **`cns-exec-gap`** (containerd startedAt → first CNS log) | **12.3 s** | **19.5 s** | **dominant** — kernel/containerd not exec'ing the binary; the daemonset-stampede gap. Legacy 6 runs measured this rolled into `cns-process-bootstrap`; one validation run (#7) confirmed 17.3 s exec-gap + 1.1 s real bootstrap. |
| `cns-process-bootstrap` (first CNS log → "Reconciling initial CNS state") | ~1.1 s | ~1.1 s | real CNS-side work between first log and first NNC delivery: read config, init telemetry, build zap logger, init nmagent client, acquire process lock, controller-runtime cache sync, receive first NNC. Only directly measured on N=1 fresh run; long-running pod scrape showed ~340 ms — fresh-pod is slightly higher likely due to controller-runtime cache sync latency under stampede. |
| `cns-nnc-ingest` (RetrievedNNC → ReconcilingIPAM logs) | 0.001 s | 0.001 s | instant |
| `cns-sync-host-nc-version` (cumulative `sync_host_nc_version_latency_seconds_sum`) | 0.30 s | 0.43 s | initial v0 NMA confirmation; gates conflist write |
| `cns-listener-ready` (containerd startedAt → "Started listening") | 13.7 s | 20.8 s | = bootstrap + ~1-2 s of actual CNS work |
| `cns-conflist-write` (containerd startedAt → newest `*.conflist` mtime) | 14.5 s | 21.1 s | gated by listener-ready + filesystem flush |
| `kubelet-cni-pickup` (conflist mtime → Node Ready) | 0.5 s | 1 s | kubelet picks up CNI quickly once conflist exists |

CNS itself is fast: from first log line ("Using config") to "Started listening"
is ~2 s. The 12-20 s "process-bootstrap" gap is **before any CNS code runs**.

## How the Numbers Were Captured

The `tools/nodeinit-bench` Go tool: scales the nodepool by N, observes the
resulting Node objects, and emits per-Node spans by joining:

- Node object + conditions (T0, Ready transition)
- Node and NNC events (`CreatedNNC`, `CreatingNC`, `UpdatedNC`, `RegisteredNode`)
- Pod object + events (Pulling/Pulled/Started, PodReady)
- NNC `managedFields` (sub-second NNC create/status writes)
- CNS stdout logs via regex anchors (`Using config`, `Reconciling initial CNS state`,
  `Retrieved NNC`, `Reconciling CNS IPAM state`, `Started listening`)
- CNS `:10092/metrics` scrape (`sync_host_nc_version_latency_seconds`,
  `has_networkcontainer`, etc.)
- Conflist mtime daemonset → Node annotation

### Bug fixes landed during this investigation

1. **`tools/nodeinit-bench/internal/cnsmetrics/cnsmetrics.go`** — the explicit
   metric list referenced `cnsHostNCsyncLatency`, `cnsReconcilerStateLatency`,
   `cnsIPAMLatency` — none of which exist on CNS. Replaced with the real
   names (`sync_host_nc_version_latency_seconds`, `sync_host_nc_version_total`,
   `has_networkcontainer`, `http_request_latency_seconds`,
   `ip_assignment_latency_seconds`, `ipconfigstatus_state_transition_seconds`,
   `ip_pool_inc_latency_seconds`, `ip_pool_dec_latency_seconds`).

2. **`tools/nodeinit-bench/internal/spans/types.go` + `observer/build.go`** —
   added `cns-sync-host-nc-version` span derived from
   `sync_host_nc_version_latency_seconds_sum`. Anchored at
   `ReconcilingIPAM` log timestamp; duration is the metric sum. Marked
   inferred. Near-zero for static NCs (overlay), captures NMAgent wait
   for dynamic NCs (Swift).

3. **`tools/nodeinit-bench/deploy/conflist-mtime-daemonset.yaml`** —
   was hardcoded to watch `15-azure-swift-overlay.conflist`; this Cilium
   cluster writes `05-cilium.conflist`. Changed to walk
   `/etc/cni/net.d/*.conflist` and report the newest mtime.

4. **`tools/nodeinit-bench/internal/scaler/scaler.go` + `cmd/run.go`** —
   added `ProvisioningState` and `WaitForReady`; cleanup now blocks on
   ARM `provisioningState == Succeeded` before the next iteration's
   scale call so multi-`--runs` invocations don't race in-flight ARM ops.

## Caveats On The Numbers

1. **`dnc-rc-create-nc = 1 s` is a floor, not the real duration.** Kubernetes
   `Event` objects have second-level resolution; `CreatingNC` and `UpdatedNC`
   land in adjacent seconds, so the reported 1 s could be anywhere from
   <1 ms to ~2 s. NNC `managedFields` (sub-second) is the right source for
   precision here.

2. **`cns-sync-host-nc-version` IS in the critical path for overlay too.**
   When CNS first ingests an NC, `HostVersion` is initialized to `"-1"`
   (see `cns/restserver/util.go:171-175`: *"Host version is the NC version
   from NMAgent, set it -1 to indicate no result from NMAgent yet."*).
   The DNC-published version on the NNC is `"0"` for static NCs in overlay.
   On the first `syncHostNCVersion` tick, `localNCVersion(-1) < dncNCVersion(0)`,
   so the NC is "outdated" and CNS calls `nma.GetNCVersionList(ctx)` to
   confirm. Only after NMAgent reports v0 does CNS update `HostVersion` to
   `"0"`, mark the NC as `programmed`, and trigger
   `MustGenerateCNIConflistOnce` (see `cns/restserver/internalapi.go:177-183`,
   `:213-220`, `:295-296`). **Conflist generation — and therefore Node
   Ready — is gated on this NMAgent confirmation, even for static NCs.**

   The histogram confirms this: every one of the 5 baseline runs shows
   exactly **1 slow call (295-452 ms) + N-1 no-op calls (<1 ms)**. The
   single slow call is the initial v0 confirmation; subsequent ticker
   invocations short-circuit at `cns/restserver/internalapi.go:223`
   (`outdatedNCs == 0 → return`).

   For Swift (dynamic NCs whose version bumps on every IP allocation
   change from DNC), this same wait recurs on each bump, so the
   cumulative `sync_host_nc_version_latency_seconds_sum` would be larger
   over the node lifetime — but the initial v0 wait at boot is the same
   in both modes.

3. **`cns-conflist-write` and `kubelet-cni-pickup` are second-precision.** The
   conflist daemonset polls every second and uses `+%Y-%m-%dT%H:%M:%SZ`.
   `Node.LastTransitionTime` is also second-precision. Sub-second values
   here are not reliable.

4. **N=5 single-node-per-run.** Statistically thin; numbers should be read
   as orders of magnitude. The dominant bottleneck holds at every iteration.

## Bottleneck Detail: The "Exec Gap" (kernel hasn't run the CNS binary yet)

For each iteration, `cns-exec-gap` is `containerd startedAt → first CNS log
line`. CNS's first log (`[Configuration] Using config path: ...`) fires
inside `main()` before any IO — no sleep, no wireserver call, no DNS
lookup. So the gap is entirely **between containerd marking the cgroup as
Started and the kernel actually exec'ing the Go binary**.

Original measurement bundled this with the real CNS-side work in a single
`cns-process-bootstrap` span. The split (validated on a 7th fresh run)
shows the exec gap is ~17 s while the real CNS bootstrap (config read +
telemetry init + controller cache sync + first NNC delivered) is ~1 s.

What is happening on the node during the exec-gap window?

| Time | Event |
|------|-------|
| 20:04:51 | 8 daemonsets all start pulling images concurrently |
| 20:04:52 | First containers Started: ip-masq, cloud-node-manager, csi-azuredisk, csi-azurefile, defender-collector, nodeinit-bench-conflist |
| 20:04:54 | CNS init container azure-ipam pulled (3 s) |
| 20:04:55 | CNS init container Started, immediately Completed |
| 20:05:08 | CNS main container event: image already present (13 s after init done!) |
| 20:05:13 | CNS main container `startedAt` = 20:05:13 |
| **20:05:13 → 20:05:33** | **20-second gap; containerd/kernel busy** |
| 20:05:13 → 20:05:40 | Cilium pulls 6+ images sequentially |
| 20:05:33 | First CNS log line; CNS Go binary actually runs |
| 20:05:34 | CNS listening |
| 20:05:35 | Conflist on disk |
| 20:05:36 | Kubelet sees CNI, marks Node Ready |

The fresh node has **28 containers** trying to start, with **Cilium alone
contributing 10** (7 init + 3 main). Containerd's "Started" event fires when
the cgroup is created and the runtime starts the task, but the actual
process exec is serialized through the kernel under load. CNS — already
running with `priorityClassName: system-node-critical` — still gets
de-prioritized behind init containers from other daemonsets that schedule
before it.

## What Would Move Node Ready

In rough order of impact, with the caveats:

1. **Reduce concurrent containers on a fresh node.** Cilium has 7 init
   containers; folding work into the main container (or removing optional
   init steps) is the largest single lever. This is a Cilium-side change,
   not a CNS change.

2. **Pre-pull `azure-ipam`** (the CNS init container, ~27 MB) into the AKS
   node image. Saves a deterministic 2-3 s. Already done for
   `azure-cns:v1.8.1-0` (image was "already present"); the init image is
   the only one still pulled per node.

3. **Pre-resolve the IMDS / wireserver calls** that CNS makes in the first
   second of `main()` (`GetAzureCloud` + `GetHostMetadata`). They have a
   7 s connection timeout each. On a freshly-booted node they sometimes hit
   that timeout. Worth instrumenting.

4. **Consider giving CNS its own kubelet pod admission gate** (Static Pod /
   `nodeRegistration.taints`) so it starts before non-network daemonsets.
   CNS is in fact the gating dependency for `Node.Ready=True` (via
   conflist), so the current "everyone starts at once" model wastes
   serialized exec time on pods that can't make progress until CNS is up
   anyway. This is an AKS / control-plane decision.

5. **Nothing in CNS itself is on the critical path.** From the first log
   line to "Started listening" is ~1.5-2 s; further code-level optimization
   will not move the needle measurably.

## Out Of Scope / Open Questions

- **Swift mode.** All numbers here are overlay/static. Swift would expose
  the real `syncHostNCVersion` cost; need a Swift cluster to capture it.
- **Other AKS distributions.** This is Azure Linux 3 / kernel 6.6. Worth
  re-running on Ubuntu 24.04 to see if scheduler/cgroup changes affect
  the process-start gap.
- **Hot-cache vs cold-cache image scenarios.** Run 4 (max=46 s) was
  preceded by 3 prior creates on the same VMSS; some image cache warming
  is implicit. A truly cold pool may be worse. A pre-baked node image
  might cut tail latency.
- **The 17-20 s gap on the very first iteration.** May be VMSS first-boot
  vs subsequent-boot effects (osconfig, walinuxagent, CSE). Need a
  deterministic run-from-cold-VMSS protocol to measure.
