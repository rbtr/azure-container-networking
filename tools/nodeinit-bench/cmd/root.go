package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

// Options holds flags shared by the root command and subcommands.
type Options struct {
	Kubeconfig    string
	Cluster       string
	ResourceGroup string
	Nodepool      string
	Delta         int
	Runs          int
	OutDir        string
	Cleanup       bool
	Scenario      string
	EnableDebug   bool
	SkipMetrics   bool
}

var opts Options

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "nodeinit-bench",
		Short:        "Measure AKS Node initialization latency for the Azure CNS/Overlay path",
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&opts.Kubeconfig, "kubeconfig", "", "path to kubeconfig (defaults to $KUBECONFIG or in-cluster)")
	root.PersistentFlags().StringVar(&opts.Cluster, "cluster", "", "AKS cluster name")
	root.PersistentFlags().StringVar(&opts.ResourceGroup, "resource-group", "", "AKS resource group")
	root.PersistentFlags().StringVar(&opts.Nodepool, "nodepool", "", "target agent pool name")
	root.PersistentFlags().IntVar(&opts.Delta, "delta", 1, "number of nodes to add per run")
	root.PersistentFlags().IntVar(&opts.Runs, "runs", 1, "number of scale up/down cycles")
	root.PersistentFlags().StringVar(&opts.OutDir, "out", "./out", "directory for spans.csv, gantt.md, gantt.html, summary.md")
	root.PersistentFlags().BoolVar(&opts.Cleanup, "cleanup", false, "scale the nodepool back down after each run")
	root.PersistentFlags().StringVar(&opts.Scenario, "scenario", "linux-overlay", "cluster scenario (v1 supports linux-overlay)")
	root.PersistentFlags().BoolVar(&opts.EnableDebug, "enable-debug-log", false, "enable CNS debug logging for the duration of the run")
	root.PersistentFlags().BoolVar(&opts.SkipMetrics, "skip-metrics", false, "skip scraping the CNS Prometheus /metrics endpoint on each pod")

	root.AddCommand(newRunCmd())
	root.AddCommand(newRenderCmd())
	return root
}

// Execute parses args and runs the root command.
func Execute(ctx context.Context) error {
	root := newRootCmd()
	if err := root.ExecuteContext(ctx); err != nil {
		return fmt.Errorf("nodeinit-bench: %w", err)
	}
	return nil
}
