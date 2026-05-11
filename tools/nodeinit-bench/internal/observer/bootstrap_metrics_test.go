package observer

import (
	"testing"
	"time"

	"github.com/Azure/azure-container-networking/tools/nodeinit-bench/internal/spans"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyBootstrapMetricsBootStateAndMode(t *testing.T) {
	nr := &spans.NodeRun{
		Spans: map[spans.SpanID]spans.Span{},
		Metrics: map[string]float64{
			`cns_boot_state{state="fresh"}`:                      1,
			`cns_boot_state{state="reboot"}`:                     0,
			`cns_boot_state{state="restart"}`:                    0,
			`cns_boot_state{state="unknown"}`:                    0,
			`cns_mode_info{channel_mode="CRD",dual_stack="true",ipam_v2="false",manage_endpoint_state="false",swift_v2="false"}`: 1,
			`cns_start_time_seconds`:                             1700000000,
			`cns_state_restored_seconds`:                         1700000005,
			`cns_first_nnc_received_seconds`:                     1700000010,
			`cns_initial_ipam_reconciled_seconds`:                1700000012,
			`cns_first_nc_programmed_seconds`:                    1700000015,
			`cns_http_listener_ready_seconds`:                    1700000003,
			`cns_conflist_written_seconds`:                       1700000020,
			`cns_ready_to_assign_seconds`:                        1700000025,
		},
	}

	set := func(id spans.SpanID, start, end time.Time, source string, inferred bool) {
		sp := spans.Span{ID: id, Start: start.UTC(), End: end.UTC(), Source: source, Inferred: inferred}
		if start.IsZero() || end.IsZero() {
			sp.Missing = true
		}
		nr.Spans[id] = sp
	}

	applyBootstrapMetrics(nr, set)

	assert.Equal(t, "fresh", nr.BootState)
	require.NotNil(t, nr.Mode)
	assert.Equal(t, "CRD", nr.Mode["channel_mode"])
	assert.Equal(t, "true", nr.Mode["dual_stack"])
	assert.Equal(t, "false", nr.Mode["ipam_v2"])

	// Each boundary span should have duration = (event - start).
	cases := []struct {
		id         spans.SpanID
		wantSecs   int
		wantSource string
	}{
		{spans.SpanCNSStateRestored, 5, "cns-metric:cns_state_restored_seconds"},
		{spans.SpanCNSFirstNNCReceived, 10, "cns-metric:cns_first_nnc_received_seconds"},
		{spans.SpanCNSInitialIPAMReconciled, 12, "cns-metric:cns_initial_ipam_reconciled_seconds"},
		{spans.SpanCNSFirstNCProgrammed, 15, "cns-metric:cns_first_nc_programmed_seconds"},
		{spans.SpanCNSListenerReady, 3, "cns-metric:cns_http_listener_ready_seconds"},
		{spans.SpanCNSConflistWrite, 20, "cns-metric:cns_conflist_written_seconds"},
		{spans.SpanCNSReadyToAssign, 25, "cns-metric:cns_ready_to_assign_seconds"},
	}
	for _, c := range cases {
		sp, ok := nr.Spans[c.id]
		require.True(t, ok, "%s missing", c.id)
		assert.False(t, sp.Missing, "%s should not be Missing", c.id)
		assert.InDelta(t, float64(c.wantSecs), sp.Duration().Seconds(), 0.001, "%s duration", c.id)
		assert.Equal(t, c.wantSource, sp.Source, "%s source", c.id)
	}
}

func TestApplyBootstrapMetricsSkipsZeroGauges(t *testing.T) {
	nr := &spans.NodeRun{
		Spans: map[spans.SpanID]spans.Span{},
		Metrics: map[string]float64{
			`cns_start_time_seconds`:             1700000000,
			`cns_state_restored_seconds`:         1700000005,
			`cns_ready_to_assign_seconds`:        0, // event hasn't fired
			`cns_boot_state{state="restart"}`:    1,
		},
	}
	set := func(id spans.SpanID, start, end time.Time, source string, inferred bool) {
		nr.Spans[id] = spans.Span{ID: id, Start: start.UTC(), End: end.UTC(), Source: source, Inferred: inferred}
	}
	applyBootstrapMetrics(nr, set)

	// state_restored should be present
	_, ok := nr.Spans[spans.SpanCNSStateRestored]
	assert.True(t, ok)
	// ready_to_assign should NOT be present (gauge was 0)
	_, ok = nr.Spans[spans.SpanCNSReadyToAssign]
	assert.False(t, ok, "zero-valued gauge should not produce a span")

	assert.Equal(t, "restart", nr.BootState)
}

func TestApplyBootstrapMetricsPreservesLogSpans(t *testing.T) {
	// log-derived listener-ready span; metric should overwrite it
	// (per the design: metric is sub-second precise)
	logT := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	nr := &spans.NodeRun{
		Spans: map[spans.SpanID]spans.Span{
			spans.SpanCNSListenerReady: {ID: spans.SpanCNSListenerReady, Start: logT, End: logT.Add(time.Second), Source: "cns-log"},
			// SyncHostNCVersion is NOT replaced by metrics — should survive.
			spans.SpanCNSSyncHostNCVersion: {ID: spans.SpanCNSSyncHostNCVersion, Start: logT, End: logT.Add(2 * time.Second), Source: "cns-metric:sync_host_nc_version_latency_seconds_sum"},
		},
		Metrics: map[string]float64{
			`cns_start_time_seconds`:             1700000000,
			`cns_http_listener_ready_seconds`:    1700000003,
		},
	}
	set := func(id spans.SpanID, start, end time.Time, source string, inferred bool) {
		nr.Spans[id] = spans.Span{ID: id, Start: start.UTC(), End: end.UTC(), Source: source, Inferred: inferred}
	}
	applyBootstrapMetrics(nr, set)

	// Listener span should be overwritten with metric source.
	listener := nr.Spans[spans.SpanCNSListenerReady]
	assert.Equal(t, "cns-metric:cns_http_listener_ready_seconds", listener.Source)

	// SyncHostNCVersion span should remain untouched.
	sync := nr.Spans[spans.SpanCNSSyncHostNCVersion]
	assert.Equal(t, "cns-metric:sync_host_nc_version_latency_seconds_sum", sync.Source)
	assert.Equal(t, 2*time.Second, sync.Duration())
}

func TestApplyBootstrapMetricsNoOpWhenMetricsEmpty(t *testing.T) {
	nr := &spans.NodeRun{Spans: map[spans.SpanID]spans.Span{}}
	set := func(id spans.SpanID, start, end time.Time, source string, inferred bool) {
		nr.Spans[id] = spans.Span{ID: id, Start: start.UTC(), End: end.UTC(), Source: source, Inferred: inferred}
	}
	applyBootstrapMetrics(nr, set)
	assert.Empty(t, nr.Spans)
	assert.Empty(t, nr.BootState)
	assert.Empty(t, nr.Mode)
}

func TestParseLabelSuffix(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]string
	}{
		{`{state="fresh"}`, map[string]string{"state": "fresh"}},
		{`{a="x",b="y"}`, map[string]string{"a": "x", "b": "y"}},
		{``, map[string]string{}},
		{`{}`, map[string]string{}},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, parseLabelSuffix(c.in), "input=%q", c.in)
	}
}
