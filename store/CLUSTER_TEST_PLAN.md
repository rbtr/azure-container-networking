# CNS Store Backend — Cluster Benchmark Test Plan

## Objective

Measure the real-world impact of replacing the CNS JSON file store with BoltDB
or SQLite on **pod startup latency** across varying pod counts and node SKU
sizes. The synthetic micro-benchmarks (`store/BENCHMARKS.md`) showed BoltDB
4–8× faster than JSON for writes; this test plan validates whether that
translates to measurable pod scheduling improvements on live AKS clusters.

## Cluster Prerequisites

### BYOCNI Cluster
Create an AKS cluster with `--network-plugin none` (BYOCNI mode) so that CNS
can be installed and reconfigured independently:

```bash
az aks create \
  --resource-group $RG \
  --name $CLUSTER \
  --network-plugin none \
  --node-count 1 \
  --kubernetes-version 1.31 \
  --node-vm-size <varies per test> \
  --generate-ssh-keys
```

### CNS Image
Build a custom CNS image with the store backend support:

```bash
# From repo root
make cns-image \
  IMAGE_REGISTRY=<your-acr>.azurecr.io \
  CNS_VERSION=store-bench-$(git rev-parse --short HEAD)
make cns-image-push \
  IMAGE_REGISTRY=<your-acr>.azurecr.io
```

### CNS Installation
Deploy CNS as a DaemonSet with a ConfigMap specifying the store backend:

```bash
# Edit the ConfigMap in cns/azure-cns.yaml to set the desired StoreBackend:
#   "StoreBackend": "json"   (or "bbolt" or "sqlite")
kubectl apply -f cns/azure-cns.yaml
kubectl rollout status daemonset/azure-cns -n kube-system --timeout=120s
```

To switch backends between test runs:
```bash
kubectl get configmap cns-config -n kube-system -o json \
  | jq '.data["cns_config.json"] |= (fromjson | .StoreBackend = "bbolt" | tojson)' \
  | kubectl apply -f -
kubectl rollout restart daemonset/azure-cns -n kube-system
kubectl rollout status daemonset/azure-cns -n kube-system --timeout=120s
```

---

## Test Matrix

### Store Backends
| ID | Backend | Config Value |
|----|---------|-------------|
| J  | JSON file store (baseline) | `"json"` |
| B  | BoltDB (bbolt) | `"bbolt"` |
| S  | SQLite (modernc.org/sqlite) | `"sqlite"` |

### Pod Scale Targets
| Scale | Replicas |
|-------|----------|
| Small | 50 |
| Medium | 100 |
| Large | 200 |

### Node VM SKUs
| SKU | vCPUs | Memory | Rationale |
|-----|-------|--------|-----------|
| `Standard_B2s` | 2 | 4 GiB | Low-thread baseline: maximizes mutex contention visibility |
| `Standard_D4s_v5` | 4 | 16 GiB | Mid-range: typical dev/test node |
| `Standard_D16s_v5` | 16 | 64 GiB | High-thread: tests concurrency scaling with available parallelism |

### Full Matrix: 3 backends × 3 scales × 3 SKUs = 27 test runs

---

## Metrics to Collect

### Primary: Pod Startup Latency

**Kubelet metrics** (scraped from each node at `<nodeIP>:10255/metrics` or via
the metrics API):

| Metric | Description |
|--------|-------------|
| `kubelet_pod_start_sli_duration_seconds` | Time from kubelet seeing the pod to it being Running (SLI metric, histogram) |
| `kubelet_pod_start_duration_seconds` | Full pod start duration including image pull (histogram) |
| `kubelet_runtime_operations_duration_seconds{operation_type="create_container"}` | Container runtime overhead |

**Pod timestamp method** (works on any cluster version):

```bash
# After scaling, extract pod create→ready times:
kubectl get pods -l app=bench-pods -o json | jq -r '
  .items[] |
  (.metadata.creationTimestamp) as $created |
  (.status.conditions[] | select(.type=="Ready") | .lastTransitionTime) as $ready |
  "\(.metadata.name) \($created) \($ready)"
' | while read name created ready; do
  c=$(date -d "$created" +%s%3N)
  r=$(date -d "$ready" +%s%3N)
  echo "$name $((r - c)) ms"
done
```

Report **P50, P95, P99, Max** and **total wall-clock time** to schedule all
replicas.

### Secondary: CNS IPAM Latency

**CNS internal metrics** (scraped from CNS at `<nodeIP>:9090/metrics`):

| Metric | Description |
|--------|-------------|
| `cns_ipam_request_duration_seconds` | Time for CNS to process an IP config request |
| `cns_ipam_release_duration_seconds` | Time for CNS to release an IP config |

If these aren't exposed, instrument manually via CNS logs:

```bash
# From the CNS container logs, grep for IPAM request timing:
kubectl logs -n kube-system -l k8s-app=azure-cns --tail=1000 \
  | grep -E "requestIPConfig|releaseIPConfig"
```

### Tertiary: System Metrics

Collect via `kubectl top` or node-level monitoring:

| Metric | Purpose |
|--------|---------|
| CNS CPU usage | Backend CPU overhead |
| CNS memory RSS | Backend memory overhead |
| Node disk I/O (`iostat`) | Store file write amplification |
| CNS store file size on disk | Storage overhead per backend |

---

## Test Procedure

### Per-Run Protocol

For each (Backend, Scale, SKU) combination:

#### 1. Prepare Environment
```bash
# Ensure cluster is at correct SKU (provision once per SKU)
# Set the store backend
kubectl get configmap cns-config -n kube-system -o json \
  | jq --arg backend "$BACKEND" \
    '.data["cns_config.json"] |= (fromjson | .StoreBackend = $backend | tojson)' \
  | kubectl apply -f -
kubectl rollout restart daemonset/azure-cns -n kube-system
kubectl rollout status daemonset/azure-cns -n kube-system --timeout=120s

# Wait for CNS to be healthy
sleep 30
kubectl get pods -n kube-system -l k8s-app=azure-cns -o wide
```

#### 2. Clean State
```bash
# Delete any leftover test pods
kubectl delete namespace bench-test --ignore-not-found --wait=true
kubectl create namespace bench-test

# Clear CNS IPAM state by deleting the store file and restarting
kubectl rollout restart daemonset/azure-cns -n kube-system
kubectl rollout status daemonset/azure-cns -n kube-system --timeout=120s
sleep 15
```

#### 3. Collect Pre-Test Metrics
```bash
TIMESTAMP_PRE=$(date +%s%3N)

# Snapshot kubelet metrics
kubectl get --raw /api/v1/nodes/$NODE/proxy/metrics \
  | grep kubelet_pod_start_sli_duration > pre-metrics.txt

# Snapshot CNS metrics
kubectl port-forward -n kube-system daemonset/azure-cns 9090:9090 &
curl -s localhost:9090/metrics | grep cns_ipam > pre-cns-metrics.txt
```

#### 4. Execute Scale-Up
```bash
# Deploy pause pods (minimal overhead, measures pure scheduling + IPAM)
cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: bench-pods
  namespace: bench-test
spec:
  replicas: $SCALE
  selector:
    matchLabels:
      app: bench-pods
  template:
    metadata:
      labels:
        app: bench-pods
    spec:
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.10
        resources:
          requests:
            cpu: 10m
            memory: 16Mi
          limits:
            cpu: 10m
            memory: 16Mi
      terminationGracePeriodSeconds: 0
EOF

# Wait for all pods to be Running
kubectl rollout status deployment/bench-pods -n bench-test --timeout=600s
TIMESTAMP_POST=$(date +%s%3N)
WALL_CLOCK_MS=$((TIMESTAMP_POST - TIMESTAMP_PRE))
echo "Total wall-clock: ${WALL_CLOCK_MS}ms"
```

#### 5. Collect Post-Test Metrics
```bash
# Pod-level startup latency
kubectl get pods -n bench-test -l app=bench-pods -o json > pods-$BACKEND-$SCALE-$SKU.json

# Extract per-pod latency
jq -r '.items[] |
  (.metadata.creationTimestamp) as $c |
  (.status.conditions[] | select(.type=="Ready") | .lastTransitionTime) as $r |
  [.metadata.name, $c, $r] | @tsv' \
  pods-$BACKEND-$SCALE-$SKU.json > latencies-$BACKEND-$SCALE-$SKU.tsv

# Kubelet metrics delta
kubectl get --raw /api/v1/nodes/$NODE/proxy/metrics \
  | grep kubelet_pod_start_sli_duration > post-metrics.txt

# CNS metrics delta
curl -s localhost:9090/metrics | grep cns_ipam > post-cns-metrics.txt

# System metrics
kubectl top pod -n kube-system -l k8s-app=azure-cns > cns-resources-$BACKEND-$SCALE-$SKU.txt

# Store file size on disk
kubectl exec -n kube-system daemonset/azure-cns -- \
  ls -la /var/lib/azure-network/ /var/run/azure-cns/ 2>/dev/null \
  > store-size-$BACKEND-$SCALE-$SKU.txt
```

#### 6. Tear Down
```bash
kubectl delete namespace bench-test --wait=true
sleep 15  # allow CNS to release all IPs
```

#### 7. Repeat
Run each configuration **3 times** to establish statistical confidence.

---

## Automated Test Harness (Go)

All test execution, metrics collection, and analysis is automated in a single
Go test at `test/integration/storebench/storebench_test.go`. It:

- Switches the CNS ConfigMap `StoreBackend` and triggers a DaemonSet rollout
- Creates/deletes a pause-pod Deployment pinned to the target node
- Measures per-pod `creationTimestamp → Ready` latency
- Computes P50/P95/P99/Max statistics
- Persists per-run JSON results and a combined CSV
- Generates a SUMMARY.md with an aggregated markdown table

### Running the harness

```bash
# Full matrix (≈2 hours)
BACKENDS="json bbolt sqlite" SCALES="50 100 200" RUNS=3 \
  go test -timeout 120m -tags storebench -count=1 -v \
    ./test/integration/storebench/ -run ^TestStoreBench$

# Quick smoke test
BACKENDS=json SCALES=10 RUNS=1 \
  go test -timeout 10m -tags storebench -count=1 -v \
    ./test/integration/storebench/ -run ^TestStoreBench$

# Pin to a specific node
NODE=aks-bench4-12345678-vmss000000 BACKENDS="json bbolt" SCALES="50 100" RUNS=2 \
  go test -timeout 60m -tags storebench -count=1 -v \
    ./test/integration/storebench/

# Specify output directory
OUTDIR=/tmp/bench-results BACKENDS=bbolt SCALES=200 RUNS=3 \
  go test -timeout 30m -tags storebench -count=1 -v \
    ./test/integration/storebench/
```

### Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `BACKENDS` | `json bbolt sqlite` | Space-separated list of backends to test |
| `SCALES` | `50 100 200` | Space-separated list of pod counts |
| `RUNS` | `3` | Number of repetitions per combination |
| `OUTDIR` | `./storebench-results` | Directory for result JSON/CSV/summary |
| `NODE` | *(auto)* | Target node name; auto-selects `benchmark=true` or first Ready |

Results land in `$OUTDIR/`:
- `result-<backend>-<scale>-<sku>-run<N>.json` — full per-run data
- `wall-clock.csv` — one-line-per-run for easy import
- `SUMMARY.md` — aggregated markdown table

---

## Expected Outcomes

Based on the micro-benchmarks (store/BENCHMARKS.md):

| Scenario | JSON Prediction | BoltDB Prediction | SQLite Prediction |
|----------|----------------|-------------------|-------------------|
| 50 pods on B2s (2 vCPU) | Baseline | 15-25% faster total schedule time | 10-15% faster |
| 200 pods on B2s (2 vCPU) | Baseline (high contention) | **30-50% faster** — contention amplifies the 8× write advantage | 15-25% faster |
| 200 pods on D16s (16 vCPU) | Baseline (less contention) | 10-20% faster — more CPUs reduce mutex pressure | 5-15% faster |

**Key hypothesis**: The improvement should be **most visible** on:
- Low-vCPU nodes (B2s) where thread contention is highest
- High pod counts (200) where the O(n) write amplification of JSON is worst
- Concurrent scheduling bursts (not trickle scheduling)

If the store write is not the dominant factor in pod startup (e.g., image pull
or CNI plugin execution dominates), the improvement may be smaller than the
micro-benchmark ratio. That's why we use `pause` pods (pre-pulled) to isolate
the IPAM path.

---

## Pre-Test Checklist

- [ ] BYOCNI AKS cluster provisioned with target SKU
- [ ] Custom CNS image built and pushed to ACR
- [ ] `pause:3.10` image pre-pulled on all nodes (`kubectl debug node/$NODE -it --image=registry.k8s.io/pause:3.10`)
- [ ] CNS DaemonSet deployed and healthy
- [ ] `kubectl top` / metrics-server functional
- [ ] Node port 10255 or metrics API accessible for kubelet metrics
- [ ] Test namespace `bench-test` does not exist (clean start)
- [ ] Sufficient IP address space for 200 pods on the node

---

## SKU Rotation Procedure

When moving to a new VM SKU, create a new node pool rather than replacing:

```bash
# Add new node pool with target SKU
az aks nodepool add \
  --resource-group $RG \
  --cluster-name $CLUSTER \
  --name "bench${VCPU}" \
  --node-count 1 \
  --node-vm-size $SKU \
  --max-pods 250 \
  --labels benchmark=true

# Taint other node pools to prevent scheduling there
az aks nodepool update \
  --resource-group $RG \
  --cluster-name $CLUSTER \
  --name nodepool1 \
  --node-taints "nobench=true:NoSchedule"

# Add nodeSelector to bench-pods deployment:
#   nodeSelector:
#     benchmark: "true"
```

After testing all 3 backends on that SKU, delete the node pool and create the
next one.

---

## Output Deliverables

1. **Per-run JSON**: `$OUTDIR/result-<backend>-<scale>-<sku>-run<N>.json` (full latency data, per-pod breakdown)
2. **CSV**: `$OUTDIR/wall-clock.csv` — wall-clock ms per run, easy to import to sheets/pandas
3. **Summary table**: `$OUTDIR/SUMMARY.md` — aggregated P50/P95/P99 per (Backend, Scale, SKU)
4. **Recommendation**: Go/no-go on BoltDB migration based on cluster evidence
