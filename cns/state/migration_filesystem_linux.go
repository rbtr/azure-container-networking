// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package state

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Azure/azure-container-networking/platform"
)

func (osRollbackFileSystem) durableReplace(source, destination string) error {
	if err := platform.ReplaceFile(source, destination); err != nil {
		return fmt.Errorf("replacing file: %w", err)
	}

	parentPath := filepath.Dir(destination)
	parent, err := os.Open(parentPath)
	if err != nil {
		return fmt.Errorf("opening parent directory %q: %w", parentPath, err)
	}
	if err := parent.Sync(); err != nil {
		_ = parent.Close()
		return fmt.Errorf("syncing parent directory %q: %w", parentPath, err)
	}
	if err := parent.Close(); err != nil {
		return fmt.Errorf("closing parent directory %q: %w", parentPath, err)
	}
	return nil
}
