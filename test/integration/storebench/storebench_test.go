//go:build storebench

// Package storebench runs cluster-level benchmarks of the CNS store backend
// by deploying pause pods and measuring pod startup latency.
//
// Run with:
//
//	BACKENDS="json bbolt sqlite" SCALES="50 100 200" RUNS=3 \
//	  go test -timeout 120m -tags storebench -v -run ^TestStoreBench$
//
// Or for a quick smoke test:
//
//	BACKENDS=json SCALES=10 RUNS=1 \
//	  go test -timeout 10m -tags storebench -v -run ^TestStoreBench$
package storebench

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-container-networking/test/internal/kubernetes"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
)

// config is loaded from environment variables via LoadEnvironment.
type config struct {
	Backends string `env:"BACKENDS" default:"json bbolt sqlite"`
	Scales   string `env:"SCALES"   default:"50 100 200"`
	Runs     int    `env:"RUNS"     default:"3"`
	OutDir   string `env:"OUTDIR"   default:"./results"`
	Node     string `env:"NODE"     default:""`
}

const (
	cnsNamespace  = "kube-system"
	cnsConfigMap  = "cns-config"
	cnsLabel      = "k8s-app=azure-cns"
	testNamespace = "storebench"
	testLabel     = "app=storebench"
	deployName    = "storebench"
)

var cfg config

func TestMain(m *testing.M) {
	loadEnvironment(&cfg)
	os.MkdirAll(cfg.OutDir, 0o755) //nolint:errcheck // best-effort
	os.Exit(m.Run())
}

// TestStoreBench is the main entry point for the cluster benchmark.
func TestStoreBench(t *testing.T) {
	ctx := context.Background()
	clientset := kubernetes.MustGetClientset()

	// Resolve target node.
	node := resolveNode(ctx, t, clientset, cfg.Node)
	sku := nodeVMSize(ctx, clientset, node)
	t.Logf("Target node: %s (SKU: %s)", node, sku)

	backends := strings.Fields(cfg.Backends)
	scales := parseIntList(t, cfg.Scales)

	// Pre-pull pause image on the target node.
	prePullPause(ctx, t, clientset, node)

	// CSV header.
	csvPath := filepath.Join(cfg.OutDir, "wall-clock.csv")
	writeFile(t, csvPath, "backend,scale,sku,run,wall_clock_ms,sli_count,sli_mean_s\n")

	// Run matrix.
	for _, backend := range backends {
		switchBackend(ctx, t, clientset, backend, node)

		for _, scale := range scales {
			for run := 1; run <= cfg.Runs; run++ {
				tag := fmt.Sprintf("%s-%d-%s-run%d", backend, scale, sku, run)
				t.Run(tag, func(t *testing.T) {
					result := runScaleTest(ctx, t, clientset, node, scale)
					result.Backend = backend
					result.Scale = scale
					result.SKU = sku
					result.Run = run

					// Persist results.
					saveResult(t, cfg.OutDir, tag, result)
					sliCount := uint64(0)
					sliMean := 0.0
					if result.KubeletSLI != nil {
						sliCount = result.KubeletSLI.SampleCount
						sliMean = result.KubeletSLI.MeanSeconds
					}
					appendCSV(t, csvPath, fmt.Sprintf("%s,%d,%s,%d,%d,%d,%.4f\n",
						backend, scale, sku, run, result.WallClockMS, sliCount, sliMean))

					t.Logf("Wall: %dms | Pods: %d | P50: %.2fs | P95: %.2fs | P99: %.2fs | Max: %.2fs",
						result.WallClockMS, result.PodCount,
						result.P50, result.P95, result.P99, result.Max)
					if result.KubeletSLI != nil && result.KubeletSLI.SampleCount > 0 {
						t.Logf("Kubelet SLI: %d pods started, mean %.3fs",
							result.KubeletSLI.SampleCount, result.KubeletSLI.MeanSeconds)
					}
				})
			}
		}
	}

	// Generate summary.
	generateSummary(t, cfg.OutDir)
}

// ──────────────────────────────────────────────────────────────────
// Result types
// ──────────────────────────────────────────────────────────────────

type benchResult struct {
	Backend     string       `json:"backend"`
	Scale       int          `json:"scale"`
	SKU         string       `json:"sku"`
	Run         int          `json:"run"`
	WallClockMS int64        `json:"wall_clock_ms"`
	PodCount    int          `json:"pod_count"`
	P50         float64      `json:"p50"`
	P95         float64      `json:"p95"`
	P99         float64      `json:"p99"`
	Max         float64      `json:"max"`
	Mean        float64      `json:"mean"`
	Latencies   []podLatency `json:"latencies"`

	// Kubelet SLI metrics (delta between pre/post scrape).
	KubeletSLI *kubeletSLIResult `json:"kubelet_sli,omitempty"`
}

// kubeletSLIResult holds the delta of kubelet_pod_start_sli_duration_seconds
// histogram observed during a single benchmark run.
type kubeletSLIResult struct {
	SampleCount uint64             `json:"sample_count"`
	SampleSum   float64            `json:"sample_sum"`
	MeanSeconds float64            `json:"mean_seconds"`
	Buckets     []histogramBucket  `json:"buckets"`
}

type histogramBucket struct {
	UpperBound      float64 `json:"le"`
	CumulativeCount uint64  `json:"count"`
}

type podLatency struct {
	Name      string  `json:"name"`
	LatencyMS float64 `json:"latency_ms"`
}

// ──────────────────────────────────────────────────────────────────
// Core test logic
// ──────────────────────────────────────────────────────────────────

func runScaleTest(ctx context.Context, t *testing.T, clientset *k8s.Clientset, node string, scale int) benchResult {
	t.Helper()
	cleanNamespace(ctx, t, clientset)
	ensureNamespace(ctx, t, clientset)

	// Snapshot kubelet SLI metrics before the scale-up.
	preSLI := scrapeKubeletSLI(ctx, t, clientset, node)

	tStart := time.Now()

	// Create the deployment pinned to the target node.
	deployment := makePauseDeployment(node, int32(scale))
	_, err := clientset.AppsV1().Deployments(testNamespace).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create deployment: %v", err)
	}

	// Wait for all pods to become Ready.
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	err = kubernetes.WaitForPodDeployment(waitCtx, clientset, testNamespace, deployName, testLabel, scale)
	if err != nil {
		t.Fatalf("Pods did not reach Ready: %v", err)
	}

	wallClock := time.Since(tStart)

	// Snapshot kubelet SLI metrics after the scale-up.
	postSLI := scrapeKubeletSLI(ctx, t, clientset, node)

	// Collect per-pod latencies.
	latencies := collectPodLatencies(ctx, t, clientset)

	// Clean up for next run.
	cleanNamespace(ctx, t, clientset)

	result := benchResult{
		WallClockMS: wallClock.Milliseconds(),
		PodCount:    len(latencies),
		Latencies:   latencies,
		KubeletSLI:  computeSLIDelta(t, preSLI, postSLI),
	}

	if len(latencies) > 0 {
		secs := make([]float64, len(latencies))
		for i, l := range latencies {
			secs[i] = l.LatencyMS / 1000.0
		}
		sort.Float64s(secs)
		result.P50 = percentile(secs, 0.50)
		result.P95 = percentile(secs, 0.95)
		result.P99 = percentile(secs, 0.99)
		result.Max = secs[len(secs)-1]
		sum := 0.0
		for _, s := range secs {
			sum += s
		}
		result.Mean = sum / float64(len(secs))
	}

	return result
}

func collectPodLatencies(ctx context.Context, t *testing.T, clientset *k8s.Clientset) []podLatency {
	t.Helper()
	pods, err := clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: testLabel,
	})
	if err != nil {
		t.Fatalf("Failed to list pods: %v", err)
	}

	var latencies []podLatency
	for i := range pods.Items {
		pod := &pods.Items[i]
		created := pod.CreationTimestamp.Time

		var ready time.Time
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				ready = cond.LastTransitionTime.Time
				break
			}
		}
		if ready.IsZero() {
			t.Logf("WARNING: pod %s has no Ready condition", pod.Name)
			continue
		}

		latMS := float64(ready.Sub(created).Milliseconds())
		latencies = append(latencies, podLatency{
			Name:      pod.Name,
			LatencyMS: latMS,
		})
	}
	return latencies
}

// ──────────────────────────────────────────────────────────────────
// Kubelet SLI metrics
// ──────────────────────────────────────────────────────────────────

const kubeletSLIMetric = "kubelet_pod_start_sli_duration_seconds"

// scrapeKubeletSLI fetches the kubelet_pod_start_sli_duration_seconds histogram
// from the kubelet on the given node via the API server node proxy.
func scrapeKubeletSLI(ctx context.Context, t *testing.T, clientset *k8s.Clientset, node string) *dto.MetricFamily {
	t.Helper()

	// The node proxy endpoint gives us the kubelet's /metrics/slis.
	// Fall back to /metrics if /metrics/slis is unavailable.
	for _, path := range []string{"/metrics/slis", "/metrics"} {
		data, err := clientset.CoreV1().RESTClient().Get().
			AbsPath("/api/v1/nodes", node, "proxy"+path).
			DoRaw(ctx)
		if err != nil {
			t.Logf("Kubelet metrics scrape via %s failed: %v", path, err)
			continue
		}

		families, err := parsePrometheusMetrics(data)
		if err != nil {
			t.Logf("Failed to parse kubelet metrics from %s: %v", path, err)
			continue
		}

		if fam, ok := families[kubeletSLIMetric]; ok {
			t.Logf("Scraped %s from %s%s: %d samples",
				kubeletSLIMetric, node, path, len(fam.GetMetric()))
			return fam
		}
	}

	t.Log("WARNING: kubelet_pod_start_sli_duration_seconds not found on node")
	return nil
}

// parsePrometheusMetrics parses Prometheus exposition format text into MetricFamily map.
func parsePrometheusMetrics(data []byte) (map[string]*dto.MetricFamily, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	return parser.TextToMetricFamilies(strings.NewReader(string(data)))
}

// computeSLIDelta computes the difference between two snapshots of the kubelet
// SLI histogram, giving us just the pods started during the benchmark run.
func computeSLIDelta(t *testing.T, pre, post *dto.MetricFamily) *kubeletSLIResult {
	t.Helper()
	if pre == nil || post == nil {
		return nil
	}

	// Sum across all label combinations (there may be multiple series).
	preHist := aggregateHistograms(pre)
	postHist := aggregateHistograms(post)
	if preHist == nil || postHist == nil {
		return nil
	}

	deltaCount := postHist.GetSampleCount() - preHist.GetSampleCount()
	deltaSum := postHist.GetSampleSum() - preHist.GetSampleSum()

	if deltaCount == 0 {
		t.Log("Kubelet SLI: no new samples during this run")
		return &kubeletSLIResult{}
	}

	result := &kubeletSLIResult{
		SampleCount: deltaCount,
		SampleSum:   deltaSum,
		MeanSeconds: deltaSum / float64(deltaCount),
	}

	// Compute delta buckets (skip +Inf which can't be JSON-marshaled).
	preBuckets := bucketMap(preHist)
	for _, b := range postHist.GetBucket() {
		le := b.GetUpperBound()
		if math.IsInf(le, 0) {
			continue
		}
		preCount := preBuckets[le]
		result.Buckets = append(result.Buckets, histogramBucket{
			UpperBound:      le,
			CumulativeCount: b.GetCumulativeCount() - preCount,
		})
	}

	t.Logf("Kubelet SLI delta: count=%d sum=%.3fs mean=%.3fs",
		deltaCount, deltaSum, result.MeanSeconds)

	return result
}

// aggregateHistograms sums all histogram metric samples in a MetricFamily into one.
func aggregateHistograms(fam *dto.MetricFamily) *dto.Histogram {
	metrics := fam.GetMetric()
	if len(metrics) == 0 {
		return nil
	}
	if len(metrics) == 1 {
		return metrics[0].GetHistogram()
	}

	// Multiple label sets — aggregate by summing counts and sums.
	var totalCount uint64
	var totalSum float64
	bucketCounts := make(map[float64]uint64)

	for _, m := range metrics {
		h := m.GetHistogram()
		if h == nil {
			continue
		}
		totalCount += h.GetSampleCount()
		totalSum += h.GetSampleSum()
		for _, b := range h.GetBucket() {
			bucketCounts[b.GetUpperBound()] += b.GetCumulativeCount()
		}
	}

	agg := &dto.Histogram{
		SampleCount: &totalCount,
		SampleSum:   &totalSum,
	}
	for le, count := range bucketCounts {
		leCopy := le
		countCopy := count
		agg.Bucket = append(agg.Bucket, &dto.Bucket{
			UpperBound:      &leCopy,
			CumulativeCount: &countCopy,
		})
	}
	return agg
}

func bucketMap(h *dto.Histogram) map[float64]uint64 {
	m := make(map[float64]uint64, len(h.GetBucket()))
	for _, b := range h.GetBucket() {
		m[b.GetUpperBound()] = b.GetCumulativeCount()
	}
	return m
}

// ──────────────────────────────────────────────────────────────────
// ConfigMap switching
// ──────────────────────────────────────────────────────────────────

func switchBackend(ctx context.Context, t *testing.T, clientset *k8s.Clientset, backend, node string) {
	t.Helper()
	t.Logf("Switching CNS store backend to: %s", backend)

	// Update the ConfigMap with the new backend.
	cm, err := clientset.CoreV1().ConfigMaps(cnsNamespace).Get(ctx, cnsConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get ConfigMap %s: %v", cnsConfigMap, err)
	}

	raw := cm.Data["cns_config.json"]
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("Failed to parse cns_config.json: %v", err)
	}

	parsed["StoreBackend"] = backend
	updated, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("Failed to marshal cns_config.json: %v", err)
	}
	cm.Data["cns_config.json"] = string(updated)

	_, err = clientset.CoreV1().ConfigMaps(cnsNamespace).Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Failed to update ConfigMap: %v", err)
	}

	// Wipe old store files from the node to prevent state leakage between backends.
	cleanStoreState(ctx, t, clientset, node)

	// Rolling restart of CNS DaemonSet (picks up new ConfigMap + clean state).
	restartDaemonSet(ctx, t, clientset, cnsNamespace, "azure-cns")

	t.Log("Waiting 30s for CNS to stabilize...")
	time.Sleep(30 * time.Second)
}

// cleanStoreState removes CNS store files from the target node so each backend
// starts with a clean slate. This prevents stale endpoint state from prior
// backends or prior runs from skewing the benchmark.
func cleanStoreState(ctx context.Context, t *testing.T, clientset *k8s.Clientset, node string) {
	t.Helper()
	t.Log("Cleaning CNS store state on node...")

	podName := "storebench-cleanup"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: cnsNamespace,
		},
		Spec: corev1.PodSpec{
			NodeSelector:  map[string]string{"kubernetes.io/hostname": node},
			Tolerations:   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			RestartPolicy: corev1.RestartPolicyNever,
			HostNetwork:   true,
			Containers: []corev1.Container{
				{
					Name:    "cleanup",
					Image:   "busybox:1.36",
					Command: []string{"/bin/sh", "-c", strings.Join([]string{
						// Remove all store files (json, bbolt, sqlite) from both state dirs.
						"rm -f /host/var/lib/azure-network/azure-cns.*",
						"rm -f /host/var/lib/azure-network/*.db",
						"rm -f /host/var/lib/azure-network/*.sqlite",
						"rm -f /host/var/run/azure-cns/azure-endpoints.*",
						"rm -f /host/var/run/azure-cns/*.db",
						"rm -f /host/var/run/azure-cns/*.sqlite",
						"echo done",
					}, " && ")},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "host-state", MountPath: "/host/var/lib/azure-network"},
						{Name: "host-endpoints", MountPath: "/host/var/run/azure-cns"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{Name: "host-state", VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/azure-network"},
				}},
				{Name: "host-endpoints", VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: "/var/run/azure-cns"},
				}},
			},
			TerminationGracePeriodSeconds: int64ptr(0),
		},
	}

	// Delete if leftover from a previous run.
	_ = clientset.CoreV1().Pods(cnsNamespace).Delete(ctx, podName, metav1.DeleteOptions{})
	time.Sleep(5 * time.Second)

	_, err := clientset.CoreV1().Pods(cnsNamespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Logf("WARNING: cleanup pod creation failed: %v", err)
		return
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		p, err := clientset.CoreV1().Pods(cnsNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			break
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			if p.Status.Phase == corev1.PodFailed {
				t.Log("WARNING: cleanup pod failed")
			} else {
				t.Log("Store state cleaned")
			}
			break
		}
		time.Sleep(3 * time.Second)
	}
	_ = clientset.CoreV1().Pods(cnsNamespace).Delete(ctx, podName, metav1.DeleteOptions{})
	time.Sleep(3 * time.Second)
}

func restartDaemonSet(ctx context.Context, t *testing.T, clientset *k8s.Clientset, ns, name string) {
	t.Helper()
	ds, err := clientset.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get DaemonSet %s: %v", name, err)
	}
	if ds.Spec.Template.Annotations == nil {
		ds.Spec.Template.Annotations = make(map[string]string)
	}
	ds.Spec.Template.Annotations["storebench/restartedAt"] = time.Now().Format(time.RFC3339)
	_, err = clientset.AppsV1().DaemonSets(ns).Update(ctx, ds, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Failed to update DaemonSet: %v", err)
	}

	// Wait for rollout to complete.
	t.Logf("Waiting for DaemonSet %s rollout...", name)
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		ds, err = clientset.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Logf("  rollout check error: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		if ds.Status.UpdatedNumberScheduled == ds.Status.DesiredNumberScheduled &&
			ds.Status.NumberReady == ds.Status.DesiredNumberScheduled &&
			ds.Status.ObservedGeneration >= ds.Generation {
			t.Log("  DaemonSet rollout complete")
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Log("WARNING: DaemonSet rollout did not complete within 3 minutes")
}

// ──────────────────────────────────────────────────────────────────
// Deployment helpers
// ──────────────────────────────────────────────────────────────────

func makePauseDeployment(node string, replicas int32) *appsv1.Deployment {
	labels := map[string]string{"app": "storebench"}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployName,
			Namespace: testNamespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{
						"kubernetes.io/hostname": node,
					},
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists},
					},
					Containers: []corev1.Container{
						{
							Name:  "pause",
							Image: "registry.k8s.io/pause:3.10",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("10m"),
									corev1.ResourceMemory: resource.MustParse("16Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("10m"),
									corev1.ResourceMemory: resource.MustParse("16Mi"),
								},
							},
						},
					},
					TerminationGracePeriodSeconds: int64ptr(0),
				},
			},
		},
	}
}

func prePullPause(ctx context.Context, t *testing.T, clientset *k8s.Clientset, node string) {
	t.Helper()
	t.Logf("Pre-pulling pause image on %s...", node)

	podName := "prepull-pause"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: cnsNamespace,
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"kubernetes.io/hostname": node},
			Tolerations:  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{
				{
					Name:    "pull",
					Image:   "registry.k8s.io/pause:3.10",
					Command: []string{"sh", "-c", "exit 0"},
				},
			},
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: int64ptr(0),
		},
	}

	// Delete if it already exists.
	_ = clientset.CoreV1().Pods(cnsNamespace).Delete(ctx, podName, metav1.DeleteOptions{})
	time.Sleep(5 * time.Second)

	_, err := clientset.CoreV1().Pods(cnsNamespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Logf("WARNING: pre-pull pod creation failed: %v (may already be cached)", err)
		return
	}

	// Wait for it to complete.
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		p, err := clientset.CoreV1().Pods(cnsNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			break
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			break
		}
		time.Sleep(3 * time.Second)
	}
	_ = clientset.CoreV1().Pods(cnsNamespace).Delete(ctx, podName, metav1.DeleteOptions{})
	time.Sleep(5 * time.Second)
	t.Log("Pre-pull complete")
}

// ──────────────────────────────────────────────────────────────────
// Namespace helpers
// ──────────────────────────────────────────────────────────────────

func ensureNamespace(ctx context.Context, t *testing.T, clientset *k8s.Clientset) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
	_, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		// Might already exist, that's fine.
		t.Logf("Namespace create: %v", err)
	}
}

func cleanNamespace(ctx context.Context, t *testing.T, clientset *k8s.Clientset) {
	t.Helper()
	err := clientset.CoreV1().Namespaces().Delete(ctx, testNamespace, metav1.DeleteOptions{})
	if err != nil {
		return // not found is fine
	}
	// Wait for namespace deletion, force-finalize if stuck.
	deadline := time.Now().Add(90 * time.Second)
	forceFinalized := false
	for time.Now().Before(deadline) {
		ns, err := clientset.CoreV1().Namespaces().Get(ctx, testNamespace, metav1.GetOptions{})
		if err != nil {
			return // deleted
		}
		// If the namespace has been Terminating for >30s and still has finalizers,
		// force-remove them (common when metrics-server API is stale).
		if !forceFinalized && time.Since(ns.DeletionTimestamp.Time) > 30*time.Second {
			t.Log("Force-removing namespace finalizers...")
			ns.Spec.Finalizers = nil
			_, err := clientset.CoreV1().Namespaces().Finalize(ctx, ns, metav1.UpdateOptions{})
			if err != nil {
				t.Logf("  finalize failed: %v", err)
			}
			forceFinalized = true
		}
		time.Sleep(3 * time.Second)
	}
	t.Log("WARNING: namespace deletion timed out")
}

// ──────────────────────────────────────────────────────────────────
// Node resolution
// ──────────────────────────────────────────────────────────────────

func resolveNode(ctx context.Context, t *testing.T, clientset *k8s.Clientset, preferred string) string {
	t.Helper()
	if preferred != "" {
		return preferred
	}

	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list nodes: %v", err)
	}

	// Prefer a node labeled benchmark=true.
	for i := range nodes.Items {
		if nodes.Items[i].Labels["benchmark"] == "true" {
			return nodes.Items[i].Name
		}
	}

	// Fall back to first Ready node.
	for i := range nodes.Items {
		for _, cond := range nodes.Items[i].Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				return nodes.Items[i].Name
			}
		}
	}

	t.Fatal("No Ready node found")
	return ""
}

func nodeVMSize(ctx context.Context, clientset *k8s.Clientset, nodeName string) string {
	node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return "unknown"
	}
	if sku, ok := node.Labels["node.kubernetes.io/instance-type"]; ok {
		return sku
	}
	return "unknown"
}

// ──────────────────────────────────────────────────────────────────
// Persistence & summary
// ──────────────────────────────────────────────────────────────────

func saveResult(t *testing.T, dir, tag string, r benchResult) {
	t.Helper()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Logf("WARNING: failed to marshal result: %v", err)
		return
	}
	writeFile(t, filepath.Join(dir, "result-"+tag+".json"), string(data))
}

func generateSummary(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Logf("WARNING: failed to read results dir: %v", err)
		return
	}

	type key struct{ Backend, Scale, SKU string }
	wallClocks := make(map[key][]int64)
	p50s := make(map[key][]float64)
	p95s := make(map[key][]float64)
	p99s := make(map[key][]float64)
	sliMeans := make(map[key][]float64)
	sliCounts := make(map[key][]uint64)

	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "result-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var r benchResult
		if err := json.Unmarshal(data, &r); err != nil {
			continue
		}
		k := key{r.Backend, strconv.Itoa(r.Scale), r.SKU}
		wallClocks[k] = append(wallClocks[k], r.WallClockMS)
		p50s[k] = append(p50s[k], r.P50)
		p95s[k] = append(p95s[k], r.P95)
		p99s[k] = append(p99s[k], r.P99)
		if r.KubeletSLI != nil && r.KubeletSLI.SampleCount > 0 {
			sliMeans[k] = append(sliMeans[k], r.KubeletSLI.MeanSeconds)
			sliCounts[k] = append(sliCounts[k], r.KubeletSLI.SampleCount)
		}
	}

	var sb strings.Builder
	sb.WriteString("# CNS Store Backend Benchmark Results\n\n")
	sb.WriteString("## Pod Startup Latency (from pod timestamps)\n\n")
	sb.WriteString("| Backend | Scale | SKU | Wall-Clock Mean (ms) | P50 (s) | P95 (s) | P99 (s) |\n")
	sb.WriteString("|---------|-------|-----|---------------------:|--------:|--------:|--------:|\n")

	// Sort keys for stable output.
	keys := make([]key, 0, len(wallClocks))
	for k := range wallClocks {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Backend != keys[j].Backend {
			return keys[i].Backend < keys[j].Backend
		}
		if keys[i].Scale != keys[j].Scale {
			return keys[i].Scale < keys[j].Scale
		}
		return keys[i].SKU < keys[j].SKU
	})

	for _, k := range keys {
		sb.WriteString(fmt.Sprintf("| %s | %s | %s | %.0f | %.2f | %.2f | %.2f |\n",
			k.Backend, k.Scale, k.SKU,
			meanInt64(wallClocks[k]),
			meanFloat64(p50s[k]),
			meanFloat64(p95s[k]),
			meanFloat64(p99s[k]),
		))
	}

	// Kubelet SLI table (only if we collected data).
	hasSLI := false
	for _, k := range keys {
		if len(sliMeans[k]) > 0 {
			hasSLI = true
			break
		}
	}
	if hasSLI {
		sb.WriteString("\n## Kubelet Pod Start SLI (from kubelet_pod_start_sli_duration_seconds)\n\n")
		sb.WriteString("| Backend | Scale | SKU | SLI Mean (s) | SLI Pod Count |\n")
		sb.WriteString("|---------|-------|-----|-------------:|--------------:|\n")
		for _, k := range keys {
			if len(sliMeans[k]) == 0 {
				sb.WriteString(fmt.Sprintf("| %s | %s | %s | n/a | n/a |\n",
					k.Backend, k.Scale, k.SKU))
			} else {
				sb.WriteString(fmt.Sprintf("| %s | %s | %s | %.3f | %.0f |\n",
					k.Backend, k.Scale, k.SKU,
					meanFloat64(sliMeans[k]),
					meanUint64(sliCounts[k]),
				))
			}
		}
	}

	sb.WriteString("\n---\n*Generated by storebench_test.go*\n")

	writeFile(t, filepath.Join(dir, "SUMMARY.md"), sb.String())
	t.Logf("Summary written to %s/SUMMARY.md", dir)
}

// ──────────────────────────────────────────────────────────────────
// Utility functions
// ──────────────────────────────────────────────────────────────────

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := p * float64(len(sorted)-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))
	if lower == upper || upper >= len(sorted) {
		return sorted[lower]
	}
	frac := idx - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}

func meanFloat64(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func meanInt64(vals []int64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := int64(0)
	for _, v := range vals {
		sum += v
	}
	return float64(sum) / float64(len(vals))
}

func meanUint64(vals []uint64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := uint64(0)
	for _, v := range vals {
		sum += v
	}
	return float64(sum) / float64(len(vals))
}

func parseIntList(t *testing.T, s string) []int {
	t.Helper()
	var out []int
	for _, f := range strings.Fields(s) {
		n, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("Invalid integer in list %q: %v", s, err)
		}
		out = append(out, n)
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Logf("WARNING: failed to write %s: %v", path, err)
		return
	}
	defer f.Close()
	f.WriteString(content) //nolint:errcheck
}

func appendCSV(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Logf("WARNING: failed to append to %s: %v", path, err)
		return
	}
	defer f.Close()
	f.WriteString(line) //nolint:errcheck
}

func int64ptr(v int64) *int64 { return &v }

// loadEnvironment reads struct fields from env vars with defaults (mirrors test/integration/load pattern).
func loadEnvironment(obj interface{}) {
	val := reflect.ValueOf(obj).Elem()
	typ := reflect.TypeOf(obj).Elem()

	for i := 0; i < val.NumField(); i++ {
		field := val.Field(i)
		tag := typ.Field(i).Tag

		env := tag.Get("env")
		def := tag.Get("default")
		v := os.Getenv(env)
		if v == "" {
			v = def
		}

		switch field.Kind() {
		case reflect.String:
			field.SetString(v)
		case reflect.Int:
			n, err := strconv.Atoi(v)
			if err != nil {
				log.Fatalf("env %s must be int: %v", env, err)
			}
			field.SetInt(int64(n))
		case reflect.Bool:
			b, err := strconv.ParseBool(v)
			if err != nil {
				log.Fatalf("env %s must be bool: %v", env, err)
			}
			field.SetBool(b)
		}
	}
}
