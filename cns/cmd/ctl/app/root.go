package app

import (
	"context"

	"github.com/spf13/cobra"
)

var ctx context.Context

// root is the base command when executed without arguments
var root = &cobra.Command{
	Use: "cnsctl",
}
