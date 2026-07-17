// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package state

import (
	"fmt"

	"github.com/Azure/azure-container-networking/platform"
)

func (osRollbackFileSystem) durableReplace(source, destination string) error {
	return durableReplaceFile(source, destination, platform.ReplaceFile)
}

func durableReplaceFile(source, destination string, replace func(string, string) error) error {
	// ReplaceFile uses MOVEFILE_WRITE_THROUGH, so the replacement is durable before it returns.
	if err := replace(source, destination); err != nil {
		return fmt.Errorf("replacing file: %w", err)
	}
	return nil
}
