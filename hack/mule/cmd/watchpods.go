package cmd

import (
	"fmt"

	"github.com/azure/azure-container-networking/hack/mule/pkg/watchpods"
	"github.com/spf13/cobra"
)

var watchpodsCmd = &cobra.Command{
	Use: "watchpods",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("exec")
		return watchpods.Execute()
	},
}

func init() {
	rootCmd.AddCommand(watchpodsCmd)
}
