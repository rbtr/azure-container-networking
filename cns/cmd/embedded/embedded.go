// Package embedded provides the "deploy", "verify", and "list"
// subcommands for the CNS binary. They expose the same surface as
// the dropgz tool: extract embedded payload files (typically CNI
// binaries) to disk and verify their integrity via sha256.
//
// The cns binary checks os.Args[1] in main(); when the first arg is
// "deploy", "verify", or "list" it dispatches to Execute() here
// instead of running the CNS daemon.
package embedded

import (
	"fmt"
	"os"
	"path"

	cnsembed "github.com/Azure/azure-container-networking/cns/embed"
	cnshash "github.com/Azure/azure-container-networking/cns/hash"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// IsSubcommand reports whether argv[1] selects an embedded-payload
// subcommand (as opposed to the daemon flag set parsed by acn).
//
// Recognized subcommands: deploy, verify, list, embedded.
// "embedded" is the umbrella command used when the caller wants to
// pass through cobra's full help (`cns embedded --help`).
func IsSubcommand(argv []string) bool {
	if len(argv) < 2 {
		return false
	}
	switch argv[1] {
	case "deploy", "verify", "list", "embedded":
		return true
	}
	return false
}

var (
	compression = string(cnsembed.Gzip)
	skipVerify  bool
	outs        []string
)

func newLogger() *zap.Logger {
	return zap.New(zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		zapcore.Lock(os.Stdout),
		zap.InfoLevel,
	))
}

func checksum(srcs, dests []string) error {
	if len(srcs) != len(dests) {
		return errors.Wrapf(cnsembed.ErrArgsMismatched, "%d srcs vs %d dests", len(srcs), len(dests))
	}
	rc, err := cnsembed.Extract("sum.txt", cnsembed.None)
	if err != nil {
		return errors.Wrap(err, "failed to extract sum.txt")
	}
	defer rc.Close()
	checksums, err := cnshash.Parse(rc)
	if err != nil {
		return errors.Wrap(err, "failed to parse checksums")
	}
	for i := range srcs {
		ok, err := checksums.Check(srcs[i], dests[i])
		if err != nil {
			return errors.Wrapf(err, "failed to validate %s", dests[i])
		}
		if !ok {
			return errors.Errorf("%s checksum mismatch (src=%s)", dests[i], srcs[i])
		}
	}
	return nil
}

func defaultDests(srcs []string, outDir string) []string {
	dests := make([]string, len(srcs))
	for i, s := range srcs {
		dests[i] = path.Join(outDir, s)
	}
	return dests
}

// Execute parses the subcommand args (os.Args minus the program
// name) and dispatches. Mirrors dropgz/cmd/Execute().
func Execute() {
	root := newRoot()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:          "azure-cns",
		Short:        "Azure CNS — Container Networking Service",
		SilenceUsage: true,
	}

	embedded := &cobra.Command{
		Use:   "embedded",
		Short: "Manage CNI binaries embedded in the CNS image",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List the files embedded in the CNS payload",
		RunE: func(*cobra.Command, []string) error {
			contents, err := cnsembed.Contents()
			if err != nil {
				return err
			}
			for _, c := range contents {
				fmt.Println(c)
			}
			return nil
		},
	}

	var outDir string
	deploy := &cobra.Command{
		Use:   "deploy [files...]",
		Short: "Extract embedded files to disk. With no files, deploys all embedded files except sum.txt.",
		RunE: func(_ *cobra.Command, args []string) error {
			srcs := args
			if len(srcs) == 0 {
				all, err := cnsembed.Contents()
				if err != nil {
					return err
				}
				for _, c := range all {
					if c == "sum.txt" {
						continue
					}
					srcs = append(srcs, c)
				}
			}
			dests := outs
			if len(dests) == 0 {
				if outDir == "" {
					return errors.New("either --output or --out-dir is required")
				}
				if err := os.MkdirAll(outDir, 0o755); err != nil { //nolint:gomnd // standard 0755
					return errors.Wrapf(err, "mkdir %s", outDir)
				}
				dests = defaultDests(srcs, outDir)
			}
			if len(srcs) != len(dests) {
				return errors.Wrapf(cnsembed.ErrArgsMismatched, "%d files vs %d outputs", len(srcs), len(dests))
			}
			log := newLogger().With(zap.String("cmd", "deploy"))
			if err := cnsembed.Deploy(log, srcs, dests, cnsembed.Compression(compression)); err != nil {
				return errors.Wrap(err, "failed to deploy")
			}
			if skipVerify {
				return nil
			}
			if err := checksum(srcs, dests); err != nil {
				return errors.Wrap(err, "checksum verification failed")
			}
			log.Info("verified file integrity")
			return nil
		},
	}
	deploy.Flags().StringVarP(&compression, "compression", "c", string(cnsembed.Gzip), `compression of embedded files: "gzip" (default) or "none"`)
	deploy.Flags().BoolVar(&skipVerify, "skip-verify", false, "do not verify deployed files via sha256")
	deploy.Flags().StringSliceVarP(&outs, "output", "o", nil, "output file path (one per src; alternative to --out-dir)")
	deploy.Flags().StringVar(&outDir, "out-dir", "/opt/cni/bin", "destination directory when --output is not used")

	verify := &cobra.Command{
		Use:   "verify [files...]",
		Short: "Verify on-disk files against the embedded sha256 sums.",
		RunE: func(_ *cobra.Command, args []string) error {
			srcs := args
			if len(srcs) == 0 {
				all, err := cnsembed.Contents()
				if err != nil {
					return err
				}
				for _, c := range all {
					if c == "sum.txt" {
						continue
					}
					srcs = append(srcs, c)
				}
			}
			dests := outs
			if len(dests) == 0 {
				if outDir == "" {
					return errors.New("either --output or --out-dir is required")
				}
				dests = defaultDests(srcs, outDir)
			}
			if err := checksum(srcs, dests); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "ok")
			return nil
		},
	}
	verify.Flags().StringSliceVarP(&outs, "output", "o", nil, "destination file path (one per src; alternative to --out-dir)")
	verify.Flags().StringVar(&outDir, "out-dir", "/opt/cni/bin", "destination directory when --output is not used")

	embedded.AddCommand(list, deploy, verify)
	root.AddCommand(embedded, list, deploy, verify)
	return root
}
