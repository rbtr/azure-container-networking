package tls

import (
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"github.com/pkg/errors"
)

const (
	CertLabel       = "CERTIFICATE"
	PrivateKeyLabel = "PRIVATE KEY"
)

var (
	ErrNoCertificate = fmt.Errorf("no certificate block found")
	ErrNoPrivateKey  = fmt.Errorf("no private key found in certificate bundle")
	ErrInvalidFormat = fmt.Errorf("Invalid PEM format")
)

// CertificateRetriever is the interface used by
// both windows and linux and cert from file retriever.
type CertificateRetriever interface {
	GetCertificate() (*x509.Certificate, error)
	GetPrivateKey() (crypto.PrivateKey, error)
}

// Settings are details related to the TLS certificate.
type Settings struct {
	SubjectName     string
	CertificatePath string
	Endpoint        string
	Port            string
}

func GetCertificateRetriever(settings Settings) (CertificateRetriever, error) {
	// if Windows build flag is set, the below will return a windows implementation
	// if Linux build flag is set, the below will return a Linux implementation
	// tls certificate parsed from disk.
	// note if file ends with OS type, ie ends with Linux or Windows
	// go treats that as a build tag : https://golang.org/cmd/go/#hdr-Build_constraints
	return NewCertificateRetriever(settings)
}

type certificateRetriever struct {
	pemBlock []*pem.Block
	settings Settings
}

// GetCertificate Returns the certificate associated with the pem
func (cr *certificateRetriever) GetCertificate() (*x509.Certificate, error) {
	for _, block := range cr.pemBlock {
		if block.Type == CertLabel {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("failed to parse certificate at location %s with error %w", cr.settings.CertificatePath, err)
			}
			if !cert.IsCA {
				return cert, nil
			}
		}
	}
	return nil, ErrNoCertificate
}

// GetPrivateKey Returns the private key associated with the pem
func (cr *certificateRetriever) GetPrivateKey() (crypto.PrivateKey, error) {
	for _, block := range cr.pemBlock {
		if block.Type == PrivateKeyLabel {
			pk, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("could not parse private key %w", err)
			}
			return pk, nil
		}
	}
	return nil, errors.Wrapf(ErrNoPrivateKey, "certificate path: %s", cr.settings.CertificatePath)
}

// ReadFile reads a from disk
func (cr *certificateRetriever) readFile() ([]byte, error) {
	content, err := os.ReadFile(cr.settings.CertificatePath)
	if err != nil {
		return nil, fmt.Errorf("error reading file from path %s with error: %w", cr.settings.CertificatePath, err)
	}
	return content, nil
}

// Parses a file to PEM format
func (cr *certificateRetriever) parsePEMFile(content []byte) error {
	pemBlocks := make([]*pem.Block, 0)
	var pemBlock *pem.Block
	nextPemBlock := content
	for {
		pemBlock, nextPemBlock = pem.Decode(nextPemBlock)
		if pemBlock == nil {
			break
		}
		pemBlocks = append(pemBlocks, pemBlock)
	}
	if len(pemBlocks) < 2 { //nolint:gomnd // need two blocks
		return errors.Wrapf(ErrInvalidFormat, "path: %s", cr.settings.CertificatePath)
	}
	cr.pemBlock = pemBlocks
	return nil
}

// NewCertificateRetriever creates a certificateRetriever.
func NewCertificateRetriever(settings Settings) (CertificateRetriever, error) {
	cr := &certificateRetriever{
		settings: settings,
	}
	content, err := cr.readFile()
	if err != nil {
		return nil, fmt.Errorf("Failed to read file with error %w", err)
	}
	decrypted, err := cr.decrypt(content)
	if err != nil {
		return nil, fmt.Errorf("Failed to decrypt file with error %w", err)
	}
	if err := cr.parsePEMFile([]byte(decrypted)); err != nil {
		return nil, fmt.Errorf("Failed to parse PEM file with error %w", err)
	}
	return cr, nil
}
