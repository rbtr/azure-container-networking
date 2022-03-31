package tls

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"testing"

	"github.com/billgraziano/dpapi"
)

func setup(pemLocation string, pemContent []byte) (func(), error) {
	encryptedPem, _ := dpapi.Encrypt(string(pemContent))
	if err := os.WriteFile(pemLocation, []byte(encryptedPem), 0o644); err != nil {
		return func() {}, err //nolint:wrapcheck // test
	}
	return func() {
		os.Remove(pemLocation)
	}, nil
}
