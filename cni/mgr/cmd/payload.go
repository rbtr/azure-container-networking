package cmd

import (
	"fmt"

	"github.com/Azure/azure-container-networking/cni/mgr/pkg/embed"
	"github.com/Azure/azure-container-networking/cni/mgr/pkg/hash"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// payload command.
var payload = &cobra.Command{
	Use: "payload",
}

// list subcommand
var list = &cobra.Command{
	Use: "list",
	Run: func(*cobra.Command, []string) {
		for _, c := range embed.Contents() {
			fmt.Printf("\t%s\n", c)
		}
	},
}

func checksum(path string) error {
	r, c, err := embed.Extract("bin/sum.txt")
	if err != nil {
		return errors.Wrap(err, "failed to extract checksum file")
	}
	defer c.Close()
	defer r.Close()

	checksums, err := hash.Parse(r)
	if err != nil {
		return errors.Wrap(err, "failed to parse checksums")
	}
	valid, err := checksums.Check(path)
	if err != nil {
		return errors.Wrapf(err, "failed to validate file at %s", path)
	}
	if !valid {
		return errors.Wrapf(err, "%s checksum validation failed", path)
	}
	return nil
}

var skipVerify bool

// deploy subcommand
var deploy = &cobra.Command{
	Use: "deploy",
	RunE: func(cmd *cobra.Command, args []string) error {
		target := args[0]
		log := z.With(zap.String("target", target), zap.String("cmd", "deploy"))
		if err := embed.Deploy(target); err != nil {
			log.Error("failed to deploy", zap.Error(err))
			return errors.Wrapf(err, "failed to deploy %s", target)
		}
		log.Info("successfully wrote file")
		if skipVerify {
			return nil
		}
		if err := checksum(target); err != nil {
			log.Error("failed to verify", zap.Error(err))
			return err
		}
		log.Info("verified file integrity")
		return nil
	},
	Args: cobra.ExactValidArgs(1),
}

// verify subcommand
var verify = &cobra.Command{
	Use: "verify",
	RunE: func(cmd *cobra.Command, args []string) error {
		target := args[0]
		log := z.With(zap.String("target", target), zap.String("cmd", "deploy"))
		if err := checksum(target); err != nil {
			log.Error("failed to verify", zap.Error(err))
			return err
		}
		return nil
	},
	Args: cobra.ExactValidArgs(1),
}

func init() {
	payload.AddCommand(list)

	verify.ValidArgs = embed.Contents()
	payload.AddCommand(verify)

	deploy.ValidArgs = embed.Contents() // setting this after the command is initialized is required
	payload.AddCommand(deploy)
	payload.Flags().BoolVar(&skipVerify, "skip-verify", false, "set to disable checksum validation")

	root.AddCommand(payload)
}
