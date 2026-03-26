# Plan: RTNL Lock Contention Reduction for Pod Startup

## Problem

Pod startup at scale (50-150 concurrent pods) is dominated by kernel RTNL lock contention, not CNS IPAM. Our CNS bolt/pool optimizations reduced CNS IPAM from ~10ms to <1ms per pod, but wall-clock pod startup remains 10-24s mean because the CNI endpoint setup does multiple netlink syscalls that contend on the kernel's RTNL mutex.

**Measured:** With 150 concurrent pods on D8ads_v6, kubelet SLI mean is 23.5s. CNS contributes <0.1% of that.

## Phase 0 Finding: RTNL Scope on Kernel 6.8 (COMPLETE)

**Tested on kernel 6.8.0-1046-azure (AKS D8ads_v6):**

| Test | Serial | Parallel (50 NS) | Speedup | Conclusion |
|------|--------|-------------------|---------|------------|
| Container-NS addr+route | 46.6ms | 13.7ms | **3.4x** | **Per-netns RTNL** |
| Container-NS addr+route (100 NS) | 92.1ms | 28.3ms | **3.3x** | **Per-netns RTNL** |
| Host-NS veth creation (50 NS) | 55.0ms | 66.1ms | **0.8x** | **Global RTNL** |
| Host-NS veth creation (100 NS) | 13.9ms | 45.0ms | **0.3x** | **Global RTNL (worse under contention)** |

**Critical insight:** On kernel 6.8, `RTM_NEWADDR` and `RTM_NEWROUTE` inside container netns use **per-netns RTNL** — they do NOT contend globally. Only host-namespace operations use the global RTNL lock.

## Revised Root Cause Analysis

For the TransparentEndpointClient (stateless CNI overlay), per dual-stack pod:

### Host-NS Operations (GLOBAL RTNL — the actual bottleneck)

| # | Operation | RTM Type | Count |
|---|-----------|----------|-------|
| 1 | Create veth pair | RTM_NEWLINK | 1 |
| 2 | Bring up host veth | RTM_SETLINK | 1 |
| 3 | Set MTU host veth | RTM_SETLINK | 1 |
| 4 | Set MTU container veth | RTM_SETLINK | 1 |
| 5 | Add /32 IPv4 route (host) | RTM_NEWROUTE | 1 |
| 6 | Add /128 IPv6 route (host) | RTM_NEWROUTE | 1 |
| 7 | Move veth to container NS | RTM_SETLINK | 1 |
| **Total** | | | **7 syscalls × global RTNL** |

Non-netlink host-side: 2 sysctl calls (DisableRA, setArpProxy) — no RTNL.

### Container-NS Operations (PER-NETNS RTNL — non-contending)

| Operation | Approx Count |
|-----------|-------------|
| Interface rename, state changes | 3 |
| Add IPv4 + IPv6 addresses | 2 |
| Delete/add routes | ~6-8 |
| ARP/neighbor entries | 2-3 |
| **Total** | **~13-16 syscalls × per-netns (non-contending)** |

**Revised contention: 150 pods × 7 global ops = ~1050 serialized kernel ops** (down from 1500-2100 assumed earlier). Still significant at ~50-100µs per op = 50-100ms total serialized.

## Revised Strategy (Re-prioritized)

### Strategy 1: CNI Concurrency Limiter (Quick Win — highest ROI)
With only 7 global RTNL ops per pod, the queueing problem is: 150 × 7 = 1050 ops on one lock. With 8 concurrent CNI processes (matching vCPU count), only 56 in-flight at a time. At ~70µs per op, that's ~4ms of RTNL wait per pod instead of ~73ms.

**Implementation:**
- flock-based semaphore at `/var/run/azure-cni.sem` (or similar)
- Acquire AFTER IPAM (CNS call) but BEFORE `EndpointCreate()`
- Release AFTER `EndpointCreate()` returns
- Limit = runtime.NumCPU() (auto-detected, or tunable via conflist)
- Each CNI process: IPAM (parallel, fast) → acquire sem → kernel setup (limited concurrency) → release

### Strategy 2: Netlink Batching — Host-Side Only (Medium)
Now that we know container-side is per-netns, batching only needs to target the 7 host-side ops. Combine into 2 batches:
- Batch A: RTM_NEWLINK (veth) + RTM_SETLINK (up + MTU × 2) = 1 round-trip for 4 msgs
- Batch B: RTM_NEWROUTE × 2 + RTM_SETLINK (move to NS) = 1 round-trip for 3 msgs
- Net: 7 ops → 2 kernel round-trips. With concurrency limiter, total serialized = 150 × 2 = 300 ops.

### Strategy 3: Veth Pool (If needed)
Pre-create veth pairs. CNI only does: pop veth (no RTM_NEWLINK) + add host routes (2 ops) + move to NS (1 op) = 3 global ops per pod. With batching: 1 round-trip.

## Implementation Order

- [x] **Phase 0: Kernel RTNL Scope Verification** — DONE
- [ ] **Phase 1: CNI Concurrency Limiter** — add flock semaphore to stateless CNI
- [ ] **Phase 2: Netlink Batching** — batch 7 host-side ops into 2 round-trips
- [ ] **Phase 3: Benchmark** — storebench at 50/100/150 scale
- [ ] **Phase 4: Veth Pool** — only if Phase 1+2 insufficient

## Key Files

| File | Role |
|------|------|
| `cni/network/network.go` | CNI ADD handler — add semaphore around EndpointCreate |
| `cni/network/stateless/main.go` | Stateless CNI entry point |
| `network/endpoint_linux.go` | Endpoint creation sequence |
| `network/transparent_endpointclient_linux.go` | TransparentEndpointClient (overlay mode) |
| `netlink/socket.go` | Netlink socket — add batch send/recv |
| `netlink/link_linux.go` | Veth/link operations |
| `netlink/ip_linux.go` | Address/route operations |

## Success Criteria

- 150-pod wall-clock startup time reduced from ~40s to <30s (25%+ improvement)
- No regression in pod networking correctness
- No increase in CNI ADD failure rate
- Benchmarked on same D8ads_v6 cluster with storebench harness
