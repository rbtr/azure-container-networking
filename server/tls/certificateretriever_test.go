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

	"github.com/stretchr/testify/require"
)

const (
	rsaBits    = 2048
	commonName = "test.azure.com"
)

func TestPemConsumption(t *testing.T) {
	pemContent := createPemCertificate(t)
	currentDirectory, _ := os.Getwd()
	pemLocation := fmt.Sprintf("%s/%s.Pem", currentDirectory, commonName)

	cleanup, err := setup(pemLocation, pemContent)
	require.NoError(t, err)
	defer cleanup()

	config := Settings{
		CertificatePath: pemLocation,
		SubjectName:     commonName,
	}

	fileCertRetriever, err := NewCertificateRetriever(config)
	if err != nil {
		t.Fatalf("Failed to open file certificate retriever %+v", err)
	}
	certificate, err := fileCertRetriever.GetCertificate()
	if err != nil {
		t.Fatalf("Failed to get certificate %+v", err)
	}
	if certificate.Subject.CommonName != commonName {
		t.Fatalf("Received a unexpected subject name %+v", err)
	}
	_, err = fileCertRetriever.GetPrivateKey()
	if err != nil {
		t.Fatalf("Failed to get private key %+v", err)
	}
}

func createPemCertificate(t *testing.T) []byte {
	priv, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		t.Fatalf("Failed to generate private key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Acme Co"},
			CommonName:   commonName,
		},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("Failed to create certificate: %v", err)
	}

	privateKeyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("Could not marshal private key %+v", err)
	}

	if err != nil {
		t.Fatalf("Could not encode certificate to Pem %+v", err)
	}

	pemCert := pem.EncodeToMemory(&pem.Block{Type: CertLabel, Bytes: derBytes})
	pemKey := pem.EncodeToMemory(&pem.Block{Type: PrivateKeyLabel, Bytes: privateKeyBytes})

	pemBundle := fmt.Sprintf("%s%s", pemCert, pemKey)

	return []byte(pemBundle)
}
