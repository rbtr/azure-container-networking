package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Azure/azure-container-networking/tools/nodeinit-bench/internal/cnslogs"
	"github.com/Azure/azure-container-networking/tools/nodeinit-bench/internal/observer"
	"github.com/Azure/azure-container-networking/tools/nodeinit-bench/internal/scaler"
	"github.com/Azure/azure-container-networking/tools/nodeinit-bench/internal/spans"
	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func newRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Scale the nodepool, observe new Nodes, and emit span artifacts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runE(cmd.Context())
		},
	}
}

func runE(ctx context.Context) error {
	if opts.Cluster == "" || opts.ResourceGroup == "" || opts.Nodepool == "" {
		return fmt.Errorf("--cluster, --resource-group, and --nodepool are required")
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", opts.OutDir, err)
	}

	cfg, err := loadKubeconfig(opts.Kubeconfig)
	if err != nil {
		return err
	}

	sc := scaler.New(opts.ResourceGroup, opts.Cluster, opts.Nodepool)
	obs, err := observer.New(cfg)
	if err != nil {
		return fmt.Errorf("observer: %w", err)
	}
	logScraper := cnslogs.New(cfg)

	all := make([]spans.NodeRun, 0, opts.Runs*opts.Delta)
	for runID := 1; runID <= opts.Runs; runID++ {
		fmt.Printf("=== run %d/%d: adding %d node(s) ===\n", runID, opts.Runs, opts.Delta)
		baseline, err := sc.Count(ctx)
		if err != nil {
			return err
		}
		target := baseline + opts.Delta

		runCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		results, err := observer.RunOne(runCtx, obs, logScraper, func(rctx context.Context) error {
			_, serr := sc.Scale(rctx, target)
			return serr
		}, baseline, target, !opts.SkipMetrics)
		cancel()
		if err != nil {
			return fmt.Errorf("run %d: %w", runID, err)
		}

		for i := range results {
			results[i].RunID = runID
		}
		all = append(all, results...)

		if opts.Cleanup {
			fmt.Printf("=== run %d: scaling back to %d ===\n", runID, baseline)
			// The scale-up operation may still be finishing in ARM even after
			// the new node reached Ready; retry briefly with backoff.
			var serr error
			for attempt := 1; attempt <= 10; attempt++ {
				if _, serr = sc.Scale(ctx, baseline); serr == nil {
					break
				}
				fmt.Printf("cleanup attempt %d: %v\n", attempt, serr)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(30 * time.Second):
				}
			}
			if serr != nil {
				fmt.Printf("cleanup scale failed after retries: %v (continuing)\n", serr)
			}
			// Block until ARM reports the nodepool is Succeeded (no in-flight
			// operation) before the next run, so the next Scale call doesn't
			// race the still-running cleanup.
			waitCtx, waitCancel := context.WithTimeout(ctx, 15*time.Minute)
			if werr := sc.WaitForReady(waitCtx); werr != nil {
				fmt.Printf("wait for nodepool ready after cleanup: %v (continuing)\n", werr)
			}
			waitCancel()
		}
	}

	if err := spans.WriteAll(opts.OutDir, all); err != nil {
		return fmt.Errorf("write outputs: %w", err)
	}
	fmt.Printf("wrote artifacts to %s\n", filepath.Clean(opts.OutDir))
	return nil
}

func loadKubeconfig(path string) (*rest.Config, error) {
	if path == "" {
		path = os.Getenv("KUBECONFIG")
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	cc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	return cc, nil
}
