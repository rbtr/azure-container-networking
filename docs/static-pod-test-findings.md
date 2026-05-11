# Static-Pod CNS — Test Findings (final)

**Status:** Variants A-pre and A-pull complete (10 runs each, N=20 total).
Variants B-pre and B-pull **could not be run** on AKS-managed VMSSes
after exhausting all reasonable extension-based injection mechanisms
(four attempts across three publishers — see "Blockers found" below).
The information from A-pre/A-pull plus the confirmed blockers is
itself decision-quality input for the node-readiness-improvements
direction: T2.1 (static pod) needs a custom AKS VHD to be testable
end-to-end, and T2.2 (systemd unit) sidesteps every issue we hit.

## Cluster

- BYOCNI overlay: `evanbaker-staticpod-westus2`, K8s 1.33, Ubuntu 22.04, 2 ×
  Standard_B2s nodes, kernel 5.15
- DNC-RC available; CNS deployed via `cns/azure-cns.yaml` derived for
  minimum-isolation (no init container, no cni-installer DaemonSet)
- CNS image `mcr.microsoft.com/containernetworking/azure-cns:v1.8.6-0`
  is pre-baked in the AKS BYOCNI VHD (verified via `crictl images` —
  P3); `:v1.8.1-0` is published on MCR but NOT pre-baked, used for the
  forced-pull arm

## Variant A-pre (DaemonSet CNS, image preloaded) — 10 runs

| Phase | p50 | p95 | max |
|-------|----:|----:|----:|
| **node-ready** | **47.0 s** | **53.0 s** | **53.0 s** |
| `cns-pod-schedule-latency` | 0.9 s | 1.1 s | 1.1 s |
| `cns-image-pull` | 0.0 s | 0.0 s | 0.0 s |
| `cns-container-start` | 1.0 s | 2.0 s | 2.0 s |
| `cns-exec-gap` | **35.2 s** | 39.4 s | 40.5 s |
| `cns-process-bootstrap` | 2.4 s | 3.3 s | 3.5 s |
| `cns-listener-ready` | 37.1 s | 42.3 s | 43.1 s |
| `cns-conflist-write` | 37.0 s | 43.2 s | 44.0 s |

The dominant phase is **`cns-exec-gap` at 35 s p50** — the gap between
containerd reporting the container as Started and the Go binary's first
log line. CNS's actual code path runs in ~2 s.

## Variant A-pull (DaemonSet CNS, image NOT pre-baked) — 10 runs

| Phase | p50 | p95 | max |
|-------|----:|----:|----:|
| **node-ready** | **40.0 s** | **47.0 s** | **47.0 s** |
| `cns-pod-schedule-latency` | 0.9 s | 1.1 s | 1.1 s |
| `cns-image-pull` | **23.0 s** | 29.0 s | 29.0 s |
| `cns-container-start` | 6.0 s | 7.0 s | 7.0 s |
| `cns-exec-gap` | **0.8 s** | 1.0 s | 1.1 s |
| `cns-process-bootstrap` | 0.1 s | 0.2 s | 0.3 s |
| `cns-listener-ready` | 1.4 s | 1.6 s | 1.6 s |
| `cns-conflist-write` | 2.0 s | 2.6 s | 3.0 s |

## A-pre vs A-pull — counterintuitive result

**A-pull (image not pre-baked) is faster than A-pre (image pre-baked) by
7 s at p50** (−14.9 %).

Per-span:

| span | A-pre p50 | A-pull p50 | delta |
|------|----------:|-----------:|------:|
| `cns-image-pull` | 0.0 s | 23.0 s | +23.0 s |
| `cns-exec-gap` | 35.2 s | 0.8 s | −34.4 s |
| `cns-listener-ready` (= bootstrap) | 37.1 s | 1.4 s | −35.7 s |
| `cns-conflist-write` | 37.0 s | 2.0 s | −35.0 s |

### Mechanism

The image pull "absorbs" the daemonset-stampede contention that would
otherwise show up as `cns-exec-gap`:

- **A-pre**: image is already present → containerd marks "Started"
  immediately → but the kernel/containerd is still busy executing other
  daemonsets that scheduled at the same time → CNS's Go binary doesn't
  actually exec for ~35 s
- **A-pull**: image pull takes 23 s (during which time other
  daemonsets are also pulling and exec'ing) → by the time `azure-cns`
  is unpacked and Pulled fires, the node has settled → exec is fast

**Implication for the static-pod hypothesis:** the savings target was
"reduce exec-gap by removing CNS from the daemonset stampede." But our
data show that the *real* time absorbed by stampede shifts between
`cns-image-pull` and `cns-exec-gap` depending on whether the image is
preloaded. Total `node-ready` is 7 s lower when forced to pull (because
stampede latency is partially overlapped with pull instead of being
strictly serialized after Pulled fires).

This means **pre-baking the image is not the unambiguous win we
assumed**. On a contended fresh node, "the image is already there" just
shifts where the wait happens.

## Blockers found while attempting variants B-pre / B-pull

These are genuinely valuable findings independent of the test outcome:

### Blocker 1 — Mirror pods cannot reference ServiceAccounts

`kubelet[5275]: E0506 23:25:19.236536 ... "Failed creating a mirror
pod" err="pods \"azure-cns-static-aks-nodepool1-...\" is forbidden:
**a mirror pod may not reference service accounts**"`

Static pod manifests are forbidden from declaring `serviceAccountName`.
The kubelet refuses to create the mirror pod in the apiserver, so the
pod runs locally on the node but is invisible to `kubectl get pods` and
RBAC.

**Implication for production design (T2.1.a):** a real static-pod CNS
must use **a hostPath kubeconfig** with a long-lived (or rotated) token
for the `azure-cns` ServiceAccount instead of a projected SA token.
This is the same pattern kubelet itself uses (`/var/lib/kubelet/kubeconfig`).
Adds operational complexity:
- Token rotation responsibility (likely the updater DaemonSet)
- Initial token bootstrap (CSE / VHD pre-bake)
- Token revocation if a node is compromised
- Kubelet's own NodeAuthorizer doesn't help (CNS isn't kubelet)

We confirmed this works: a 24 h token + kubeconfig dropped on disk +
`KUBECONFIG` env var lets CNS authenticate as `system:serviceaccount:
kube-system:azure-cns`. The static pod ran successfully on the existing
nodes once we did this.

### Blocker 2 — Adding `Microsoft.Azure.Extensions.CustomScript` to an AKS-managed VMSS REPLACES AKS's bootstrap CSE (`vmssCSE`)

**Root cause confirmed via isolated repro on a fresh cluster
(`evanbaker-csedebug-westus2`, 2026-05-07).**

This is **not an AKS-specific lockout.** It's how Azure VM Extensions
work fundamentally: per Azure docs, "You cannot deploy two extensions
of the same publisher and type to one VM at the same time." Azure VM
Agent (waagent) treats `publisher.type` as the unique installation key
for an extension — installing a "second" copy of the same publisher.type
is treated as an update and overwrites the first install.

AKS uses an extension named `vmssCSE` with publisher
`Microsoft.Azure.Extensions` and type `CustomScript` v2.0 to bootstrap
its nodes. The script `vmssCSE` runs:
- writes `/etc/systemd/system/kubelet.service.d/`
- configures containerd
- joins the cluster
- starts kubelet

When we ran `az vmss extension set --publisher Microsoft.Azure.Extensions
--name CustomScript` (the obvious / most-documented path for adding a
script extension), Azure replaced `vmssCSE` with our extension because
they share the same publisher.type slot. Verified by direct
BEFORE/AFTER comparison of the VMSS extension list:

```
BEFORE:  vmssCSE | Microsoft.Azure.Extensions
         AKSLinuxBilling | Microsoft.AKS
         AKSLinuxExtension | Microsoft.AKS

AFTER `az vmss extension set`:
         AKSLinuxBilling | Microsoft.AKS
         AKSLinuxExtension | Microsoft.AKS
         CustomScript | Microsoft.Azure.Extensions     ← ours; vmssCSE GONE
```

Subsequent scaling: new VM provisions, but kubelet never starts and
`/etc/systemd/system/kubelet.service.d/` does not exist (those are
written by the now-deleted vmssCSE script).

**Why the friction:** every Azure CLI / ARM tutorial reaches for
`Microsoft.Azure.Extensions.CustomScript` as "the" CSE. AKS happens to
have claimed that slot for its bootstrap. So the obvious approach
collides; you have to deliberately use a different publisher.type.

**Empirically tested alternatives — all extension paths failed:**

| Mechanism | Result | Cluster |
|-----------|--------|---------|
| `Microsoft.Azure.Extensions.CustomScript` v2.0 | **REPLACES vmssCSE** (publisher.type collision) → kubelet never installed on new VMs | `evanbaker-staticpod-westus2`, `evanbaker-csedebug-westus2` |
| `Microsoft.CPlat.Core.RunCommandLinux` v1.0 | Coexists in VMSS extension list, but **new instance stuck in `Creating`** for 10+ min, eventually marked Failed. Also blocks `az vmss run-command invoke`. | `evanbaker-cseretry-westus2`, `evanbaker-staticpod2-westus2` |
| `Microsoft.CPlat.Core.RunCommandHandlerLinux` v1.3 | **Per-instance only.** Run Command v2 model uses `az vmss run-command create --instance-id`; not a VMSS-template-level extension. Will not run automatically on newly-scaled instances. | `evanbaker-staticpod2-westus2` |
| `Microsoft.OSTCExtensions.CustomScriptForLinux` v1.5 | Different publisher entirely (no publisher.type collision), vmssCSE intact in VMSS model. **New instance still went `Creating → Deleting`.** vmAgent never reported any extension status; per-instance view showed no extensions at all on the failed VM. AKS reaped the instance after ~13 min. | `evanbaker-staticpod2-westus2` |

**Conclusion: extension-based injection of static-pod manifest at boot
is not viable on AKS-managed VMSSes.** Across three publishers and
four extension types, every attempt either replaced vmssCSE
(publisher.type collision) or caused new-instance bootstrap to fail
even though vmssCSE was still present in the VMSS model. The OSTC
result is the most informative: the new instance came up to
PowerState=running with osProvisioningComplete, but the VM Agent never
reported, so neither vmssCSE nor our extension ever ran. This is
consistent with AKS's reconciler treating user-added VMSS extensions
as drift and refusing to provision new instances cleanly when they're
present, but we could not capture decisive logs to fully prove that
mechanism. Either way, the empirical answer is the same: no.

**Mechanisms still on the table — not tested:**

| Mechanism | Notes |
|-----------|-------|
| Custom AKS node image (VHD bake-in) | Production-preferred path. The static-pod manifest, kubeconfig, and `cns_config.json` ship in `/etc/kubernetes/manifests/` and adjacent paths in the VHD. No VMSS extension needed; vmssCSE is untouched. Highest effort but the only path that puts the manifest on disk strictly before kubelet starts. |
| AKS-supported bootstrap hooks (if any) | `--node-os-config`, `--linux-os-config`, AKS GA preview features. None we've seen accept "drop these files into kubelet's manifests dir." Worth confirming with AKS team before a production design commits to VHD bake-in. |
| DaemonSet writer (k8s-native) | Drops the manifest *after* kubelet starts. Tests a weaker form of the hypothesis — does NOT validate "manifest exists before kubelet starts" — but is simplest. Out of scope for this experiment per user direction. |

**Implication for production design:** the static-pod-at-boot
hypothesis cannot be validated on AKS-managed VMSSes via VM
extensions. To validate it the experiment would need either (a) custom
AKS VHD with the manifest pre-baked, or (b) self-managed VMSS where we
own provisioning. Both are substantially more setup than the
extension path we attempted.

### Blocker 3 — Cluster recoverability after extension manipulation

Once a VMSS reaches `Failed` provisioning state from extension
manipulation, simple operations (scale, instance delete) cannot
recover it. We observed this on `evanbaker-staticpod-westus2`. Required
recovery is either re-running the AKS bootstrap manually on each
broken VM (deep AKS internal knowledge) or destroying and recreating
the cluster.

**Implication:** any future AKS-side static-pod testing should be done
on disposable clusters with `make down` ready to go.

## What we still don't know

- Whether static-pod CNS *itself* (without the bootstrap-time injection
  benefit) is meaningfully faster than DaemonSet CNS on a stable node.
  This could still be tested via the writer-DaemonSet path on a fresh
  cluster. **Out of scope per user direction** — does not test the
  "manifest before kubelet" hypothesis.
- What the savings would be if the manifest was baked into the AKS node
  image (the only production-real path that would actually deliver the
  static-pod-before-kubelet benefit). Requires a custom AKS image
  build — significantly more setup than the extension path we
  attempted.

## Updated recommendations for `node-readiness-improvements.md`

T2.1 (CNS as static pod) needs the following amendments based on what
we learned:

1. **Mirror-pod SA restriction must be addressed in the production
   design.** The static pod must use a hostPath kubeconfig + long-lived
   token. The updater DaemonSet (T2.1.a) has additional responsibility:
   token rotation. Without rotation, CNS will lose apiserver access
   when the token expires.

2. **CSE-via-VMSS-extension is not the right injection mechanism for
   AKS.** Production rollout requires either AKS node image bake-in or
   an AKS-supported bootstrap hook.

3. **The "static pod is faster because no stampede" hypothesis needs
   further validation.** Our A-pre vs A-pull data shows that stampede
   latency is shape-shifty: it absorbs into pull if there is one, into
   exec-gap if there isn't. Static pod *might* eliminate exec-gap by
   getting the manifest into kubelet's pod-sync queue earlier, but the
   total savings could be smaller than predicted because the stampede
   bound on overall throughput is the same.

4. **Static pod ALONE doesn't address the largest contributor.** The
   `cns-exec-gap` of 35 s in our A-pre data is kernel/containerd
   contention from concurrent container starts. Static pod might let
   CNS jump the kubelet queue, but containerd still has to serialize
   all the other pod creations. A more comprehensive fix would be
   either:
   - **T1.4 (taint-gated startup)** to actually reduce concurrent
     pulls, OR
   - **T2.2 (systemd unit)** which removes CNS from the
     containerd/kubelet flow entirely.

The original framing of T2.1 as "the cheaper, lower-risk halfway"
should be reconsidered given the kubeconfig-bootstrap and CSE-AKS
conflict findings. T2.2 (systemd unit) avoids both blockers cleanly:
no static-pod SA issue (CNS isn't a pod at all) and no AKS extension
conflict (the systemd unit ships in the node image, not as an
extension). Combined with the larger expected savings, **T2.2 is
likely the better target for the architectural commitment.**
