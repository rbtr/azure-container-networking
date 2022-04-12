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

type Checksums map[string]string

func Parse(r io.Reader) (Checksums, error) {
	checksums := Checksums{}
	linescanner := bufio.NewScanner(r)
	linescanner.Split(bufio.ScanLines)

	for linescanner.Scan() {
		line := linescanner.Text()
		entry := strings.Fields(line)
		if len(entry) != 2 { //nolint:gomnd // sha256 checksum file constant
			return nil, errors.Errorf("malformed sha checksum line: %s", line)
		}
		checksums[entry[1]] = entry[0]
	}
	return checksums, nil
}

func (sums Checksums) Check(path string) (bool, error) {
	want, ok := sums[path]
	if !ok {
		return false, errors.Errorf("unknown path %s", path)
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		return false, errors.Wrapf(err, "unable to read file %s", path)
	}
	hash := sha256.Sum256(buf)
	return want == fmt.Sprintf("%x", hash), nil
}
