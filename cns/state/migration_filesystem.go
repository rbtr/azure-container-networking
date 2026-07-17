// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package state

import (
	"fmt"
	"io"
	"io/fs"
	"os"
)

type rollbackTemporaryFile interface {
	io.WriteCloser
	Name() string
	Sync() error
}

type rollbackFileSystem interface {
	mkdirAll(path string, perm fs.FileMode) error
	createTemp(dir, pattern string) (rollbackTemporaryFile, error)
	remove(path string) error
	durableReplace(source, destination string) error
}

type osRollbackFileSystem struct{}

func (osRollbackFileSystem) mkdirAll(path string, perm fs.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		return fmt.Errorf("making directories: %w", err)
	}
	return nil
}

func (osRollbackFileSystem) createTemp(dir, pattern string) (rollbackTemporaryFile, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, fmt.Errorf("creating temporary file: %w", err)
	}
	return file, nil
}

func (osRollbackFileSystem) remove(path string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("removing file: %w", err)
	}
	return nil
}
