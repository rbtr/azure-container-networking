package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

var (
	ctx context.Context
	z   *zap.Logger
)

// root represent the base invocation.
var root = &cobra.Command{
	Use: "mgr",
}

func init() {
	// set up signal handlers
	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(context.Background())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
		fmt.Println("exiting")
	}()

	// bind root flags
	root.PersistentFlags().StringP("log-level", "v", "info", "log level [trace,debug,info,warn,error]")
}

func Execute() {
	var err error
	z, err = zap.NewProduction()
	if err != nil {
		os.Exit(1)
	}
	if err = root.Execute(); err != nil {
		z.Error("mgr exiting", zap.Error(err))
		os.Exit(1)
	}
}
