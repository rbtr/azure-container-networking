package hash

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseValid(t *testing.T) {
	in := `c39c4eee92f95ad7e9636ca57e3a35c7d3e36b9f37cd9e0fa07c2c2a9aa7c2cb  azure-vnet
deadbeef1234abcd5678ef0011223344deadbeef1234abcd5678ef0011223344  azure-ipam
`
	sums, err := Parse(strings.NewReader(in))
	require.NoError(t, err)
	assert.Len(t, sums, 2)
	assert.Equal(t, "c39c4eee92f95ad7e9636ca57e3a35c7d3e36b9f37cd9e0fa07c2c2a9aa7c2cb", sums["azure-vnet"])
}

func TestParseMalformed(t *testing.T) {
	_, err := Parse(strings.NewReader("oops not a checksum line\n"))
	assert.Error(t, err)
}

func TestCheckMissingPath(t *testing.T) {
	sums := Checksums{"a": "deadbeef"}
	_, err := sums.Check("b", "/tmp/whatever")
	assert.Error(t, err)
}
