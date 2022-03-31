//go:build windows

package tls

import (
	"fmt"
	"strings"

	"github.com/billgraziano/dpapi"
)

func (cr *certificateRetriever) decrypt(content []byte) (string, error) {
	decrypted, err := dpapi.Decrypt(string(content))
	if err != nil {
		return "", fmt.Errorf("Error decrypting file from path %s with error: %w", cr.settings.CertificatePath, err)
	}
	decrypted = formatDecryptedPemString(decrypted)
	return decrypted, nil
}

// formatDecryptedPemString ensures pem format
// removes spaces that should be line breaks
// ensures headers are properly formatted
// removes null terminated strings that dpapi.decrypt introduces
func formatDecryptedPemString(s string) string {
	s = strings.ReplaceAll(s, " ", "\r\n")
	s = strings.ReplaceAll(s, "\000", "")
	s = strings.ReplaceAll(s, "-----BEGIN\r\nPRIVATE\r\nKEY-----", "-----BEGIN PRIVATE KEY-----")
	s = strings.ReplaceAll(s, "-----END\r\nPRIVATE\r\nKEY-----", "-----END PRIVATE KEY-----")
	s = strings.ReplaceAll(s, "-----BEGIN\r\nCERTIFICATE-----", "-----BEGIN CERTIFICATE-----")
	s = strings.ReplaceAll(s, "-----END\r\nCERTIFICATE-----", "-----END CERTIFICATE-----")
	return s
}
