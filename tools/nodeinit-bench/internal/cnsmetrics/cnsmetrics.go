// Package cnsmetrics scrapes the CNS Prometheus metrics endpoint on a single
// pod via an API-server port-forward, so the caller doesn't need direct
// network access to the node. It extracts a small, curated set of histogram
// and counter series useful for nodeinit-bench Gantt annotations.
package cnsmetrics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// cnsMetricsPort is the container port where CNS exposes /metrics when started
// with -metrics-listen-address.
const cnsMetricsPort = 10092

// explicitMetrics are series we always try to record, regardless of prefix.
// Names must match the actual Prometheus metric names exposed by CNS at /metrics.
// See cns/restserver/metrics.go and cns/metric/pool.go.
var explicitMetrics = []string{
	"sync_host_nc_version_latency_seconds",
	"sync_host_nc_version_total",
	"has_networkcontainer",
	"http_request_latency_seconds",
	"ip_assignment_latency_seconds",
	"ipconfigstatus_state_transition_seconds",
	"ip_pool_inc_latency_seconds",
	"ip_pool_dec_latency_seconds",
}

// Scrape opens a port-forward to pod ns/pod on cnsMetricsPort, fetches
// /metrics, parses the Prometheus text format, and returns a flat map of
// series name -> value. Histogram/summary series are flattened as
// `<name>_sum`, `<name>_count`, and `<name>_bucket{le="..."}`.
//
// It is best-effort: if CNS isn't exposing metrics, the call returns
// (nil, err) and the caller is expected to log and continue.
func Scrape(ctx context.Context, cfg *rest.Config, ns, pod string) (map[string]float64, error) {
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	req := kc.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(ns).
		Name(pod).
		SubResource("portforward")

	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("spdy roundtripper: %w", err)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", req.URL())

	stopCh := make(chan struct{}, 1)
	readyCh := make(chan struct{})
	defer close(stopCh)

	pfCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ports := []string{fmt.Sprintf("0:%d", cnsMetricsPort)}
	pf, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, ports, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return nil, fmt.Errorf("portforward: %w", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- pf.ForwardPorts() }()

	select {
	case <-readyCh:
	case err := <-errCh:
		return nil, fmt.Errorf("portforward start: %w", err)
	case <-pfCtx.Done():
		return nil, fmt.Errorf("portforward ready: %w", pfCtx.Err())
	}

	fwdPorts, err := pf.GetPorts()
	if err != nil || len(fwdPorts) == 0 {
		return nil, fmt.Errorf("resolve local port: %w", err)
	}
	localPort := fwdPorts[0].Local

	u := url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", localPort), Path: "/metrics"}
	httpReq, err := http.NewRequestWithContext(pfCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("GET /metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /metrics: status %d", resp.StatusCode)
	}

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse metrics: %w", err)
	}

	out := make(map[string]float64)
	explicit := make(map[string]struct{}, len(explicitMetrics))
	for _, m := range explicitMetrics {
		explicit[m] = struct{}{}
	}

	for name, mf := range families {
		_, isExplicit := explicit[name]
		if !isExplicit && !hasCNSPrefix(name) {
			continue
		}
		for _, m := range mf.Metric {
			flatten(out, name, mf.GetType(), m)
		}
	}
	return out, nil
}

func hasCNSPrefix(name string) bool {
	return strings.HasPrefix(name, "cns_") || strings.HasPrefix(name, "cx_")
}

func flatten(out map[string]float64, name string, t dto.MetricType, m *dto.Metric) {
	suffix := labelSuffix(m)
	switch t {
	case dto.MetricType_COUNTER:
		if m.Counter != nil {
			out[name+suffix] = m.Counter.GetValue()
		}
	case dto.MetricType_GAUGE:
		if m.Gauge != nil {
			out[name+suffix] = m.Gauge.GetValue()
		}
	case dto.MetricType_HISTOGRAM:
		if m.Histogram != nil {
			out[name+"_sum"+suffix] = m.Histogram.GetSampleSum()
			out[name+"_count"+suffix] = float64(m.Histogram.GetSampleCount())
			for _, b := range m.Histogram.Bucket {
				bsuf := mergeLabelSuffix(suffix, fmt.Sprintf("le=\"%g\"", b.GetUpperBound()))
				out[name+"_bucket"+bsuf] = float64(b.GetCumulativeCount())
			}
		}
	case dto.MetricType_SUMMARY:
		if m.Summary != nil {
			out[name+"_sum"+suffix] = m.Summary.GetSampleSum()
			out[name+"_count"+suffix] = float64(m.Summary.GetSampleCount())
		}
	default:
	}
}

// labelSuffix renders a Metric's labels as a deterministic `{k="v",...}` suffix,
// or the empty string if it has no labels.
func labelSuffix(m *dto.Metric) string {
	if len(m.Label) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(m.Label))
	for _, lp := range m.Label {
		pairs = append(pairs, fmt.Sprintf("%s=%q", lp.GetName(), lp.GetValue()))
	}
	sort.Strings(pairs)
	return "{" + strings.Join(pairs, ",") + "}"
}

// mergeLabelSuffix injects an extra `k="v"` pair into an existing `{...}` suffix
// (or creates a fresh suffix if none). Used to attach le="..." to histogram
// buckets that already carry dimensional labels.
func mergeLabelSuffix(existing, extra string) string {
	if existing == "" {
		return "{" + extra + "}"
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(existing, "{"), "}")
	return "{" + inner + "," + extra + "}"
}
