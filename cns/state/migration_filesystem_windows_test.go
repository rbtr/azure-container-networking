//go:build windows

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

var errWindowsNativeReplace = errors.New("native replace failed")

func TestWindowsDurableReplaceNativeBoundary(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr string
	}{
		{name: "success"},
		{
			name:    "failure",
			err:     errWindowsNativeReplace,
			wantErr: "replacing file: native replace failed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			err := durableReplaceFile("source.tmp", `C:\k\azure-cns.json`, func(source, destination string) error {
				calls++
				require.Equal(t, "source.tmp", source)
				require.Equal(t, `C:\k\azure-cns.json`, destination)
				return test.err
			})

			if test.wantErr != "" {
				require.EqualError(t, err, test.wantErr)
				require.ErrorIs(t, err, errWindowsNativeReplace)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, 1, calls)
		})
	}
}

func TestWindowsRollbackAtomicReplacementNativeIntegration(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "azure-cns.json.tmp")
	destination := filepath.Join(root, "azure-cns.json")
	require.NoError(t, os.WriteFile(source, []byte("new-state"), 0o600))
	require.NoError(t, os.WriteFile(destination, []byte("old-state"), 0o600))

	require.NoError(t, (osRollbackFileSystem{}).durableReplace(source, destination))

	got, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, []byte("new-state"), got)
	_, err = os.Stat(source)
	require.ErrorIs(t, err, os.ErrNotExist)
}
