package api

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"
)

func TestNewAppCertificateBundle(t *testing.T) {
	leafPEM, chainPEM := appBundleTestChain(t)
	t.Setenv("ACCOUNT_MANAGER_ENV", "staging")
	issuedAt := time.Now().UTC().Truncate(time.Second)

	bundle, err := newAppCertificateBundle("brand-1", "user-1", "request-1", leafPEM, chainPEM, issuedAt)
	if err != nil {
		t.Fatalf("newAppCertificateBundle: %v", err)
	}
	if bundle.Format != "rtk_certificate_bundle" || bundle.Version != 1 || bundle.Profile != "certificate_only" {
		t.Fatalf("bundle header = %#v", bundle)
	}
	if bundle.Environment != "staging" || bundle.Usage != "app_mtls" || bundle.TenantID != "brand-1" {
		t.Fatalf("bundle scope = %#v", bundle)
	}
	if bundle.Identity.Kind != "app" || bundle.Identity.ID != "user-1" || len(bundle.Identity.URISANs) != 1 {
		t.Fatalf("bundle identity = %#v", bundle.Identity)
	}
	if bundle.Key.Algorithm != "ecdsa_p256" || bundle.Key.Material.Type != "caller_managed" || len(bundle.Key.SPKISHA256) != 64 {
		t.Fatalf("bundle key = %#v", bundle.Key)
	}
	if len(bundle.Certificate.ChainPEM) != 2 || bundle.Certificate.SerialNumberHex != "01ab" || len(bundle.Certificate.FingerprintSHA256) != 64 {
		t.Fatalf("bundle certificate = %#v", bundle.Certificate)
	}
	if bundle.Issuance.RequestID != "request-1" || !bundle.Issuance.IssuedAt.Equal(issuedAt) {
		t.Fatalf("bundle issuance = %#v", bundle.Issuance)
	}
}

func TestAppBundleCertificateChainValidation(t *testing.T) {
	leafPEM, chainPEM := appBundleTestChain(t)
	chain, certs, err := appBundleCertificateChain(leafPEM, leafPEM+chainPEM)
	if err != nil {
		t.Fatalf("deduplicate chain: %v", err)
	}
	if len(chain) != 2 || len(certs) != 2 {
		t.Fatalf("chain lengths = %d, %d", len(chain), len(certs))
	}

	for name, value := range map[string]string{
		"empty":       "",
		"invalid PEM": "not a certificate",
		"invalid DER": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("bad")})),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := appBundleCertificateChain(value, ""); err == nil {
				t.Fatal("expected invalid chain")
			}
		})
	}
}

func TestAppBundleKeyAlgorithm(t *testing.T) {
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	rsa2048, _ := rsa.GenerateKey(rand.Reader, 2048)
	rsa1024, _ := rsa.GenerateKey(rand.Reader, 1024)

	for _, tc := range []struct {
		key  any
		want string
	}{
		{&p256.PublicKey, "ecdsa_p256"},
		{&p384.PublicKey, ""},
		{edKey.Public(), "ed25519"},
		{&rsa2048.PublicKey, "rsa_2048"},
		{&rsa1024.PublicKey, ""},
		{"unsupported", ""},
	} {
		if got := appBundleKeyAlgorithm(tc.key); got != tc.want {
			t.Fatalf("appBundleKeyAlgorithm(%T) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestDeveloperPKITestIssuanceEnabled(t *testing.T) {
	for _, tc := range []struct {
		name, enabled, environment string
		want                       bool
	}{
		{"flag disabled", "false", "staging", false},
		{"production", "true", "production", false},
		{"default local", "true", "", true},
		{"development", "TRUE", "development", true},
		{"dev", " true ", "dev", true},
		{"staging", "true", "staging", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DEVELOPER_PKI_TEST_TOOLS_ENABLED", tc.enabled)
			t.Setenv("ACCOUNT_MANAGER_ENV", tc.environment)
			if got := developerPKITestIssuanceEnabled(); got != tc.want {
				t.Fatalf("developerPKITestIssuanceEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func appBundleTestChain(t *testing.T) (string, string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "RTK Test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identityURI, _ := url.Parse("urn:rtk:app:user-1")
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(0x1ab), Subject: pkix.Name{CommonName: "app-brand-cloud-user:user-1"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), URIs: []*url.URL{identityURI},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
}
