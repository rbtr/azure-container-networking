package tls

import "os"

func setup(pemLocation string, pemContent []byte) (func(), error) {
	if err := os.WriteFile(pemLocation, pemContent, 0o600); err != nil {
		return func() {}, err //nolint:wrapcheck // test
	}
	return func() {
		os.Remove(pemLocation)
	}, nil
}
