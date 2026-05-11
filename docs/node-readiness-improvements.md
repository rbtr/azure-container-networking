# AKS Node Readiness — Improvement Proposals

**Status:** brainstorm; numbers from `tools/nodeinit-bench/out/dashboard` (8 baseline runs, evanbaker-eastus2 Cilium overlay, B12ms).

## Where the time goes today

Median 32 s `node-ready`, decomposed by `nodeinit-bench`:

| Phase | p50 | What's actually happening | Real work? |
|-------|----:|---------------------------|:----------:|
| `cns-pod-schedule-latency` | 0.8 s | kube-scheduler | yes |
| `cns-init-image-pull` | 3 s | pull `azure-ipam` (~27 MB) | yes |
| `cns-init-container-run` | 1 s | one-shot CNI binary install to `/opt/cni/bin` | yes |
| **`cns-init-to-main-gap`** | **12 s** | kubelet pod-sync pipeline waiting for containerd | **no** |
| `cns-image-pull` | 0 s | `azure-cns` already preloaded | n/a |
| `cns-container-start` | 2 s | containerd `Created` + `Started` events | mostly no |
| **`cns-exec-gap`** | **14 s** | kernel hasn't exec'd the Go binary yet (stampede) | **no** |
| `cns-process-bootstrap` | 1 s | parse config, init telemetry, controller cache sync, first NNC | yes |
| `cns-nnc-ingest` | <0.01 s | apply NNC into local state | yes |
| `cns-sync-host-nc-version` | 0.3 s | NMAgent confirms NC v0 programmed | yes |
| `cns-conflist-write` | ~6 s after container-start | atomic write + fsync | yes (~ms) |
| `kubelet-cni-pickup` | <1 s | kubelet sees conflist → marks Ready | yes |

**~26 s of the 32 s is pure scheduler/runtime wait with nothing happening.** The actual work is ~5-6 s. So there is a hard floor somewhere south of 10 s if every gap collapses; everything between that and 32 s is preventable.

The remainder of this doc enumerates ways to remove those gaps, ranked by impact × feasibility.

---

## Tier 1 — Cheap wins, no rearchitecture

### T1.1 Pre-bake `azure-ipam` into the AKS node image
**Saves:** ~3 s deterministically (and unblocks T1.2).
**How:** add the binary to the AKS node image build (same place `azure-cns` already lives). Init container's image becomes "already present" → 0 s pull.
**Cost:** node image bump. Per-version coupling between AKS image and CNI release.

### T1.2 Eliminate the init container — bake CNI binaries into node image
**Saves:** ~3 s (init pull) + ~1 s (init run) + most of the **12 s init-to-main-gap** = **~15 s**.
**How:** ship `/opt/cni/bin/azure-vnet`, `/opt/cni/bin/azure-ipam`, and the CNI conflist template inside the node image (or written by CSE). Drop the init container entirely from the CNS DaemonSet manifest.
**Cost:** AKS image owns the CNI binary version, not the container image. Need a story for CNI upgrade-without-node-reimage (e.g. a privileged daemonset that swaps the binary at runtime, separate from CNS startup path).

### T1.3 Reduce concurrent containers on the new Node
**Saves:** unknown; should reduce `cns-exec-gap` and `cns-init-to-main-gap` proportionally to how much containerd queue depth drops. Cilium contributes 7 init + 3 main = 10 of the 28 containers; folding any subset wins.
**How:** Cilium-side change to merge init containers; AKS-side audit of optional daemonsets (Defender, csi-azurefile, ip-masq) for whether they can defer.
**Cost:** distributed across multiple owning teams.

### T1.4 Use a `tolerations`-gated initial Node taint that only CNS tolerates
**Saves:** removes the stampede; expected to compress `cns-exec-gap` and `cns-init-to-main-gap` by an order of magnitude (kubelet/containerd work CNS-first instead of in parallel with 27 others).
**How:**
1. AKS-provisioned Node arrives with a startup taint, e.g. `node.cloudprovider.kubernetes.io/uninitialized=cns:NoExecute`.
2. CNS DaemonSet has a matching toleration; nothing else does.
3. CNS, on starting up successfully, calls the apiserver to remove the taint from its own Node.
**Cost:** AKS control-plane change to set the taint at registration; lifecycle bug if CNS crashes during startup (the taint blocks all other workloads). Solvable with a kubelet-driven "remove if CNS healthz responds" loop or a node-problem-detector hook.

### T1.5 Replace per-pod `wait-for-NMA-v0` with NC pre-programming
**Saves:** ~0.3 s typical, but tail-bounded — a slow NMAgent today blocks every fresh CNS startup.
**How:** DNC-RC requests the NC from the fabric **before** the Node registers (using the VMSS instance's pre-allocated MAC/NIC info). NMAgent has the NC programmed by the time the Node first appears, so CNS's first `syncHostNCVersion` finds `localNCVersion(-1) < dncNCVersion(0)`, calls NMA, and gets v0 immediately with zero queue time.
**Cost:** DNC-RC reorder; backwards-compatible with current CNS code.

---

## Tier 2 — Architectural changes

### T2.1 CNS as a static pod, not a DaemonSet

> **⚠️ Validation status (2026-05-07, updated):** partial test on a
> fresh BYOCNI cluster surfaced two confirmed blockers:
>
> **Blocker 1 (confirmed):** Mirror pods cannot reference a
> ServiceAccount. A static-pod CNS requires a hostPath kubeconfig with
> rotated token (same pattern kubelet itself uses).
>
> **Blocker 2 (confirmed empirically across four extension types):**
> Extension-based injection of a static-pod manifest at boot is **not
> viable on AKS-managed VMSSes**. We tested:
>
> | Extension | Result |
> |---|---|
> | `Microsoft.Azure.Extensions.CustomScript` v2.0 | Replaces vmssCSE (publisher.type collision) → kubelet never installed |
> | `Microsoft.CPlat.Core.RunCommandLinux` v1.0 | Coexists with vmssCSE in the model, but new instances stuck `Creating` then Failed |
> | `Microsoft.CPlat.Core.RunCommandHandlerLinux` v1.3 | Per-instance only; not a VMSS-template extension — won't run on newly-scaled instances |
> | `Microsoft.OSTCExtensions.CustomScriptForLinux` v1.5 | Different publisher; vmssCSE intact in model. New instance still bootstrapped to PowerState=running but VM Agent never reported; AKS reaped after ~13 min |
>
> Even with no publisher.type collision, AKS-managed VMSSes do not
> provision new instances cleanly when user extensions are present in
> the VMSS model. The only remaining "manifest at boot" path is a
> custom AKS VHD with the manifest pre-baked into
> `/etc/kubernetes/manifests/`. A DaemonSet writer is k8s-native but
> does NOT validate the "manifest exists before kubelet starts"
> hypothesis.
>
> See [`static-pod-test-findings.md`](./static-pod-test-findings.md)
> for the full repro details.
>
> **The T2.2 (systemd unit) path avoids every blocker we hit** — no
> mirror-pod-SA issue (CNS isn't a pod), no VMSS-extension issue (the
> unit ships in the node image), and CNS bootstraps before kubelet
> rather than alongside it. Given that the natural production path
> for either T2.1 or T2.2 is custom-VHD bake-in, T2.2's larger
> expected savings make it the better target for architectural
> commitment.

**Saves:** removes scheduling latency, removes apiserver dependency for pod object, makes CNS one of the very first containers kubelet starts (static pods are local and don't share the daemonset queue). Still subject to containerd contention but earlier in the queue. Estimate: 5-10 s off `cns-exec-gap` + `cns-init-to-main-gap`.
**How:** drop a manifest into `/etc/kubernetes/manifests/azure-cns.yaml` from CSE. Kubelet creates the mirror pod on its own.
**Cost:** AKS image / CSE owns the CNS manifest; no kubectl-rollout for CNS upgrades on existing nodes (need image rebuild or in-place file swap). Can be combined with a DaemonSet that handles upgrades.

#### T2.1.a Upgrade path: static pod managed by a thin updater DaemonSet

Yes — a DaemonSet can absolutely update a static pod, and this is exactly
how kubeadm manages control-plane component upgrades (kube-apiserver, etcd,
controller-manager). The mechanism is "DaemonSet writes a new manifest
file; kubelet's manifest watcher picks up the change and restarts the
mirror pod."

**Architecture:**

```
                  ┌─────────────────────────┐
                  │ azure-cns-updater DS     │ (k8s-managed; updatable
                  │  - hostPath mounts:      │  via standard kubectl rollout)
                  │      /etc/kubernetes     │
                  │      /opt/cni/bin        │
                  │  - reads desired version │
                  │    from ConfigMap or     │
                  │    own image tag         │
                  │  - pre-pulls cns image   │
                  │    via crictl            │
                  │  - atomically replaces   │
                  │    the manifest file     │
                  └────────────┬─────────────┘
                               │
                               ▼ (writes to host)
              /etc/kubernetes/manifests/azure-cns.yaml
                               │
                               ▼ (kubelet watches)
                          kubelet restarts the static pod
                               │
                               ▼
                       /var/run/azure-cns
                       /etc/cni/net.d/<conflist>
```

**Concrete sequence for an upgrade:**

1. Cluster operator updates the desired CNS version (e.g., bumps a
   ConfigMap `azure-cns-config.version=v1.8.2-0` or rolls the updater
   DaemonSet image, depending on which model is chosen).
2. On each node, the updater pod's reconcile loop notices the desired
   version differs from `/etc/kubernetes/manifests/azure-cns.yaml`'s
   image reference.
3. Updater calls `crictl pull mcr.microsoft.com/.../azure-cns:v1.8.2-0`
   to ensure the image is on disk before kubelet tries to start the
   new pod (avoids a stop → fail-to-pull → outage gap).
4. Updater writes the new manifest atomically:
   `write /etc/kubernetes/manifests/.azure-cns.yaml.tmp` →
   `rename to /etc/kubernetes/manifests/azure-cns.yaml`. Atomic
   rename is critical — kubelet must never see a half-written file.
5. Kubelet's manifest watcher (`fsnotify`) fires within ~1 s, parses
   the new pod spec, kills the old container, starts the new one.
6. Updater polls CNS `:10092/healthz` until it returns 200, then
   considers the upgrade successful.
7. If healthz never returns success within a configurable timeout,
   the updater restores the previous manifest from a sibling
   `azure-cns.yaml.bak` it kept around — automatic rollback.

**Where the desired version comes from (3 choices):**

| Source | Pros | Cons |
|--------|------|------|
| **ConfigMap** (`azure-cns-config: image: ...`) | trivial to update via kubectl; one source of truth across nodes | another moving piece; updater must watch CM |
| **Updater image tag** (manifest template baked into updater) | one image rollout = one CNS rollout; DaemonSet semantics handle staged rollout, maxSurge, etc. | every CNS bump = updater rebuild |
| **CRD** (`AzureCNSConfig` cluster-scoped) | room for per-nodepool config, scenario-specific images | most code; least standard |

Recommend the **updater-image-tag** model for parity with how kubeadm /
operator-style components are typically delivered: the updater is
versioned with CNS, so a `kubectl set image ds/azure-cns-updater
updater=...:v1.8.2-0` is the entire upgrade UX. RollingUpdate strategy
gives you the standard staged rollout for free.

**Key correctness concerns:**

1. **Atomicity** — always write `.tmp` + `rename`; kubelet's manifest
   watcher will otherwise occasionally see a syntactically invalid
   YAML and skip the update.
2. **Image pre-pull** — `crictl pull` (or equivalent containerd API
   call) before manifest swap. Otherwise: old pod gets killed,
   kubelet tries to start the new one, image pull takes 10 s, and
   CNS is down for 10+ s. With pre-pull, the swap window is just
   "kill + start" = 1-3 s.
3. **Brief CNS outage during the swap** — static pod has no
   rolling-update semantics; it's strictly stop → start. CNI calls
   to CNS during the ~2 s gap will fail; `azure-vnet` already retries
   on connection-refused so this is tolerable for new pods but
   worth flagging. Pod deletes during the gap are also delayed.
4. **State preservation** — CNS persistent state lives in
   `/var/lib/azure-network/azure-cns.json` and
   `/var/run/azure-cns/azure-endpoints.json`, both hostPath. Surviving
   the restart is automatic since both new and old pods mount the
   same host paths.
5. **Conflist regeneration** — `MustGenerateCNIConflistOnce` is
   called once per CNS process lifetime. After an upgrade the new
   pod generates a fresh conflist; the file is overwritten atomically.
6. **Rollback** — keep `azure-cns.yaml.bak`; if the new pod fails
   `healthz` for N polls, restore. This is similar to what kubeadm
   does for control-plane upgrades.
7. **Updater self-upgrade** — the updater DaemonSet upgrades itself
   via standard k8s rollout (it's not a static pod). One layer of
   indirection cleanly separates "what k8s manages" (updater) from
   "what's local on the node" (CNS itself).

**Comparison to alternatives:**

| Strategy | Upgrade UX | Restart impact | Bootstrap impact |
|----------|------------|----------------|------------------|
| Pure DaemonSet (today) | `kubectl set image` → DaemonSet rolls | brief outage during pod replacement | scheduling/admission/exec-gap on every fresh node |
| Static pod + updater DaemonSet | `kubectl set image` on updater | identical brief outage | **eliminated on fresh node — that's the win** |
| systemd unit + updater DaemonSet (T2.2) | `kubectl set image` on updater; updater swaps `/usr/local/bin/azure-cns` and `systemctl restart` | identical brief outage | even better (no kubelet/containerd at all) |
| In-place AKS node image bump | full node reimage | full pod drain | no benefit beyond the bump |

The static-pod-plus-updater pattern preserves the standard kubectl
upgrade UX while moving the *cold-start* path entirely off the
DaemonSet stampede. That is precisely what we want: pay the
DaemonSet/scheduling cost only on intentional upgrade events, not on
every fresh node.

**Bootstrap on a brand-new node:**
1. CSE drops `/etc/kubernetes/manifests/azure-cns.yaml` (initial version
   from the AKS node image).
2. Kubelet starts → notices manifest → starts CNS as static pod.
3. CNS writes conflist; node Ready.
4. Later, when the apiserver is reachable, the `azure-cns-updater`
   DaemonSet schedules onto the node (not on the cold-start path),
   reconciles version, possibly upgrades CNS to the cluster-current
   version.

This means a fresh node skips the entire DaemonSet stampede for CNS,
and only converges to the "correct" cluster-current version after it
is already serving pod IPAM. That tradeoff is fine: a one-version-stale
CNS at boot is much better than a 30-s-late ready CNS.

### T2.2 CNS as a systemd unit
**Saves:** removes container exec layer entirely. CNS starts during `network-online.target` along with kubelet. By the time kubelet reports `Ready`, CNS has already written the conflist. **Eliminates `cns-exec-gap`, `cns-init-to-main-gap`, `cns-image-pull`, `cns-container-start`** — collectively ~30 s today.
**Estimate:** node-ready in 5-10 s (pure VM boot + CSE + kubelet first-sync).
**How:**
- Build CNS as a static binary (already mostly is).
- Ship it on the node image at `/usr/local/bin/azure-cns`.
- systemd unit at `/etc/systemd/system/azure-cns.service` with `After=network-online.target` `Before=kubelet.service`.
- Persistent state stays in `/var/lib/azure-network/azure-cns.json`; same code path.
- Conflist generation happens before kubelet starts watching the CNI directory.
**Cost:**
- Lifecycle: how do we upgrade CNS? Options:
  - In-place binary swap by a privileged daemonset that drops a new binary + restarts the unit.
  - Or: keep a thin DaemonSet for upgrades that quiesces the systemd unit and replaces the binary.
- Loses k8s-native log/healthz collection — need to wire CNS stdout to journald and expose `:10092/metrics` from the systemd-managed listener.
- Deployment story changes: AKS image owns CNS version (until first upgrade), then daemonset owns it.

### T2.3 CNS HTTP API → local socket / shared-memory
**Saves:** eliminates ~5 ms TCP roundtrip per `RequestIPConfigs`. Not a node-readiness win but a per-pod IPAM win.
**How:** unix domain socket in `/run/azure-cns.sock`. Already feasible with current code.

### T2.4 CNS as a CNI plugin process (no separate service)
**Saves:** if CNS is *only* needed during pod creation, it could be invoked synchronously by `azure-vnet` instead of running as a daemon. Eliminates the entire CNS lifecycle from node-readiness path.
**How:** redesign so the IP allocation logic lives inside `azure-ipam`/`azure-vnet`; the goal-state controller (NNC reconciler, NMA polling) becomes a small standalone "azure-cns-ctrl" daemon that updates a local state file. CNI binary reads from local state, doesn't need RPC.
**Cost:** large refactor. Loses observability (no central HTTP endpoint to scrape). May not satisfy SwiftV2 multi-tenancy needs that depend on a central process.

---

## Tier 3 — Radical / longer-term

### T3.1 Per-node IP pool baked in at provisioning time
**Concept:** DNC-RC writes the NC config for the new VM into a known location (e.g., a tag on the VM, a custom data field, or an extension) **before** the VM boots. CSE on first boot drops the NC config into `/var/run/azure-cns/nc-config.json`. CNS starts with the NC already populated; no need to watch NNC or call NMA for v0.
**Saves:** removes `cns-nnc-ingest`, `cns-sync-host-nc-version`, and most of `cns-process-bootstrap` (no controller cache sync needed). Combined with T2.2: node-ready could be **<5 s**, dominated by VM boot + kubelet first-sync.
**Cost:** DNC-RC needs to allocate NCs before VMs are scheduled (already kind of does this in autoscaler-driven scenarios). Loses dynamic NNC update semantics — NCs become immutable for the VM lifetime, which simplifies but constrains pool resizing.

### T3.2 CNS embedded in kubelet via DRA / CNI v2
**Concept:** when CNI v2 (with sandbox-and-network-plugin separation) lands, IPAM could be a kubelet plugin negotiated at startup, not a separate process.
**Cost:** upstream CNI evolution; multi-year horizon.

### T3.3 Eliminate CNS entirely; native VPC routing on the host
**Concept:** Azure CNI with no IPAM service. Each VM gets a NIC range up front; pod IPs are allocated locally from that range with no central coordinator. Like AWS VPC CNI's static-NIC mode.
**Cost:** loses dynamic resizing, loses pool-coordination across pods, loses Swift multi-tenancy. Not a viable replacement for the full feature set, but worth considering for a "low-overhead overlay" tier.

---

## Recommended sequence

If the goal is "make node-ready in 10 s instead of 32 s":

1. **T1.1 + T1.2** (bake CNI binaries, drop init container) — buys ~15 s, low risk, no API changes. **Best $/sec.**
2. **T2.1** (CNS as static pod) or **T1.4** (taint-gated startup) — buys another 5-10 s by removing stampede contention.
3. **T1.5** (NC pre-programming) — bounds the tail.

If the goal is "make node-ready in <5 s":

1. **T2.2 + T3.1** (CNS as systemd unit + NC config baked in via CSE) — radical, but the math works and there is no fundamental k8s-API dependency on the critical path.

### Static pod vs systemd on Linux — should they be the same answer as Windows?

After working through the Windows side, this is worth challenging: **if
we're recommending Windows Service (T2.2) on Windows for dependency
ordering and SCM-native lifecycle reasons, should Linux also go directly
to systemd (T2.2) instead of stopping at static pod (T2.1)?**

Probably yes. The original framing of T2.1 as the "lower-risk halfway"
and T2.2 as the "more aggressive next step" undersells how much T2.1
leaves on the table on Linux:

| Aspect | T2.1 static pod | T2.2 systemd unit |
|--------|-----------------|-------------------|
| Linux perf win | ~5-10 s (CNS still goes through containerd, just earlier in the queue) | **~30 s (eliminates `cns-exec-gap`, `cns-image-pull`, `cns-container-start`, `cns-init-to-main-gap` entirely; CNS is up before kubelet starts watching)** |
| Dependency ordering | implicit ("kubelet starts manifests early") | explicit (`After=network-online.target Before=kubelet.service`) — same paradigm Windows gets via SCM `DependsOnService` |
| kubectl logs / describe / exec | ✅ kubelet creates a mirror pod | ❌ need `journalctl -u azure-cns` on the node, or a logs aggregator |
| Pod readinessProbe → NodeCondition | ✅ free | ❌ need a separate health endpoint scraper that posts a NodeCondition |
| Updater DaemonSet pattern (T2.1.a) | swap manifest file → kubelet restarts mirror pod | swap binary + `systemctl restart azure-cns` — same DaemonSet, different action |
| Auto-restart on crash | ✅ (kubelet restarts) | ✅ (`Restart=always`) |
| Resource limits | ✅ via pod spec | ✅ via systemd `MemoryHigh`/`CPUQuota` |
| Conventional on platform | yes | **also yes** — kubelet, containerd, walinuxagent are all systemd units on AKS Linux nodes |

The thing that previously kept me framing T2.1 as the "Linux answer" was
the kubectl-observability tax. But:

1. **That tax is platform-asymmetric**: on Windows we're already paying
   it (Windows Service has no pod, no kubectl logs/describe). If we're
   accepting it on Windows for consistency reasons, accepting it on
   Linux for **3× more performance** is the easier sell.

2. **The replacements are straightforward**:
   - **Logs**: journald already collects CNS stdout; ship to AKS log
     analytics via existing fluent-bit / OMS agent that's already on
     every node (it scrapes journald entries today).
   - **Health visibility**: a tiny CNS-health DaemonSet (or an addition
     to the same updater DaemonSet from T2.1.a) can `curl
     localhost:10092/healthz` and post a `NodeCondition` like
     `AzureCNSReady=True/False`. That gives you kubectl-visible health
     better than today's pod readinessProbe (which only tells you the
     pod is alive, not that conflist is on disk and IPAM is serving).
   - **Debugging**: `kubectl debug node/<name>` is the standard
     escape hatch for node-local processes; the same node access used
     today for kubelet/containerd debugging works for CNS.

3. **The performance cliff is real**. T2.1 leaves ~20 s on the table
   that T2.2 captures. If we're doing the architectural work anyway,
   stopping at static pod means doing a second migration later when
   someone asks why Linux node-ready is still 15 s.

4. **Symmetry across OSes is a maintenance win**. Today the CNS
   deployment story is "DaemonSet on both Linux and Windows, with
   different YAMLs." Tomorrow it could be "system service on both
   Linux and Windows, with different unit/SCM definitions." The
   latter is more honest about what CNS actually is — a node-level
   infrastructure service, not a workload pod.

### Updated recommendation

The honest version:

| Goal | Path | Trade |
|------|------|-------|
| Quick wins, no architectural change | T1.1 + T1.2 + T1.5 | ~15-20 s saved; still a DaemonSet |
| Cap stampede contention without rearchitecting CNS lifecycle | + T1.4 (taint-gated startup) | another ~5-10 s; AKS control-plane change required |
| Move CNS off the cold-start critical path entirely | **T2.2 (systemd on Linux, Windows Service on Windows) + T2.1.a updater DaemonSet** | ~30 s saved on Linux; consistent OS-agnostic story; loses kubectl-pod observability for CNS (replaceable with journald + NodeCondition + node debug) |
| Best achievable | + T3.1 (NC config baked in via CSE / VMSS extension) | node-ready in <5 s; requires DNC-RC reorder |

**The cleanest answer is "system service on both OSes."** Static pod is
defensible as a smaller-risk stepping stone, but it permanently leaves
~20 s on the table and creates an asymmetric story across Linux and
Windows. If the team is willing to absorb the operator UX migration on
Windows (which it should be — it's idiomatic there), absorbing the same
on Linux for 3× the performance gain is the better trade.

---

## Windows nodes — what changes

Windows AKS nodes are a meaningfully different platform; not everything in
the tiers above maps 1:1. The current state and the per-tier translation:

### Current Windows CNS deployment

- DaemonSet `azure-cns-win` (`cns/azure-cns-windows.yaml`) — same image as
  Linux CNS but launched via `azure-cns.exe`.
- **HostProcess container** (`securityContext.windowsOptions.hostProcess: true`,
  `runAsUserName: NT AUTHORITY\SYSTEM`). This is critical: HostProcess
  containers do **not** carry a Windows OS layer in the image and have no
  isolation overhead — they execute directly on the host like a native
  service. That alone makes the Windows "exec gap" much smaller than
  Linux's, where every container starts a fresh runc-managed cgroup.
- `hostNetwork: true`, `priorityClassName: system-node-critical` (same as
  Linux).
- State paths differ: `/k/azurecns/azure-endpoints.json` instead of
  `/var/run/azure-cns/`.
- HNS (Host Network Service) dependency: CNS calls into HNS for endpoint
  creation. HNS is itself a native Windows Service that must be running
  before CNS does anything useful.

### What we don't have

We have **no Windows nodeinit-bench data**. The Linux 32 s baseline does
not transfer — Windows pod startup has different costs (image pulls
larger for non-HostProcess containers, container creation 5-15 s typical,
fewer daemonsets per node so the stampede is smaller). The decomposition
table at the top of this doc is Linux-only. **Anyone implementing these
proposals on Windows should run nodeinit-bench against a Windows
nodepool first** to confirm where Windows actually hurts. The tool itself
should work — Plotly + the conflist daemonset would need a HostProcess
variant for Windows, but the Go observer is platform-agnostic.

### Per-tier translation

| Tier | Linux | Windows equivalent | Notes |
|------|-------|--------------------|-------|
| **T1.1** pre-bake `azure-ipam` | bake into AKS Linux node image | bake into AKS Windows node image (already partially done — `azure-cns:v1.4.x` is preloaded, but azure-ipam Windows binary needs the same treatment) | same win, possibly larger because Windows image pulls are slower |
| **T1.2** drop init container | bake CNI binaries in node image | same — drop the init container from `azure-cns-windows.yaml` and ship CNI binaries on the Windows VHD | **bigger relative win** because Windows has fewer daemonsets to amortize the stampede across |
| **T1.3** reduce concurrent daemonsets | Cilium = 10 of 28 containers | Windows nodes don't run Cilium. Typical AKS Windows daemonsets: kube-proxy, azure-cns-win, csi-proxy, defender, csi-azuredisk-win, csi-azurefile-win — closer to ~10 containers total | smaller relative gain on Windows |
| **T1.4** taint-gated startup | works as-is | works as-is — k8s taints are platform-agnostic | no Windows-specific concerns |
| **T1.5** NC pre-programming | DNC-RC change | DNC-RC change | identical; DNC-RC doesn't care about node OS |
| **T2.1** static pod | kubelet `--pod-manifest-path=/etc/kubernetes/manifests/` | kubelet on Windows supports static pods at `C:\etc\kubernetes\manifests\` (or wherever AKS configures `--pod-manifest-path`) | **directly applicable**; updater DaemonSet T2.1.a works the same — DaemonSet pods can `hostPath`-mount the Windows manifest directory |
| **T2.2** systemd unit | systemd | **Windows Service via SCM** (Service Control Manager). The Windows analog is even more idiomatic than systemd is on Linux — `kubelet`, `containerd`, and `HNS` are all already Windows Services on AKS Windows nodes. CNS as a Windows Service slots into this naturally. | sequencing via SCM `DependsOn` — declare CNS depends on `hns` and starts before `kubelet`. **But: requires CNS to register itself with SCM** — currently CNS just runs as a regular EXE. Wrap with `golang.org/x/sys/windows/svc` (~50 lines of code) or use sc.exe + a helper. |
| **T2.4** CNS in-process inside CNI plugin | Linux: netlink in-process | Windows: HCN/HNS in-process. The `network/` Windows code already calls HCN APIs directly; restructuring would parallel the Linux refactor. | similar effort either platform |
| **T3.1** NC config baked in via CSE | cloud-init | **VMSS extension running PowerShell** (Windows AKS provisioning equivalent of CSE). Drops `nc-config.json` to a known path. | identical concept; just PowerShell instead of bash |
| **T3.3** eliminate CNS, native VPC | Linux netlink can be done in CNI plugin alone | Windows: harder — HNS is more centralized, more state to manage outside a daemon | less applicable to Windows |

### Windows-specific considerations

1. **HostProcess already eats most of the "exec gap"**. Windows
   HostProcess containers are essentially native processes wrapped by
   kubelet — no runc, no cgroup setup latency, no separate user
   namespace. The 14 s exec gap we see on Linux likely doesn't exist
   in the same form on Windows; the dominant cost shifts to "kubelet
   pod-sync waits for HNS" and "image pull bandwidth" instead. **This
   needs measurement**.

2. **HNS is the critical dependency, not netlink**. CNS on Windows calls
   the HCN API to create endpoints. HNS is a Windows Service that
   starts during boot. If we make CNS a Windows Service (T2.2), the
   correct service dependency is `DependsOnService=hns` (and ideally
   `BeforeService=kubelet` if we want CNS up before kubelet starts
   syncing pods). Today the DaemonSet model means CNS races HNS
   readiness on every fresh node — a small but measurable bug.

3. **The static-pod-plus-updater-DaemonSet pattern (T2.1.a) works
   unchanged on Windows.** Windows kubelet supports static pods;
   Windows DaemonSets can hostPath-mount any host directory; atomic
   file rename is available via `MoveFileEx` with `MOVEFILE_REPLACE_EXISTING`
   (the Go runtime's `os.Rename` already does this). The updater pod
   itself would be a HostProcess container with `hostPath`
   mounts of `C:\etc\kubernetes\manifests\` and `C:\opt\cni\bin\`.

4. **Windows Service is the better long-term fit than static pod**.
   Reasoning: CNS-as-Windows-Service would slot into the existing
   AKS Windows service-management model (kubelet, containerd, HNS,
   csi-proxy are all Services already). It picks up SCM-native
   features for free: automatic restart on crash, recovery actions
   on repeated failures, dependency-ordered startup, event log
   integration. Linux T2.2 (systemd) requires more bespoke wiring
   for the same outcomes; on Windows it's the conventional approach.

5. **Image upgrade with HostProcess containers** is straightforward
   because there's no OS layer — pulling a new HostProcess image is
   small and fast. The same updater DaemonSet pattern works; the
   pre-pull step (`crictl pull`) is even less risky.

### Recommended Windows path

1. **Run nodeinit-bench against a Windows nodepool first.** Without
   that data we are guessing. The Linux bottleneck profile is unlikely
   to match.
2. Apply T1.1 + T1.2 (pre-bake CNI binaries) — these win on both
   platforms and don't constrain future architectural choices.
3. **Skip Linux T2.1 (static pod) for Windows; go directly to
   Windows-T2.2 (Windows Service)** because:
   - SCM is the conventional Windows way; reviewers and operators
     understand it.
   - Native dependency on HNS via SCM is a feature that doesn't
     have a clean static-pod equivalent.
   - The Linux equivalent (systemd) is treated as a bigger
     architectural commitment, but on Windows it's the default.
   - Updater DaemonSet pattern (T2.1.a) still works — just have
     it call `sc.exe stop azure-cns; copy new exe; sc.exe start
     azure-cns` instead of swapping a manifest file.
4. T1.5 (NC pre-programming) and T3.1 (NC baked in via VMSS extension)
   apply unchanged.

---

## Out-of-scope but worth flagging

- **VM provision time** (87-107 s in our data, *before* T0). Not in `node-ready` SLI, but if AKS publishes "time-to-pod" the customer cares about it. Pre-warmed VM pools, surge nodepools, or dedicated host hot-pools are the standard answers.
- **Image pull bandwidth.** If MCR is geographically far, the 3 s init pull becomes 8 s. ACR cache or node-image preload (T1.1) sidesteps this.
