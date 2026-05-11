// Command nodeinit-bench drives repeatable Node-initialization latency tests
// against AKS + Azure CNS (Overlay) clusters and emits Gantt-ready span data.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/Azure/azure-container-networking/tools/nodeinit-bench/cmd"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := cmd.Execute(ctx); err != nil {
		os.Exit(1)
	}
}
