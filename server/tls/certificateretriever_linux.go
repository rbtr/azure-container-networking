//go:build linux

package tls

// Decrypt is a no-op for linux implementation
func (cr *certificateRetriever) decrypt(content []byte) (string, error) {
	return string(content), nil
}
