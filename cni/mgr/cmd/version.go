package cmd

import (
	"fmt"

	"github.com/Azure/azure-container-networking/cni/mgr/internal/buildinfo"
	"github.com/spf13/cobra"
)

// version command.
var version = &cobra.Command{
	Use: "version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(buildinfo.Version)
	},
}

func init() {
	root.AddCommand(version)
}
