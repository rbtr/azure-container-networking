package embed

import (
	"bufio"
	"compress/gzip"
	"embed"
	"io"
	"os"
	"path/filepath"

	"github.com/pkg/errors"
)

const cwd = "."

// fs contains the CNI binaries for deployment, as a read-only FileSystem rooted at "bin/".
//nolint:typecheck // dir is populated at build.
//go:embed bin
var fs embed.FS

func Contents() []string {
	return treeDir(cwd)
}

func treeDir(path string) []string {
	contents, err := fs.ReadDir(path)
	if err != nil {
		return nil
	}
	tree := []string{}
	for _, entry := range contents {
		if entry.IsDir() {
			tree = append(tree, treeDir(filepath.Join(path, entry.Name()))...)
			continue
		}
		tree = append(tree, filepath.Join(path, entry.Name()))
	}
	return tree
}

func Extract(path string) (io.ReadCloser, io.Closer, error) {
	f, err := fs.Open(path)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "failed to open file %s", path)
	}
	r, err := gzip.NewReader(bufio.NewReader(f))
	r.Close()
	return r, f, errors.Wrap(err, "failed to build gzip reader")
}

func Deploy(path string) error {
	r, c, err := Extract(path)
	if err != nil {
		return err
	}
	defer c.Close()
	defer r.Close()
	target, err := os.Create(path)
	if err != nil {
		return errors.Wrapf(err, "failed to create file %s", path)
	}
	defer target.Close()
	_, err = io.Copy(bufio.NewWriter(target), r)
	return errors.Wrap(err, "failed to copy during deploy")
}
