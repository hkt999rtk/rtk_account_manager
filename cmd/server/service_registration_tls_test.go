package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"context"
	"rtk_account_manager/internal/api"
	"rtk_account_manager/internal/store"
)

type tlsRegistrationStore struct {
	registrations int
	principal     store.PlatformServicePrincipal
}

func (*tlsRegistrationStore) ApprovePlatformServiceWorkload(context.Context, store.PlatformServiceWorkloadApproval) error {
	return nil
}
func (*tlsRegistrationStore) RevokePlatformServiceWorkload(context.Context, store.PlatformServiceWorkloadRevocation, time.Time) error {
	return nil
}
func (*tlsRegistrationStore) SetPlatformServiceStatus(context.Context, string, string, string, string) (int64, error) {
	return 1, nil
}
func (s *tlsRegistrationStore) RegisterPlatformServiceInstance(_ context.Context, _ store.PlatformServiceRegistration, principal store.PlatformServicePrincipal, _ time.Time) (store.PlatformServiceLease, error) {
	s.registrations++
	s.principal = principal
	return store.PlatformServiceLease{ServiceID: "mqtt"}, nil
}
func (*tlsRegistrationStore) HeartbeatPlatformServiceInstance(context.Context, string, string, string, bool, store.PlatformServicePrincipal, time.Time) (store.PlatformServiceLease, error) {
	return store.PlatformServiceLease{}, nil
}
func (*tlsRegistrationStore) DeregisterPlatformServiceInstance(context.Context, string, string, store.PlatformServicePrincipal, time.Time) error {
	return nil
}
func (*tlsRegistrationStore) PublishPlatformServiceManifest(context.Context, string, string, string, store.PlatformServicePrincipal, time.Time) (int64, error) {
	return 1, nil
}
func (*tlsRegistrationStore) ListPlatformServiceOptions(context.Context, string, time.Time) (store.PlatformServiceCatalog, error) {
	return store.PlatformServiceCatalog{}, nil
}

func TestServiceRegistrationTLSHandshakeAndCRLBlocksRevokedClient(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	issuer, issuerKey := testServiceCRLIssuer(t, now)
	serverCert := issueServiceTLSLeaf(t, issuer, issuerKey, now, 10, "platform-register", true)
	clientCert := issueServiceTLSLeaf(t, issuer, issuerKey, now, 42, "service:mqtt", false)
	roots := x509.NewCertPool()
	roots.AddCert(issuer)
	crlPath := filepath.Join(t.TempDir(), "service.crl")
	writeServiceCRL(t, crlPath, issuer, issuerKey, now, nil)
	tlsConfig, err := serviceRegistrationTLSConfig(serverCert, roots, crlPath)
	if err != nil {
		t.Fatal(err)
	}
	backend := &tlsRegistrationStore{}
	apiServer := api.New(nil, nil)
	apiServer.ConfigurePlatformServices(backend, "staging")
	server := httptest.NewUnstartedServer(serviceRegistrationCRLMiddleware(apiServer.ServiceRouter(), crlPath))
	server.TLS = tlsConfig
	server.StartTLS()
	defer server.Close()
	client := func(certs []tls.Certificate) *http.Client {
		return &http.Client{Transport: &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{
			RootCAs: roots, Certificates: certs, MinVersion: tls.VersionTLS13,
		}}}
	}
	register := func(httpClient *http.Client) (*http.Response, error) {
		request, err := http.NewRequest(http.MethodPut, server.URL+"/v1/platform/services/mqtt/instances/mqtt-1", strings.NewReader(`{"request_id":"register-1","service_id":"mqtt","instance_id":"mqtt-1"}`))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		return httpClient.Do(request)
	}
	response, err := register(client([]tls.Certificate{clientCert}))
	if err != nil {
		t.Fatalf("valid mTLS registration: %v", err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || backend.registrations != 1 || backend.principal.Environment != "staging" || backend.principal.CertificateSubject != "service:mqtt" {
		t.Fatalf("registration status=%d count=%d principal=%+v", response.StatusCode, backend.registrations, backend.principal)
	}
	if response, err := register(client(nil)); err == nil {
		response.Body.Close()
		t.Fatalf("client without service certificate reached HTTP: %d", response.StatusCode)
	}
	writeServiceCRL(t, crlPath, issuer, issuerKey, now, []x509.RevocationListEntry{{SerialNumber: big.NewInt(42), RevocationTime: now.Add(-time.Minute)}})
	if response, err := register(client([]tls.Certificate{clientCert})); err == nil {
		response.Body.Close()
		t.Fatalf("revoked client reached HTTP: %d", response.StatusCode)
	}
	if backend.registrations != 1 {
		t.Fatalf("revoked client registered after CRL rotation: %d", backend.registrations)
	}
}

func TestServiceRegistrationCRLRevokesExistingKeepAliveConnection(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	issuer, issuerKey := testServiceCRLIssuer(t, now)
	serverCert := issueServiceTLSLeaf(t, issuer, issuerKey, now, 10, "platform-register", true)
	clientCert := issueServiceTLSLeaf(t, issuer, issuerKey, now, 42, "service:mqtt", false)
	roots := x509.NewCertPool()
	roots.AddCert(issuer)
	crlPath := filepath.Join(t.TempDir(), "service.crl")
	writeServiceCRL(t, crlPath, issuer, issuerKey, now, nil)
	tlsConfig, err := serviceRegistrationTLSConfig(serverCert, roots, crlPath)
	if err != nil {
		t.Fatal(err)
	}
	backend := &tlsRegistrationStore{}
	apiServer := api.New(nil, nil)
	apiServer.ConfigurePlatformServices(backend, "staging")
	server := httptest.NewUnstartedServer(serviceRegistrationCRLMiddleware(apiServer.ServiceRouter(), crlPath))
	server.TLS = tlsConfig
	server.StartTLS()
	defer server.Close()
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{clientCert}, MinVersion: tls.VersionTLS13}, MaxIdleConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	register := func(trace *httptrace.ClientTrace) (*http.Response, error) {
		request, err := http.NewRequest(http.MethodPut, server.URL+"/v1/platform/services/mqtt/instances/mqtt-1", strings.NewReader(`{"request_id":"register-1","service_id":"mqtt","instance_id":"mqtt-1"}`))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		if trace != nil {
			request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
		}
		return client.Do(request)
	}
	first, err := register(nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK || backend.registrations != 1 {
		t.Fatalf("initial status=%d registrations=%d", first.StatusCode, backend.registrations)
	}
	writeServiceCRL(t, crlPath, issuer, issuerKey, now, []x509.RevocationListEntry{{SerialNumber: big.NewInt(42), RevocationTime: now.Add(-time.Minute)}})
	reused := false
	second, err := register(&httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }})
	if err != nil {
		t.Fatalf("request over keep-alive connection: %v", err)
	}
	io.Copy(io.Discard, second.Body)
	second.Body.Close()
	if !reused || second.StatusCode != http.StatusUnauthorized || backend.registrations != 1 {
		t.Fatalf("reused=%v status=%d registrations=%d", reused, second.StatusCode, backend.registrations)
	}
}

func issueServiceTLSLeaf(t *testing.T, issuer *x509.Certificate, issuerKey *ecdsa.PrivateKey, now time.Time, serial int64, name string, server bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	if server {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	raw, err := x509.CreateCertificate(rand.Reader, template, issuer, &key.PublicKey, issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateKey}))
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestServiceRegistrationCRLReloadAndRevocation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	issuer, key := testServiceCRLIssuer(t, now)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(42)}
	state := tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leaf, issuer}}}
	path := filepath.Join(t.TempDir(), "service.crl")
	writeServiceCRL(t, path, issuer, key, now, nil)
	if err := verifyServiceRegistrationCRL(state, path, now); err != nil {
		t.Fatalf("unrevoked service certificate rejected: %v", err)
	}
	tlsConfig, err := serviceRegistrationTLSConfig(tls.Certificate{}, x509.NewCertPool(), path)
	if err != nil {
		t.Fatalf("current CRL should permit listener startup: %v", err)
	}
	if err := tlsConfig.VerifyConnection(state); err != nil {
		t.Fatalf("TLS listener rejected current service CRL: %v", err)
	}
	writeServiceCRL(t, path, issuer, key, now, []x509.RevocationListEntry{{SerialNumber: leaf.SerialNumber, RevocationTime: now.Add(-time.Minute)}})
	if err := verifyServiceRegistrationCRL(state, path, now); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("rotated CRL must revoke the service certificate, got %v", err)
	}
	if err := tlsConfig.VerifyConnection(state); err == nil {
		t.Fatal("TLS listener must reject service certificate after CRL rotation")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := verifyServiceRegistrationCRL(state, path, now); err == nil {
		t.Fatal("missing CRL must fail closed")
	}
}

func TestServiceRegistrationCRLRejectsStaleAndWrongIssuer(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	issuer, key := testServiceCRLIssuer(t, now)
	otherIssuer, otherKey := testServiceCRLIssuer(t, now)
	state := tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{SerialNumber: big.NewInt(7)}, issuer}}}
	path := filepath.Join(t.TempDir(), "service.crl")
	writeServiceCRL(t, path, issuer, key, now.Add(-2*time.Hour), nil)
	if err := verifyServiceRegistrationCRL(state, path, now); err == nil || !strings.Contains(err.Error(), "not current") {
		t.Fatalf("expired CRL should be rejected, got %v", err)
	}
	writeServiceCRL(t, path, issuer, key, now.Add(time.Hour), nil)
	if err := verifyServiceRegistrationCRL(state, path, now); err == nil || !strings.Contains(err.Error(), "not current") {
		t.Fatalf("future CRL should be rejected, got %v", err)
	}
	writeServiceCRL(t, path, otherIssuer, otherKey, now, nil)
	if err := verifyServiceRegistrationCRL(state, path, now); err == nil || !strings.Contains(err.Error(), "verified issuer") {
		t.Fatalf("wrong issuer CRL should be rejected, got %v", err)
	}
	if err := os.WriteFile(path, []byte("not a CRL"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := serviceRegistrationTLSConfig(tls.Certificate{}, x509.NewCertPool(), path); err == nil {
		t.Fatal("malformed CRL must prevent listener startup")
	}
}

func testServiceCRLIssuer(t *testing.T, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial, NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		Subject:      pkix.Name{CommonName: "Internal Service CA"},
		SubjectKeyId: []byte{1, 2, 3, 4},
	}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func writeServiceCRL(t *testing.T, path string, issuer *x509.Certificate, key *ecdsa.PrivateKey, at time.Time, revoked []x509.RevocationListEntry) {
	t.Helper()
	raw, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number: big.NewInt(1), ThisUpdate: at.Add(-time.Minute), NextUpdate: at.Add(time.Hour), RevokedCertificateEntries: revoked,
	}, issuer, key)
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: raw})
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}
