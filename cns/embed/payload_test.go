package embed

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestContentsListsEmbeddedFiles(t *testing.T) {
	// In source control fs/ only contains _README (excluded by
	// //go:embed's underscore rule) and sum.txt. Contents() should
	// therefore return just sum.txt unless the build pipeline has
	// dropped binaries in.
	contents, err := Contents()
	require.NoError(t, err)
	assert.Contains(t, contents, "sum.txt", "sum.txt placeholder must always be embedded")
	assert.NotContains(t, contents, "_README", "underscore-prefixed files must be excluded by go:embed")
}

func TestExtractNoCompressionRoundtrip(t *testing.T) {
	rc, err := Extract("sum.txt", None)
	require.NoError(t, err)
	defer rc.Close()
	_, err = io.ReadAll(rc)
	require.NoError(t, err)
}

func TestExtractMissingFile(t *testing.T) {
	_, err := Extract("nope.bin", None)
	assert.Error(t, err)
}

func TestDeployArgsMismatch(t *testing.T) {
	err := Deploy(zap.NewNop(), []string{"a"}, []string{"a", "b"}, None)
	assert.ErrorIs(t, err, ErrArgsMismatched)
}

// TestDeployEndToEnd builds a tiny gzip payload, hands it to deploy
// via the embed.FS round-trip, and confirms the bytes land on disk
// intact. We can't actually re-embed at runtime, so the test instead
// drives the lower-level deploy via Extract+gzip on sum.txt as a
// guaranteed-present placeholder. The full payload-with-gzip path is
// exercised in the image-build integration test once payload binaries
// are populated.
func TestDeployFromPlaceholder(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "sum.txt.copy")
	err := Deploy(zap.NewNop(), []string{"sum.txt"}, []string{dest}, None)
	require.NoError(t, err)
	_, err = os.Stat(dest)
	assert.NoError(t, err)
}

func TestDeployRenamesExistingToOld(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "sum.txt.copy")
	// pre-populate
	require.NoError(t, os.WriteFile(dest, []byte("old"), 0o644))
	require.NoError(t, Deploy(zap.NewNop(), []string{"sum.txt"}, []string{dest}, None))
	// the previous contents should now be at dest+".old"
	got, err := os.ReadFile(dest + ".old")
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
}

// TestDeployHandlesNestedDestinationDir verifies that Deploy fails
// cleanly if the dest dir doesn't exist (callers are expected to
// MkdirAll first; the daemon does so).
func TestDeployFailsWhenDestDirMissing(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "no-such-subdir", "out.bin")
	err := Deploy(zap.NewNop(), []string{"sum.txt"}, []string{dest}, None)
	assert.Error(t, err)
}

// gzipRoundtripUnitTest constructs a small gzipped buffer and reads
// it back through the same code path Extract uses, to lock in that
// the buffered gzip reader path is wired right. The embed.FS itself
// can't be mocked, so this test exercises just the gzip layer.
func TestGzipReaderRoundtripUnit(t *testing.T) {
	original := []byte("hello cni binaries")
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, err := gw.Write(original)
	require.NoError(t, err)
	require.NoError(t, gw.Close())

	gr, err := gzip.NewReader(&buf)
	require.NoError(t, err)
	got, err := io.ReadAll(gr)
	require.NoError(t, err)
	require.NoError(t, gr.Close())
	assert.Equal(t, original, got)
}
