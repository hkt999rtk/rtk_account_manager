package api

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"time"
)

type certificateBundle struct {
	Format      string                    `json:"format"`
	Version     int                       `json:"version"`
	Profile     string                    `json:"profile"`
	Environment string                    `json:"environment"`
	Usage       string                    `json:"usage"`
	TenantID    string                    `json:"tenant_id"`
	Identity    certificateBundleIdentity `json:"identity"`
	Key         certificateBundleKey      `json:"key"`
	Certificate certificateBundleChain    `json:"certificate"`
	Issuance    certificateBundleIssuance `json:"issuance"`
}

type certificateBundleIdentity struct {
	Kind      string   `json:"kind"`
	ID        string   `json:"id"`
	SubjectDN string   `json:"subject_dn"`
	URISANs   []string `json:"uri_sans"`
}
type certificateBundleKey struct {
	Algorithm  string                       `json:"algorithm"`
	SPKISHA256 string                       `json:"spki_sha256"`
	Material   certificateBundleKeyMaterial `json:"material"`
}
type certificateBundleKeyMaterial struct {
	Type string `json:"type"`
}
type certificateBundleChain struct {
	ChainPEM          []string  `json:"chain_pem"`
	SerialNumberHex   string    `json:"serial_number_hex"`
	FingerprintSHA256 string    `json:"fingerprint_sha256"`
	NotBefore         time.Time `json:"not_before"`
	NotAfter          time.Time `json:"not_after"`
}
type certificateBundleIssuance struct {
	RequestID string    `json:"request_id"`
	IssuedAt  time.Time `json:"issued_at"`
}

func newAppCertificateBundle(tenantID, identityID, requestID, leafPEM, chainPEM string, issuedAt time.Time) (*certificateBundle, error) {
	chain, certs, err := appBundleCertificateChain(leafPEM, chainPEM)
	if err != nil {
		return nil, err
	}
	leaf := certs[0]
	algorithm := appBundleKeyAlgorithm(leaf.PublicKey)
	if algorithm == "" {
		return nil, fmt.Errorf("unsupported app certificate public key algorithm")
	}
	spki := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	fingerprint := sha256.Sum256(leaf.Raw)
	serial := strings.ToLower(leaf.SerialNumber.Text(16))
	if len(serial)%2 != 0 {
		serial = "0" + serial
	}
	uriSANs := make([]string, len(leaf.URIs))
	for i, uri := range leaf.URIs {
		uriSANs[i] = uri.String()
	}
	environment := strings.ToLower(strings.TrimSpace(os.Getenv("ACCOUNT_MANAGER_ENV")))
	if environment == "" || environment == "dev" || environment == "development" || environment == "test" {
		environment = "local"
	}
	if environment == "prod" {
		environment = "production"
	}
	return &certificateBundle{
		Format: "rtk_certificate_bundle", Version: 1, Profile: "certificate_only", Environment: environment, Usage: "app_mtls", TenantID: tenantID,
		Identity:    certificateBundleIdentity{Kind: "app", ID: identityID, SubjectDN: leaf.Subject.String(), URISANs: uriSANs},
		Key:         certificateBundleKey{Algorithm: algorithm, SPKISHA256: hex.EncodeToString(spki[:]), Material: certificateBundleKeyMaterial{Type: "caller_managed"}},
		Certificate: certificateBundleChain{ChainPEM: chain, SerialNumberHex: serial, FingerprintSHA256: hex.EncodeToString(fingerprint[:]), NotBefore: leaf.NotBefore.UTC(), NotAfter: leaf.NotAfter.UTC()},
		Issuance:    certificateBundleIssuance{RequestID: requestID, IssuedAt: issuedAt.UTC()},
	}, nil
}

func appBundleCertificateChain(leafPEM, chainPEM string) ([]string, []*x509.Certificate, error) {
	var encoded []string
	var certs []*x509.Certificate
	seen := map[string]bool{}
	for _, value := range []string{leafPEM, chainPEM} {
		rest := []byte(value)
		for len(bytes.TrimSpace(rest)) > 0 {
			block, next := pem.Decode(rest)
			if block == nil || block.Type != "CERTIFICATE" {
				return nil, nil, fmt.Errorf("app certificate chain contains invalid PEM")
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, nil, fmt.Errorf("parse app certificate: %w", err)
			}
			key := string(cert.Raw)
			if !seen[key] {
				seen[key] = true
				certs = append(certs, cert)
				encoded = append(encoded, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})))
			}
			rest = next
		}
	}
	if len(certs) == 0 {
		return nil, nil, fmt.Errorf("app certificate chain is empty")
	}
	return encoded, certs, nil
}

func appBundleKeyAlgorithm(key any) string {
	switch value := key.(type) {
	case *ecdsa.PublicKey:
		if value.Curve.Params().Name == "P-256" {
			return "ecdsa_p256"
		}
	case ed25519.PublicKey:
		return "ed25519"
	case *rsa.PublicKey:
		if value.N.BitLen() == 2048 {
			return "rsa_2048"
		}
	}
	return ""
}
