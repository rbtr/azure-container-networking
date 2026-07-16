// Copyright 2017 Microsoft. All rights reserved.
// MIT License

package platform

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadBootID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boot_id")
	require.NoError(t, os.WriteFile(path, []byte("550e8400-e29b-41d4-a716-446655440000\n"), 0o600))

	got, err := readBootID(path)
	require.NoError(t, err)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440000", got)
}

func TestReadBootIDRejectsMissingAndMalformedValues(t *testing.T) {
	_, err := readBootID(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)

	path := filepath.Join(t.TempDir(), "boot_id")
	require.NoError(t, os.WriteFile(path, []byte("not-a-uuid"), 0o600))
	_, err = readBootID(path)
	require.Error(t, err)
}
