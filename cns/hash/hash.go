// Package hash parses sha256sum-formatted manifest files and
// verifies file content against the expected checksums.
// Mirrors dropgz/pkg/hash so the CNS-embedded payload uses the same
// sum.txt format as the dropgz images today.
package hash

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pkg/errors"
)

// Checksums maps src filename to its expected hex-encoded sha256.
type Checksums map[string]string

// Parse reads a sha256sum-style manifest (one "<hex> <filename>"
// line per entry) and returns the corresponding Checksums map.
func Parse(r io.Reader) (Checksums, error) {
	checksums := Checksums{}
	s := bufio.NewScanner(r)
	s.Split(bufio.ScanLines)
	for s.Scan() {
		line := s.Text()
		entry := strings.Fields(line)
		if len(entry) != 2 { //nolint:gomnd // sha256 line is "hash filename"
			return nil, errors.Errorf("malformed sha checksum line: %s", line)
		}
		checksums[entry[1]] = entry[0]
	}
	if err := s.Err(); err != nil {
		return nil, errors.Wrap(err, "error reading sum manifest")
	}
	return checksums, nil
}

// Check returns true when the file at dst hashes to the recorded
// expected sha256 for src.
func (sums Checksums) Check(src, dst string) (bool, error) {
	want, ok := sums[src]
	if !ok {
		return false, errors.Errorf("unknown path %s", src)
	}
	buf, err := os.ReadFile(dst)
	if err != nil {
		return false, errors.Wrapf(err, "unable to read file %s", dst)
	}
	have := sha256.Sum256(buf)
	return want == fmt.Sprintf("%x", have), nil
}
