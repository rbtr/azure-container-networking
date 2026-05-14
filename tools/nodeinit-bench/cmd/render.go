package cmd

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-container-networking/tools/nodeinit-bench/internal/spans"
	"github.com/spf13/cobra"
)

func newRenderCmd() *cobra.Command {
	var out string
	c := &cobra.Command{
		Use:   "render <run-dir> [<run-dir> ...]",
		Short: "Re-emit dashboard.html (and other artifacts) from one or more existing run directories",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if out == "" {
				return fmt.Errorf("--out is required")
			}
			return renderE(cmd.Context(), out, args)
		},
	}
	c.Flags().StringVar(&out, "out", "", "directory to write the combined artifacts into")
	return c
}

// renderE loads every (spans.csv, metrics.csv) pair under each given dir,
// renumbers runs sequentially, and re-emits the full artifact set.
func renderE(_ context.Context, outDir string, dirs []string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}
	var allRuns []spans.NodeRun
	nextID := 1
	for _, d := range dirs {
		runs, err := loadRuns(d, &nextID)
		if err != nil {
			return fmt.Errorf("load %s: %w", d, err)
		}
		allRuns = append(allRuns, runs...)
	}
	if len(allRuns) == 0 {
		return fmt.Errorf("no runs loaded from %v", dirs)
	}
	if err := spans.WriteAll(outDir, allRuns); err != nil {
		return fmt.Errorf("WriteAll: %w", err)
	}
	fmt.Printf("rendered %d runs into %s\n", len(allRuns), filepath.Clean(outDir))
	return nil
}

// loadRuns reads spans.csv (+ optional metrics.csv) from dir and returns
// the reconstructed NodeRuns. RunIDs are reassigned starting from *nextID
// so that callers can stitch multiple directories together without
// collisions.
//
// Older spans.csv files (pre vm-provision split) used a single
// dnc-rc-create-nnc span anchored at the submit time. When we encounter
// such a file, we split it on load: dnc-rc-create-nnc becomes
// (T0 → CreatedNNC) and we synthesize vm-provision = (submit → T0).
//
// Older spans.csv files (pre cns-exec-gap split) used a single
// cns-process-bootstrap span = (containerStartedAt → ReconcilingInitial).
// In our data that is dominated (~95%) by the kernel-exec gap, so we
// remap it to cns-exec-gap on load and leave the narrow
// cns-process-bootstrap missing. New CSVs that already contain
// cns-exec-gap are loaded as-is.
func loadRuns(dir string, nextID *int) ([]spans.NodeRun, error) {
	type key struct {
		runID int
		node  string
	}
	byKey := map[key]*spans.NodeRun{}
	// remember submit times keyed by (runID,node) so we can synthesize
	// vm-provision for older spans.csv files that don't have it.
	submitByKey := map[key]time.Time{}
	// track which keys had cns-exec-gap explicitly, so we know not to
	// remap the cns-process-bootstrap row for them.
	hadExecGap := map[key]bool{}
	// stash the legacy cns-process-bootstrap row (start/end) in case we
	// need to remap it after we've finished iterating.
	legacyBootstrap := map[key]spans.Span{}

	spansCSV := filepath.Join(dir, "spans.csv")
	f, err := os.Open(spansCSV)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	if _, err := r.Read(); err != nil { // header
		return nil, fmt.Errorf("spans header: %w", err)
	}
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("spans row: %w", err)
		}
		// Two CSV layouts exist:
		//   Old (11 cols): run_id,node,pod,t0,span,start,end,duration_s,source,inferred,missing
		//   New (17 cols): run_id,node,pod,t0,boot_state,channel_mode,ipam_v2,swift_v2,
		//                  manage_endpoint_state,dual_stack,span,start,end,duration_s,source,inferred,missing
		// Detect the layout by column count and pick the right indices.
		var iSpan, iStart, iEnd, iSource, iInferred, iMissing int
		switch {
		case len(row) >= 17:
			iSpan, iStart, iEnd, iSource, iInferred, iMissing = 10, 11, 12, 14, 15, 16
		case len(row) >= 11:
			iSpan, iStart, iEnd, iSource, iInferred, iMissing = 4, 5, 6, 8, 9, 10
		default:
			continue
		}
		origRunID, _ := strconv.Atoi(row[0])
		node := row[1]
		k := key{origRunID, node}
		nr, ok := byKey[k]
		if !ok {
			t0, _ := time.Parse(time.RFC3339Nano, row[3])
			nr = &spans.NodeRun{
				RunID:   *nextID,
				Node:    node,
				PodName: row[2],
				T0:      t0,
				Spans:   map[spans.SpanID]spans.Span{},
			}
			// Carry the per-run mode/boot metadata over when the
			// new 17-column layout is present so cross-run grouping
			// and dashboard filters still work after a reload.
			if len(row) >= 17 {
				nr.BootState = row[4]
				nr.Mode = map[string]string{
					"channel_mode":           row[5],
					"ipam_v2":                row[6],
					"swift_v2":               row[7],
					"manage_endpoint_state":  row[8],
					"dual_stack":             row[9],
				}
			}
			*nextID++
			byKey[k] = nr
		}
		sp := spans.Span{
			ID:       spans.SpanID(row[iSpan]),
			Source:   row[iSource],
			Inferred: row[iInferred] == "true",
			Missing:  row[iMissing] == "true",
		}
		if !sp.Missing && row[iStart] != "" && row[iEnd] != "" {
			sp.Start, _ = time.Parse(time.RFC3339Nano, row[iStart])
			sp.End, _ = time.Parse(time.RFC3339Nano, row[iEnd])
		}
		// Newer span emitters write empty start/end columns when the
		// underlying source signal was not observed (e.g. CNS metric
		// gauges from PR #4398 that aren't emitted by older CNS
		// builds), but leave the missing flag as false. Promote that
		// to Missing so the dashboard hides them rather than plotting
		// time-zero as INT64_MIN.
		if !sp.Missing && (sp.Start.IsZero() || sp.End.IsZero()) {
			sp.Missing = true
		}
		// Detect legacy dnc-rc-create-nnc (start == submit, before T0):
		// remember the submit time and rewrite the span to start at T0
		// so it represents only the DNC-RC reaction.
		if sp.ID == spans.SpanDNCRCCreateNNC && !sp.Missing && !nr.T0.IsZero() && sp.Start.Before(nr.T0) {
			submitByKey[k] = sp.Start
			sp.Start = nr.T0
			sp.Source = "node-creationTimestamp+node-event (rewritten on load)"
		}
		if sp.ID == spans.SpanCNSExecGap {
			hadExecGap[k] = true
		}
		if sp.ID == spans.SpanCNSProcessBootstrap && !sp.Missing {
			// stash, decide remap after we know whether cns-exec-gap was present
			legacyBootstrap[k] = sp
		}
		nr.Spans[sp.ID] = sp
	}

	// Remap legacy cns-process-bootstrap → cns-exec-gap when the CSV
	// didn't already contain cns-exec-gap. The narrow
	// cns-process-bootstrap is then unknown and stays missing.
	for k, sp := range legacyBootstrap {
		if hadExecGap[k] {
			continue
		}
		nr := byKey[k]
		if nr == nil {
			continue
		}
		remapped := sp
		remapped.ID = spans.SpanCNSExecGap
		remapped.Source = "legacy cns-process-bootstrap (remapped on load)"
		nr.Spans[spans.SpanCNSExecGap] = remapped
		// blank out the narrow span — endpoint we need (first log) is
		// not available in legacy data.
		nr.Spans[spans.SpanCNSProcessBootstrap] = spans.Span{
			ID:      spans.SpanCNSProcessBootstrap,
			Source:  "missing in legacy CSV (no Using-config timestamp)",
			Missing: true,
		}
	}

	// Synthesize cns-init-to-main-gap from any captured init-container-run
	// end and cns-container-start start times. New CSVs already include it
	// directly; older CSVs don't, but for them we can derive it because the
	// start of cns-container-start was the main container's Pulled event
	// time (positional pair in the old code).
	for _, nr := range byKey {
		if existing, ok := nr.Spans[spans.SpanCNSInitToMainGap]; ok && !existing.Missing {
			continue
		}
		initRun, ok1 := nr.Spans[spans.SpanCNSInitContainerRun]
		cstart, ok2 := nr.Spans[spans.SpanCNSContainerStart]
		if !ok1 || !ok2 || initRun.Missing || cstart.Missing {
			continue
		}
		if initRun.End.IsZero() || cstart.Start.IsZero() || !cstart.Start.After(initRun.End) {
			continue
		}
		nr.Spans[spans.SpanCNSInitToMainGap] = spans.Span{
			ID:     spans.SpanCNSInitToMainGap,
			Start:  initRun.End,
			End:    cstart.Start,
			Source: "synthesized: init.finishedAt → cns-container-start.start",
		}
	}

	// Synthesize vm-provision from any captured submit times if the row
	// wasn't already present in the file.
	for k, submit := range submitByKey {
		nr := byKey[k]
		if nr == nil {
			continue
		}
		if existing, ok := nr.Spans[spans.SpanVMProvision]; ok && !existing.Missing {
			continue
		}
		nr.Spans[spans.SpanVMProvision] = spans.Span{
			ID:     spans.SpanVMProvision,
			Start:  submit,
			End:    nr.T0,
			Source: "synthesized: submit→node-creationTimestamp",
		}
	}

	// optional metrics.csv
	if mf, err := os.Open(filepath.Join(dir, "metrics.csv")); err == nil {
		defer mf.Close()
		mr := csv.NewReader(mf)
		mr.FieldsPerRecord = -1
		if _, err := mr.Read(); err == nil { // header
			for {
				row, err := mr.Read()
				if err == io.EOF {
					break
				}
				if err != nil {
					return nil, fmt.Errorf("metrics row: %w", err)
				}
				if len(row) < 5 {
					continue
				}
				origRunID, _ := strconv.Atoi(row[0])
				node := row[1]
				val, _ := strconv.ParseFloat(strings.TrimSpace(row[4]), 64)
				if nr, ok := byKey[key{origRunID, node}]; ok {
					if nr.Metrics == nil {
						nr.Metrics = map[string]float64{}
					}
					nr.Metrics[row[3]] = val
				}
			}
		}
	}

	out := make([]spans.NodeRun, 0, len(byKey))
	for _, nr := range byKey {
		out = append(out, *nr)
	}
	return out, nil
}
