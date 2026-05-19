// Package embed provides an in-binary filesystem of payload files that
// CNS extracts to disk during bootstrap. The payload — typically the
// azure-vnet, azure-vnet-ipam, azure-vnet-telemetry, and azure-ipam
// binaries — is assembled at image build time by a Dockerfile stage
// that gzips each file and writes a sum.txt with sha256s.
//
// This package mirrors the dropgz/pkg/embed implementation so the
// payload format and tooling stay shared. Callers use the cns deploy /
// cns verify subcommands (cns/cmd/embedded) or invoke Deploy directly
// from the daemon bootstrap path.
package embed

import (
	"bufio"
	"compress/gzip"
	"embed"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/pkg/errors"
	"go.uber.org/zap"
)

const (
	cwd           = "fs"
	oldFileSuffix = ".old"
)

// ErrArgsMismatched is returned when the source and destination
// slice lengths differ in a Deploy or verify call.
var ErrArgsMismatched = errors.New("mismatched argument count")

// Compression names the compression algorithm used on each payload
// entry. The image-build payload stage always uses Gzip; None is
// kept for testing.
type Compression string

const (
	None Compression = "none"
	Gzip Compression = "gzip"
)

// embedfs contains the embedded payload files. The fs/ directory is
// populated at build time by the CNS image Dockerfile payload stage;
// it ships with only a _README placeholder in source control.
//
//nolint:typecheck // fs directory is populated at build.
//
//go:embed fs
var embedfs embed.FS

// Contents lists every embedded file by basename, excluding directory
// entries. _README and other dot/underscore-prefixed files at the
// top of fs/ are excluded by the //go:embed default behavior.
func Contents() ([]string, error) {
	contents := []string{}
	err := fs.WalkDir(embedfs, cwd, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		_, filename := filepath.Split(p)
		contents = append(contents, filename)
		return nil
	})
	if err != nil {
		return nil, errors.Wrap(err, "error walking embed fs")
	}
	return contents, nil
}

// compoundReadCloser pairs a source file handle with a decompressing
// reader so callers can close both in a single Close call.
type compoundReadCloser struct {
	closer     io.Closer
	readcloser io.ReadCloser
}

func (c *compoundReadCloser) Read(p []byte) (int, error) {
	return c.readcloser.Read(p)
}

func (c *compoundReadCloser) Close() error {
	if err := c.readcloser.Close(); err != nil {
		return err
	}
	return c.closer.Close()
}

// Extract opens the named embedded entry and returns a reader that
// decompresses according to the given Compression.
func Extract(name string, compression Compression) (io.ReadCloser, error) {
	f, err := embedfs.Open(path.Join(cwd, name))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to open file %s", name)
	}
	var rc io.ReadCloser = f
	if compression == Gzip {
		rc, err = gzip.NewReader(bufio.NewReader(f))
		if err != nil {
			return nil, errors.Wrap(err, "failed to build gzip reader")
		}
	}
	return &compoundReadCloser{closer: f, readcloser: rc}, nil
}

// deploy extracts a single payload file to dest. If dest already
// exists it is renamed to dest+".old" first so callers (or
// administrators) can recover the previous version if a deploy goes
// wrong.
func deploy(src, dest string, compression Compression) error {
	rc, err := Extract(src, compression)
	if err != nil {
		return err
	}
	defer rc.Close()
	if _, err := os.Stat(dest); err == nil {
		oldDest := dest + oldFileSuffix
		if err := os.Rename(dest, oldDest); err != nil {
			return errors.Wrapf(err, "failed to rename %s to %s", dest, oldDest)
		}
	}
	target, err := os.OpenFile(dest, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o755) //nolint:gomnd,gosec // executable mode is required
	if err != nil {
		return errors.Wrapf(err, "failed to create file %s", dest)
	}
	defer target.Close()
	buf := bufio.NewWriter(target)
	if _, err := io.Copy(buf, rc); err != nil {
		return errors.Wrapf(err, "failed to copy %s to %s", src, dest)
	}
	return errors.Wrapf(buf.Flush(), "failed to flush %s", dest)
}

// Deploy extracts every src from the embedded filesystem to its
// matching dest, in order. srcs and dests must be the same length.
// Each Deploy invocation logs one structured line per file via the
// supplied zap logger.
func Deploy(log *zap.Logger, srcs, dests []string, compression Compression) error {
	if len(srcs) != len(dests) {
		return errors.Wrapf(ErrArgsMismatched, "%d srcs vs %d dests", len(srcs), len(dests))
	}
	for i := range srcs {
		src := srcs[i]
		dest := dests[i]
		if err := deploy(src, dest, compression); err != nil {
			return err
		}
		log.Info("wrote file", zap.String("src", src), zap.String("dest", dest))
	}
	return nil
}
