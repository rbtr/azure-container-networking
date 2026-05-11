package spans

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// WriteAll emits spans.csv, gantt.md, gantt.html, dashboard.html, and
// summary.md into outDir.
func WriteAll(outDir string, runs []NodeRun) error {
	if err := writeCSV(filepath.Join(outDir, "spans.csv"), runs); err != nil {
		return fmt.Errorf("csv: %w", err)
	}
	if err := writeMermaid(filepath.Join(outDir, "gantt.md"), runs); err != nil {
		return fmt.Errorf("mermaid: %w", err)
	}
	if err := writePlotly(filepath.Join(outDir, "gantt.html"), runs); err != nil {
		return fmt.Errorf("plotly: %w", err)
	}
	if err := writeDashboard(filepath.Join(outDir, "dashboard.html"), runs); err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}
	if err := writeSummary(filepath.Join(outDir, "summary.md"), runs); err != nil {
		return fmt.Errorf("summary: %w", err)
	}
	if err := WriteMetricsCSV(filepath.Join(outDir, "metrics.csv"), runs); err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	return nil
}

func writeCSV(path string, runs []NodeRun) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{
		"run_id", "node", "pod", "t0", "boot_state", "channel_mode", "ipam_v2", "swift_v2", "manage_endpoint_state", "dual_stack",
		"span", "start", "end", "duration_s", "source", "inferred", "missing",
	}); err != nil {
		return err
	}
	for _, r := range runs {
		mode := r.Mode
		for _, id := range OrderedSpans {
			sp := r.Spans[id]
			var start, end string
			if !sp.Start.IsZero() {
				start = sp.Start.UTC().Format(time.RFC3339Nano)
			}
			if !sp.End.IsZero() {
				end = sp.End.UTC().Format(time.RFC3339Nano)
			}
			dur := ""
			if !sp.Missing {
				dur = fmt.Sprintf("%.3f", sp.Duration().Seconds())
			}
			if err := w.Write([]string{
				fmt.Sprintf("%d", r.RunID),
				r.Node,
				r.PodName,
				r.T0.UTC().Format(time.RFC3339Nano),
				r.BootState,
				mode["channel_mode"],
				mode["ipam_v2"],
				mode["swift_v2"],
				mode["manage_endpoint_state"],
				mode["dual_stack"],
				string(id),
				start,
				end,
				dur,
				sp.Source,
				boolStr(sp.Inferred),
				boolStr(sp.Missing),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func writeMermaid(path string, runs []NodeRun) error {
	var b strings.Builder
	b.WriteString("# Node init Gantt\n\n")
	for _, r := range runs {
		fmt.Fprintf(&b, "## run %d · %s\n\nT0 = %s\n\n```mermaid\ngantt\n  dateFormat  YYYY-MM-DDTHH:mm:ss.SSSZ\n  title Node init for %s\n  axisFormat %%H:%%M:%%S\n",
			r.RunID, r.Node, r.T0.UTC().Format(time.RFC3339Nano), r.Node)
		fmt.Fprintf(&b, "  section spans\n")
		for _, id := range OrderedSpans {
			sp := r.Spans[id]
			if sp.Missing {
				fmt.Fprintf(&b, "  %s :crit, %s, 0s\n", string(id), r.T0.UTC().Format("2006-01-02T15:04:05.000Z"))
				continue
			}
			tag := ""
			if sp.Inferred {
				tag = "active,"
			}
			fmt.Fprintf(&b, "  %s :%s%s, %s\n",
				string(id), tag,
				sp.Start.UTC().Format("2006-01-02T15:04:05.000Z"),
				sp.End.UTC().Format("2006-01-02T15:04:05.000Z"),
			)
		}
		b.WriteString("```\n\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func writePlotly(path string, runs []NodeRun) error {
	type row struct {
		Task  string  `json:"Task"`
		Span  string  `json:"Span"`
		Start string  `json:"Start"`
		End   string  `json:"End"`
		Dur   float64 `json:"Dur"`
		Miss  bool    `json:"Miss"`
		Inf   bool    `json:"Inferred"`
	}
	var rows []row
	for _, r := range runs {
		for _, id := range OrderedSpans {
			sp := r.Spans[id]
			if sp.Missing {
				continue
			}
			rows = append(rows, row{
				Task:  fmt.Sprintf("run%d/%s", r.RunID, r.Node),
				Span:  string(id),
				Start: sp.Start.UTC().Format(time.RFC3339Nano),
				End:   sp.End.UTC().Format(time.RFC3339Nano),
				Dur:   sp.Duration().Seconds(),
				Inf:   sp.Inferred,
			})
		}
	}
	rowsJSON, err := jsonMarshal(rows)
	if err != nil {
		return err
	}
	html := fmt.Sprintf(plotlyTemplate, rowsJSON)
	return os.WriteFile(path, []byte(html), 0o644)
}

const plotlyTemplate = `<!doctype html>
<html><head><meta charset="utf-8"><title>nodeinit-bench</title>
<script src="https://cdn.plot.ly/plotly-2.35.2.min.js"></script>
<style>body{font-family:system-ui;margin:20px;}</style>
</head><body>
<h2>Node init Gantt</h2>
<div id="chart" style="width:100%%;height:90vh;"></div>
<script>
const rows = %s;
const traces = {};
for (const r of rows) {
  const key = r.Span;
  if (!traces[key]) traces[key] = {x: [], y: [], base: [], type: 'bar', orientation: 'h', name: key, hovertemplate: '%%{y}<br>' + key + ': %%{customdata:.3f}s<extra></extra>', customdata: []};
  const start = new Date(r.Start).getTime();
  const end = new Date(r.End).getTime();
  traces[key].base.push(start);
  traces[key].x.push(end - start);
  traces[key].y.push(r.Task);
  traces[key].customdata.push(r.Dur);
}
const data = Object.values(traces);
const layout = {barmode: 'overlay', xaxis: {type: 'date'}, yaxis: {automargin: true}, margin: {l: 240, r: 20, t: 20, b: 40}, height: Math.max(400, 40 * data.length)};
Plotly.newPlot('chart', data, layout, {responsive: true});
</script>
</body></html>
`

func writeSummary(path string, runs []NodeRun) error {
	type stats struct {
		count int
		min   float64
		p50   float64
		p95   float64
		p99   float64
		max   float64
		miss  int
	}
	perSpan := map[SpanID]*stats{}
	for _, id := range OrderedSpans {
		perSpan[id] = &stats{}
	}
	for _, r := range runs {
		for _, id := range OrderedSpans {
			s := perSpan[id]
			sp := r.Spans[id]
			if sp.Missing {
				s.miss++
				continue
			}
			s.count++
		}
	}
	// Collect durations per span then compute percentiles.
	dur := map[SpanID][]float64{}
	for _, r := range runs {
		for _, id := range OrderedSpans {
			sp := r.Spans[id]
			if sp.Missing {
				continue
			}
			dur[id] = append(dur[id], sp.Duration().Seconds())
		}
	}
	for id, v := range dur {
		sort.Float64s(v)
		s := perSpan[id]
		if len(v) == 0 {
			continue
		}
		s.min = v[0]
		s.max = v[len(v)-1]
		s.p50 = pct(v, 0.50)
		s.p95 = pct(v, 0.95)
		s.p99 = pct(v, 0.99)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# nodeinit-bench summary\n\nNodes observed: %d\n\n", len(runs))
	fmt.Fprintf(&b, "| span | n | missing | min | p50 | p95 | p99 | max |\n|---|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, id := range OrderedSpans {
		s := perSpan[id]
		fmt.Fprintf(&b, "| `%s` | %d | %d | %.3f | %.3f | %.3f | %.3f | %.3f |\n",
			id, s.count, s.miss, s.min, s.p50, s.p95, s.p99, s.max)
	}
	b.WriteString("\nAll durations in seconds. `cns-conflist-write` reads the Node annotation ")
	b.WriteString("written by the conflist-mtime DaemonSet. `kubelet-cni-pickup` is inferred ")
	b.WriteString("(conflist mtime → Node Ready).\n")
	appendMetricsSection(&b, runs)
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// appendMetricsSection adds an "optional CNS internal metrics" table showing
// per-histogram sum/count across nodes. It is skipped entirely if no run
// captured metrics.
func appendMetricsSection(b *strings.Builder, runs []NodeRun) {
	type agg struct {
		sum, count float64
		nodes      int
	}
	byMetric := map[string]*agg{}
	for _, r := range runs {
		for k, v := range r.Metrics {
			base, kind := splitHistogramKey(k)
			if kind == "" {
				continue
			}
			a := byMetric[base]
			if a == nil {
				a = &agg{}
				byMetric[base] = a
			}
			switch kind {
			case "sum":
				a.sum += v
			case "count":
				a.count += v
			}
			a.nodes++
		}
	}
	if len(byMetric) == 0 {
		return
	}
	names := make([]string, 0, len(byMetric))
	for k := range byMetric {
		names = append(names, k)
	}
	sort.Strings(names)
	b.WriteString("\n## optional CNS internal metrics\n\n")
	b.WriteString("| metric | observations | sum | mean |\n|---|---:|---:|---:|\n")
	for _, n := range names {
		a := byMetric[n]
		mean := 0.0
		if a.count > 0 {
			mean = a.sum / a.count
		}
		fmt.Fprintf(b, "| `%s` | %.0f | %.6f | %.6f |\n", n, a.count, a.sum, mean)
	}
}

// splitHistogramKey parses a flattened key like `foo_sum` or
// `foo_count{url="..."}` into ("foo{url=\"...\"}", "sum"|"count") — i.e.
// the metric name with labels but without the _sum/_count token. Returns
// ("", "") for keys that are not histogram sum/count rows.
func splitHistogramKey(k string) (base, kind string) {
	labels := ""
	name := k
	if i := strings.IndexByte(k, '{'); i >= 0 {
		name, labels = k[:i], k[i:]
	}
	switch {
	case strings.HasSuffix(name, "_sum"):
		return strings.TrimSuffix(name, "_sum") + labels, "sum"
	case strings.HasSuffix(name, "_count"):
		return strings.TrimSuffix(name, "_count") + labels, "count"
	}
	return "", ""
}

func pct(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := q * float64(len(sorted)-1)
	lo := int(pos)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}
