package spans

import (
	"encoding/csv"
	"fmt"
	"os"
	"sort"
)

// WriteMetricsCSV emits metrics.csv (run_id, node, pod, metric, value) for
// every NodeRun that has non-empty Metrics. It is a no-op if no run captured
// any metrics, so we don't litter the output directory with empty files.
func WriteMetricsCSV(path string, runs []NodeRun) error {
	any := false
	for _, r := range runs {
		if len(r.Metrics) > 0 {
			any = true
			break
		}
	}
	if !any {
		return nil
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"run_id", "node", "pod", "metric", "value"}); err != nil {
		return err
	}
	for _, r := range runs {
		names := make([]string, 0, len(r.Metrics))
		for k := range r.Metrics {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, name := range names {
			if err := w.Write([]string{
				fmt.Sprintf("%d", r.RunID),
				r.Node,
				r.PodName,
				name,
				fmt.Sprintf("%g", r.Metrics[name]),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
