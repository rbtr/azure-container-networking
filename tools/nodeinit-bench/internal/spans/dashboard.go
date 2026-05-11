package spans

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// writeDashboard emits a single self-contained HTML dashboard with multiple
// linked views over the run set:
//
//  1. summary header (n runs, p50/p95/max node-ready, dominant phase)
//  2. cross-run stacked bar (one bar per run, segments = phases)
//  3. per-phase distribution box plot (variance across runs)
//  4. interactive Gantt with run selector (single-run timeline)
//  5. per-run critical path waterfall (gating chain to Node Ready)
//  6. CNS internal metrics histogram strip
//
// All data is embedded as JSON. Plotly.js is loaded from the CDN; otherwise
// the file is dependency-free and can be opened locally.
func writeDashboard(path string, runs []NodeRun) error {
	type spanPoint struct {
		ID       string  `json:"id"`
		Start    string  `json:"start"`
		End      string  `json:"end"`
		StartRel float64 `json:"startRel"`
		EndRel   float64 `json:"endRel"`
		Dur      float64 `json:"dur"`
		Inferred bool    `json:"inferred"`
		Missing  bool    `json:"missing"`
	}
	type runRow struct {
		RunID   int                `json:"runId"`
		Node    string             `json:"node"`
		Pod     string             `json:"pod"`
		T0      string             `json:"t0"`
		Spans   []spanPoint        `json:"spans"`
		Metrics map[string]float64 `json:"metrics,omitempty"`
	}

	out := struct {
		GeneratedAt string         `json:"generatedAt"`
		PhaseOrder  []string       `json:"phaseOrder"`
		Runs        []runRow       `json:"runs"`
		MetricKeys  []string       `json:"metricKeys"`
		Aggregates  map[string]any `json:"aggregates"`
	}{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		PhaseOrder:  spanIDsAsStrings(OrderedSpans),
	}

	allMetricKeys := map[string]struct{}{}
	for _, r := range runs {
		row := runRow{
			RunID: r.RunID,
			Node:  r.Node,
			Pod:   r.PodName,
			T0:    r.T0.UTC().Format(time.RFC3339Nano),
		}
		for _, id := range OrderedSpans {
			sp := r.Spans[id]
			pt := spanPoint{ID: string(id), Inferred: sp.Inferred, Missing: sp.Missing}
			if !sp.Missing {
				pt.Start = sp.Start.UTC().Format(time.RFC3339Nano)
				pt.End = sp.End.UTC().Format(time.RFC3339Nano)
				pt.Dur = sp.Duration().Seconds()
				if !r.T0.IsZero() {
					pt.StartRel = sp.Start.Sub(r.T0).Seconds()
					pt.EndRel = sp.End.Sub(r.T0).Seconds()
				}
			}
			row.Spans = append(row.Spans, pt)
		}
		if len(r.Metrics) > 0 {
			row.Metrics = r.Metrics
			for k := range r.Metrics {
				allMetricKeys[k] = struct{}{}
			}
		}
		out.Runs = append(out.Runs, row)
	}
	sort.Slice(out.Runs, func(i, j int) bool { return out.Runs[i].RunID < out.Runs[j].RunID })

	out.MetricKeys = make([]string, 0, len(allMetricKeys))
	for k := range allMetricKeys {
		out.MetricKeys = append(out.MetricKeys, k)
	}
	sort.Strings(out.MetricKeys)

	out.Aggregates = aggregateForDashboard(runs)

	dataJSON, err := jsonMarshal(out)
	if err != nil {
		return fmt.Errorf("marshal dashboard data: %w", err)
	}

	// Write to a sibling file so the Go template doesn't have to escape
	// every brace and percent in the HTML/JS body.
	html := strings.ReplaceAll(dashboardTemplate, "/*__DATA__*/", dataJSON)
	return os.WriteFile(path, []byte(html), 0o644)
}

func spanIDsAsStrings(in []SpanID) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = string(s)
	}
	return out
}

// aggregateForDashboard produces precomputed summary stats (per-phase
// percentiles, dominant phase per run, p50/p95/max node-ready) so the
// dashboard JS can render the headline numbers without recomputing.
func aggregateForDashboard(runs []NodeRun) map[string]any {
	perPhase := map[string][]float64{}
	for _, r := range runs {
		for _, id := range OrderedSpans {
			sp := r.Spans[id]
			if sp.Missing {
				continue
			}
			perPhase[string(id)] = append(perPhase[string(id)], sp.Duration().Seconds())
		}
	}
	type stat struct {
		Count int     `json:"count"`
		Min   float64 `json:"min"`
		P50   float64 `json:"p50"`
		P95   float64 `json:"p95"`
		Max   float64 `json:"max"`
		Mean  float64 `json:"mean"`
	}
	phaseStats := map[string]stat{}
	for k, v := range perPhase {
		sort.Float64s(v)
		s := stat{Count: len(v), Min: v[0], Max: v[len(v)-1], P50: pct(v, 0.5), P95: pct(v, 0.95)}
		var sum float64
		for _, x := range v {
			sum += x
		}
		s.Mean = sum / float64(len(v))
		phaseStats[k] = s
	}
	// Headline Node Ready stats.
	nodeReady := perPhase["node-ready"]
	headline := stat{}
	if len(nodeReady) > 0 {
		sort.Float64s(nodeReady)
		headline.Count = len(nodeReady)
		headline.Min = nodeReady[0]
		headline.Max = nodeReady[len(nodeReady)-1]
		headline.P50 = pct(nodeReady, 0.5)
		headline.P95 = pct(nodeReady, 0.95)
		var sum float64
		for _, x := range nodeReady {
			sum += x
		}
		headline.Mean = sum / float64(len(nodeReady))
	}
	return map[string]any{
		"perPhase":  phaseStats,
		"nodeReady": headline,
	}
}

// dashboardTemplate is a self-contained HTML/JS dashboard. It loads
// Plotly.js from the CDN. Run data is interpolated at /*__DATA__*/.
const dashboardTemplate = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>nodeinit-bench dashboard</title>
  <script src="https://cdn.plot.ly/plotly-2.35.2.min.js"></script>
  <style>
    :root {
      --bg: #0f1115;
      --panel: #181b22;
      --panel-2: #20242e;
      --muted: #8a93a4;
      --fg: #e6eaf2;
      --accent: #4ea1ff;
      --warn: #f5a623;
      --crit: #ff5d6c;
      --good: #46d18c;
      --border: #2a2f3a;
    }
    * { box-sizing: border-box; }
    body { background: var(--bg); color: var(--fg); font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif; margin: 0; padding: 0; }
    header { padding: 20px 28px 16px; border-bottom: 1px solid var(--border); }
    header h1 { font-size: 18px; margin: 0 0 4px; font-weight: 600; letter-spacing: -0.2px; }
    header .sub { color: var(--muted); font-size: 13px; }
    .stats { display: grid; grid-template-columns: repeat(5, 1fr); gap: 12px; padding: 16px 28px; border-bottom: 1px solid var(--border); }
    .stat { background: var(--panel); border: 1px solid var(--border); border-radius: 8px; padding: 12px 14px; }
    .stat .label { font-size: 11px; color: var(--muted); text-transform: uppercase; letter-spacing: 0.5px; }
    .stat .value { font-size: 22px; font-weight: 600; margin-top: 4px; font-variant-numeric: tabular-nums; }
    .stat .unit { font-size: 12px; color: var(--muted); margin-left: 4px; font-weight: 400; }
    main { padding: 16px 28px 40px; display: grid; grid-template-columns: 1fr; gap: 16px; max-width: 1600px; }
    .panel { background: var(--panel); border: 1px solid var(--border); border-radius: 10px; padding: 16px 20px 20px; }
    .panel h2 { font-size: 14px; margin: 0 0 4px; font-weight: 600; letter-spacing: -0.1px; }
    .panel .desc { color: var(--muted); font-size: 12px; margin-bottom: 12px; }
    .panel .ctrl { display: flex; gap: 12px; align-items: center; margin-bottom: 12px; }
    .panel .ctrl label { font-size: 12px; color: var(--muted); }
    .panel .ctrl select { background: var(--panel-2); color: var(--fg); border: 1px solid var(--border); border-radius: 6px; padding: 4px 8px; font-size: 13px; }
    .legend-note { color: var(--muted); font-size: 11px; margin-top: 8px; line-height: 1.5; }
    .chart { width: 100%; }
    .grid-2 { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
    @media (max-width: 1024px) { .grid-2 { grid-template-columns: 1fr; } }
    .legend-pill { display: inline-block; padding: 1px 6px; border-radius: 4px; font-size: 10px; font-weight: 600; vertical-align: 1px; }
    .legend-pill.crit { background: rgba(255,93,108,0.15); color: var(--crit); border: 1px solid rgba(255,93,108,0.3); }
    .legend-pill.warn { background: rgba(245,166,35,0.15); color: var(--warn); border: 1px solid rgba(245,166,35,0.3); }
    .legend-pill.good { background: rgba(70,209,140,0.15); color: var(--good); border: 1px solid rgba(70,209,140,0.3); }
    code { background: var(--panel-2); padding: 1px 5px; border-radius: 4px; font-size: 12px; }
  </style>
</head>
<body>
<header>
  <h1>AKS Node Readiness · nodeinit-bench</h1>
  <div class="sub">T0 → Node Ready decomposition. Generated <span id="genAt"></span>.</div>
</header>

<section class="stats" id="statRow"></section>

<main>
  <section class="panel">
    <h2>Cross-Run Comparison <span class="legend-pill warn">Stacked phases</span></h2>
    <div class="desc">One bar per run; each segment is a phase whose width is its duration. Bars are anchored at T0 so segments are sequential when phases are sequential, overlapping when phases are concurrent. Look for the segment that varies most across runs.</div>
    <div class="chart" id="cmp" style="height:520px"></div>
  </section>

  <section class="grid-2">
    <section class="panel">
      <h2>Per-Phase Distribution <span class="legend-pill crit">Variance &amp; bottleneck</span></h2>
      <div class="desc">Box plot per phase across all runs. The box spans p25-p75; the line is p50; whiskers are min/max. Wide boxes mean variable phases; tall absolute values mean dominant phases.</div>
      <div class="chart" id="dist" style="height:560px"></div>
    </section>

    <section class="panel">
      <h2>Critical Path to Node Ready <span class="legend-pill good">Gating chain</span></h2>
      <div class="desc">The chain that actually gates <code>Node Ready=True</code>: every link must complete before the next one can start. Median across runs.</div>
      <div class="chart" id="critpath" style="height:560px"></div>
    </section>
  </section>

  <section class="panel">
    <h2>Per-Run Timeline <span class="legend-pill warn">Interactive Gantt</span></h2>
    <div class="ctrl">
      <label for="runSel">Inspect run:</label>
      <select id="runSel"></select>
    </div>
    <div class="desc">Wall-clock view of one run. Hover for exact start/end/duration. Phases marked <span class="legend-pill warn">inferred</span> are derived from metrics rather than directly observed transitions.</div>
    <div class="chart" id="tl" style="height:560px"></div>
  </section>

  <section class="panel">
    <h2>CNS Internal Metrics <span class="legend-pill good">Prometheus</span></h2>
    <div class="desc">Histograms scraped from each CNS pod's <code>:10092/metrics</code>. Cumulative across the pod's lifetime at scrape time, so most useful for confirming whether sync_host_nc_version_latency hit its slow path (initial NMA wait) and for relative comparison across runs.</div>
    <div class="chart" id="metrics" style="height:520px"></div>
  </section>
</main>

<script>
const DATA = /*__DATA__*/;

// Phase color palette - perceptually distinct, dark-mode friendly.
const PHASE_COLORS = {
  'vm-provision':            '#5a6377',
  'node-registered':         '#7e88a3',
  'dnc-rc-create-nnc':       '#5b9cff',
  'dnc-rc-create-nc':        '#3d7ed4',
  'nnc-status-written':      '#4f9bb8',
  'cns-pod-schedule-latency':'#4ec9a8',
  'cns-init-image-pull':     '#46d18c',
  'cns-init-container-run':  '#5fbf63',
  'cns-init-to-main-gap':    '#b2c750',
  'cns-image-pull':          '#9bc44a',
  'cns-container-start':     '#d4a93d',
  'cns-exec-gap':            '#ff8a3d',
  'cns-process-bootstrap':   '#f5a623',
  'cns-nnc-ingest':          '#e08c4f',
  'cns-sync-host-nc-version':'#e76b3c',
  'cns-listener-ready':      '#d54a6b',
  'cns-conflist-write':      '#ff5d6c',
  'cns-pod-ready':           '#a872d6',
  'node-ready':              '#9764d4',
  'kubelet-cni-pickup':      '#7e6ad4',
};
function colorFor(id) { return PHASE_COLORS[id] || '#888'; }

const PLOT_BG = {paper_bgcolor: '#181b22', plot_bgcolor: '#181b22',
                 font: {color: '#e6eaf2', family: '-apple-system,BlinkMacSystemFont,Segoe UI,system-ui,sans-serif', size: 11},
                 xaxis: {gridcolor: '#2a2f3a', zerolinecolor: '#2a2f3a'},
                 yaxis: {gridcolor: '#2a2f3a', zerolinecolor: '#2a2f3a'}};

document.getElementById('genAt').textContent = new Date(DATA.generatedAt).toLocaleString();

// ---------- Stat row ----------
function fmtSec(n) { return n.toFixed(n >= 10 ? 1 : 2); }
function statTile(label, value, unit) {
  return '<div class="stat"><div class="label">' + label + '</div>' +
         '<div class="value">' + value + '<span class="unit">' + (unit || '') + '</span></div></div>';
}
const nr = DATA.aggregates.nodeReady || {count:0,p50:0,p95:0,max:0};
// Identify the dominant phase by p50 (excluding aggregate spans like node-ready, cns-pod-ready, vm-provision which include external time).
const aggregatePhases = new Set(['node-ready','cns-pod-ready','vm-provision','node-registered']);
let dominant = {id: '-', p50: 0};
for (const [id, s] of Object.entries(DATA.aggregates.perPhase || {})) {
  if (aggregatePhases.has(id)) continue;
  if (s.p50 > dominant.p50) dominant = {id: id, p50: s.p50};
}
document.getElementById('statRow').innerHTML = [
  statTile('Runs observed', nr.count, ''),
  statTile('Node Ready p50', fmtSec(nr.p50), 's'),
  statTile('Node Ready p95', fmtSec(nr.p95), 's'),
  statTile('Node Ready max', fmtSec(nr.max), 's'),
  statTile('Dominant phase', dominant.id + ' (' + fmtSec(dominant.p50) + 's)', ''),
].join('');

// ---------- Run selector ----------
const sel = document.getElementById('runSel');
DATA.runs.forEach(r => {
  const o = document.createElement('option');
  o.value = String(r.runId);
  o.textContent = 'run ' + r.runId + '  ·  ' + r.node + '  (' + r.t0.replace('T',' ').replace('Z','') + ')';
  sel.appendChild(o);
});

// ---------- 1. Cross-run stacked comparison ----------
function renderCmp() {
  // For each phase, one trace; X = duration, Y = run, base = startRel relative to T0.
  // Bars overlap (barmode: 'overlay') to show concurrency correctly.
  const traces = [];
  for (const phase of DATA.phaseOrder) {
    const x = [], y = [], base = [], hover = [];
    for (const r of DATA.runs) {
      const sp = r.spans.find(s => s.id === phase);
      if (!sp || sp.missing) continue;
      x.push(sp.dur);
      y.push('run ' + r.runId);
      base.push(sp.startRel);
      hover.push(phase + ': ' + sp.dur.toFixed(2) + 's<br>relative t=[' + sp.startRel.toFixed(1) + 's, ' + sp.endRel.toFixed(1) + 's]<br>' + r.node);
    }
    if (!x.length) continue;
    traces.push({type: 'bar', orientation: 'h', name: phase, x: x, y: y, base: base,
                 marker: {color: colorFor(phase), opacity: 0.85},
                 hovertemplate: '%{customdata}<extra></extra>', customdata: hover});
  }
  Plotly.newPlot('cmp', traces, Object.assign({}, PLOT_BG, {
    barmode: 'overlay',
    xaxis: Object.assign({}, PLOT_BG.xaxis, {title: 'seconds since T0 (Node creationTimestamp)', tickformat: '.0f'}),
    yaxis: Object.assign({}, PLOT_BG.yaxis, {automargin: true}),
    margin: {l: 80, r: 16, t: 12, b: 48},
    legend: {orientation: 'h', y: -0.15, font: {size: 10}},
    showlegend: true,
  }), {responsive: true, displaylogo: false});
}

// ---------- 2. Per-phase box plot ----------
function renderDist() {
  // X = phase, Y = duration. One box per phase aggregating runs.
  const traces = [];
  for (const phase of DATA.phaseOrder) {
    const ys = [];
    for (const r of DATA.runs) {
      const sp = r.spans.find(s => s.id === phase);
      if (!sp || sp.missing) continue;
      ys.push(sp.dur);
    }
    if (!ys.length) continue;
    traces.push({type: 'box', y: ys, name: phase, marker: {color: colorFor(phase)},
                 boxpoints: 'all', jitter: 0.4, pointpos: 0, line: {width: 1}, fillcolor: colorFor(phase) + '44'});
  }
  Plotly.newPlot('dist', traces, Object.assign({}, PLOT_BG, {
    yaxis: Object.assign({}, PLOT_BG.yaxis, {title: 'duration (s)', type: 'log', dtick: 1}),
    xaxis: Object.assign({}, PLOT_BG.xaxis, {tickangle: -35, automargin: true}),
    margin: {l: 60, r: 16, t: 12, b: 120},
    showlegend: false,
  }), {responsive: true, displaylogo: false});
}

// ---------- 3. Critical path waterfall ----------
function renderCritPath() {
  // The phases that gate Node Ready in series. Each subsequent phase starts
  // when the previous one ended.
  // Update the critical path to include the exec gap (it gates everything
// downstream).
const chain = [
  'cns-pod-schedule-latency',
  'cns-init-image-pull',
  'cns-init-container-run',
  'cns-init-to-main-gap',
  'cns-container-start',
  'cns-exec-gap',
  'cns-process-bootstrap',
  'cns-nnc-ingest',
  'cns-sync-host-nc-version',
  'cns-conflist-write',
  'kubelet-cni-pickup',
];
  const meds = chain.map(p => (DATA.aggregates.perPhase[p] || {p50: 0}).p50);
  // Build a vertical waterfall.
  const x = chain;
  const measure = chain.map(_ => 'relative');
  Plotly.newPlot('critpath', [{
    type: 'waterfall',
    orientation: 'v',
    x: x,
    y: meds,
    measure: measure,
    text: meds.map(v => v.toFixed(2) + 's'),
    textposition: 'outside',
    increasing: {marker: {color: '#4ea1ff'}},
    decreasing: {marker: {color: '#46d18c'}},
    totals: {marker: {color: '#9764d4'}},
    connector: {line: {color: '#444b58'}},
    hovertemplate: '%{x}<br>p50: %{y:.2f}s<extra></extra>',
  }], Object.assign({}, PLOT_BG, {
    xaxis: Object.assign({}, PLOT_BG.xaxis, {tickangle: -35, automargin: true}),
    yaxis: Object.assign({}, PLOT_BG.yaxis, {title: 'cumulative gating time (s)', tickformat: '.1f'}),
    margin: {l: 60, r: 16, t: 12, b: 120},
  }), {responsive: true, displaylogo: false});
}

// ---------- 4. Per-run interactive Gantt ----------
function renderTimeline(runId) {
  const r = DATA.runs.find(x => x.runId === runId);
  if (!r) return;
  const traces = [];
  // Reverse phase order so the first row visually = first event.
  const phases = DATA.phaseOrder.slice().reverse();
  for (const phase of phases) {
    const sp = r.spans.find(s => s.id === phase);
    if (!sp || sp.missing) continue;
    const sym = sp.inferred ? ' (inferred)' : '';
    traces.push({
      type: 'bar', orientation: 'h', x: [sp.dur], y: [phase], base: [sp.startRel],
      marker: {color: colorFor(phase), opacity: sp.inferred ? 0.55 : 0.9, line: {color: '#181b22', width: 1}},
      hovertemplate: '<b>' + phase + '</b>' + sym +
                     '<br>start: ' + sp.start.replace('T',' ').replace('Z','') +
                     '<br>end: ' + sp.end.replace('T',' ').replace('Z','') +
                     '<br>duration: ' + sp.dur.toFixed(3) + 's' +
                     '<br>relative: t=[' + sp.startRel.toFixed(1) + 's, ' + sp.endRel.toFixed(1) + 's]' +
                     '<extra></extra>',
      showlegend: false,
    });
  }
  Plotly.newPlot('tl', traces, Object.assign({}, PLOT_BG, {
    barmode: 'overlay',
    xaxis: Object.assign({}, PLOT_BG.xaxis, {title: 'seconds since T0 (' + r.t0.replace('T',' ').replace('Z','') + ')', tickformat: '.0f'}),
    yaxis: Object.assign({}, PLOT_BG.yaxis, {automargin: true}),
    margin: {l: 200, r: 16, t: 12, b: 48},
  }), {responsive: true, displaylogo: false});
}

// ---------- 5. Internal metrics histogram strip ----------
function renderMetrics() {
  // For each known histogram metric, plot the bucket counts as cumulative
  // observations (Prometheus histograms ARE cumulative). We pick a curated
  // set of CNS-relevant metrics; missing ones are skipped.
  const interesting = [
    'sync_host_nc_version_latency_seconds',
    'http_request_latency_seconds',
    'ip_assignment_latency_seconds',
  ];
  const traces = [];
  for (const mname of interesting) {
    for (const r of DATA.runs) {
      if (!r.metrics) continue;
      // collect bucket le -> cumulative count for this run/metric
      const points = [];
      for (const k of Object.keys(r.metrics)) {
        if (!k.startsWith(mname + '_bucket')) continue;
        const m = k.match(/le="([^"]+)"/);
        if (!m) continue;
        const le = m[1] === '+Inf' ? Infinity : parseFloat(m[1]);
        if (isFinite(le)) points.push({le: le, count: r.metrics[k]});
      }
      if (!points.length) continue;
      points.sort((a,b) => a.le - b.le);
      // Convert cumulative -> per-bucket (right edge labelling).
      const x = points.map(p => p.le);
      const y = points.map(p => p.count);
      traces.push({type: 'scatter', mode: 'lines+markers',
        name: mname.replace(/_seconds$/,'') + ' / run ' + r.runId,
        x: x, y: y,
        line: {shape: 'hv'},
        hovertemplate: '%{fullData.name}<br>≤ %{x}s: %{y:.0f} obs<extra></extra>'});
    }
  }
  if (!traces.length) {
    document.getElementById('metrics').innerHTML = '<div style="color:var(--muted);padding:40px;text-align:center">No CNS Prometheus metrics in this run set (use without <code>--skip-metrics</code> to capture).</div>';
    return;
  }
  Plotly.newPlot('metrics', traces, Object.assign({}, PLOT_BG, {
    xaxis: Object.assign({}, PLOT_BG.xaxis, {type: 'log', title: 'bucket upper bound (s, log)'}),
    yaxis: Object.assign({}, PLOT_BG.yaxis, {title: 'cumulative observations'}),
    margin: {l: 60, r: 16, t: 12, b: 60},
    legend: {font: {size: 10}},
  }), {responsive: true, displaylogo: false});
}

renderCmp();
renderDist();
renderCritPath();
renderMetrics();
sel.addEventListener('change', () => renderTimeline(parseInt(sel.value, 10)));
if (DATA.runs.length) {
  sel.value = String(DATA.runs[0].runId);
  renderTimeline(DATA.runs[0].runId);
}
</script>
</body>
</html>
`
