package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"rtk_account_manager/internal/store"
)

// This joins mTLS service registration and a registered Product grant to a
// separately compiled Video Cloud factory application and device-token handler.
func TestIntegrationRegisteredPluginAcrossFactoryApplication(t *testing.T) {
	binary, videoDSN := os.Getenv("TEST_FACTORY_APPLICATION_BINARY"), os.Getenv("TEST_FACTORY_APPLICATION_DSN")
	if binary == "" || videoDSN == "" || os.Getenv("TEST_DATABASE_URL") == "" {
		if os.Getenv("TEST_REGISTERED_SERVICE_CHAIN_REQUIRED") == "1" {
			t.Fatal("required registered-service chain is missing its factory fixture or PostgreSQL DSN")
		}
		t.Skip("requires independently compiled Video Cloud factory application and isolated PostgreSQL DSN")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("factory application binary must be an absolute path")
	}
	env := newIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	unique := strconv.FormatInt(now.UnixNano(), 10)
	owner := verifiedDeveloperForTest(t, env, "registered-factory-"+unique+"@example.test")
	environment := "registered-factory-" + unique
	t.Cleanup(func() {
		for _, table := range []string{"platform_service_instances", "platform_service_workloads", "platform_services", "platform_service_manifests", "platform_service_registration_requests", "platform_service_catalog_revisions"} {
			if _, err := env.db.Exec(context.Background(), "DELETE FROM "+table+" WHERE environment=$1", environment); err != nil {
				t.Errorf("clean service registry fixture: %v", err)
			}
		}
	})
	env.store.ConfigurePlatformServiceProductWrites(environment, true)
	env.server.ConfigurePlatformServices(env.store, environment)
	env.server.ConfigurePlatformServiceProductWrites(true)
	issuer, issuerKey := registeredFactoryServiceIssuer(t, now)
	roots := x509.NewCertPool()
	roots.AddCert(issuer)
	listener := httptest.NewUnstartedServer(env.server.ServiceRouter())
	listener.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}
	listener.StartTLS()
	defer listener.Close()
	issuerHash := sha256.Sum256(issuer.Raw)
	issuerFingerprint := hex.EncodeToString(issuerHash[:])
	register := func(serviceID, optionCode string, requires []string, serial int64) {
		t.Helper()
		instanceID := serviceID + "-1"
		certificateSubject := "service:" + serviceID
		if err := env.store.ApprovePlatformServiceWorkload(ctx, store.PlatformServiceWorkloadApproval{
			Environment: environment, ServiceID: serviceID, InstanceID: instanceID, CertificateSubject: certificateSubject,
			IssuerFingerprint: issuerFingerprint, AllowedOptionCodes: []string{optionCode}, ApprovedBy: owner.UserID,
		}); err != nil {
			t.Fatal(err)
		}
		transport := listener.Client().Transport.(*http.Transport).Clone()
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		transport.TLSClientConfig.Certificates = []tls.Certificate{registeredFactoryServiceCert(t, issuer, issuerKey, now, certificateSubject, serial)}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport}
		send := func(method, suffix string, body any) {
			t.Helper()
			payload, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequest(method, listener.URL+"/v1/platform/services/"+serviceID+"/instances/"+instanceID+suffix, bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			response, err := client.Do(request)
			if err != nil {
				t.Fatal("service registration mTLS request", err)
			}
			responseBody, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("service %s %s registration = %d: %s", serviceID, method, response.StatusCode, responseBody)
			}
		}
		send(http.MethodPut, "", store.PlatformServiceRegistration{
			RequestID: serviceID + "-register", ServiceID: serviceID, InstanceID: instanceID, ManifestVersion: "1", ProtocolVersion: "1",
			EndpointRef: serviceID, Ready: false, Options: []store.PlatformServiceOption{{Code: optionCode, DisplayName: optionCode, Requires: requires}},
		})
		send(http.MethodPost, "/heartbeat", map[string]any{"manifest_version": "1", "ready": true})
	}
	register("mqtt", "mqtt", nil, 2)
	register("test-plugin", "test_capability", []string{"mqtt"}, 3)
	if _, err := env.store.SetPlatformServiceStatus(ctx, environment, "test-plugin", "active", owner.UserID); err != nil {
		t.Fatal(err)
	}
	catalog, err := env.store.ListPlatformServiceOptions(ctx, environment, time.Now().UTC())
	if err != nil || len(catalog.Options) != 2 || !catalog.Options[1].Selectable {
		t.Fatalf("registered catalog = %+v, error=%v", catalog, err)
	}
	options := []string{"mqtt", "test_capability"}
	productPath := "/v1/orgs/" + owner.BrandCloudID + "/device-item-profiles"
	created := performJSON(env.router, http.MethodPost, productPath, map[string]any{
		"profile_key": "registered-factory", "display_name": "Registered factory Product", "category": "ip_camera",
		"ca_profile": "fixture-ca", "issuer_profile": "fixture-issuer", "service_options": options, "catalog_revision": catalog.CatalogRevision,
	}, owner.AccessToken)
	if created.Code != http.StatusCreated {
		t.Fatalf("registered Product = %d: %s", created.Code, created.Body.String())
	}
	product := decodeBody[deviceItemProfileBody](t, created).DeviceItemProfile
	if !slices.Equal(product.ServiceOptions, options) {
		t.Fatalf("Product options = %v", product.ServiceOptions)
	}
	const productionSecret = "isolated-registered-factory-production-secret"
	env.server.ConfigureProductionJWT(productionSecret, "factory-enroll")
	env.server.ConfigureFactoryEnrollmentToken(factoryCoordinationTestToken)
	issued := performJSON(env.router, http.MethodPost, productPath+"/"+product.ID+"/production-runs", map[string]any{
		"allowed_quantity": 1, "valid_from": now.Add(-time.Minute).Format(time.RFC3339), "valid_until": now.Add(time.Hour).Format(time.RFC3339),
	}, owner.AccessToken)
	if issued.Code != http.StatusCreated {
		t.Fatalf("production run = %d: %s", issued.Code, issued.Body.String())
	}
	run := decodeBody[productionRunBody](t, issued)
	var grantDigest string
	if run.ProductionRun.ProductServiceRevision == nil {
		t.Fatal("production run omitted Product service revision")
	}
	if err := env.db.QueryRow(ctx, `SELECT snapshot_sha256 FROM product_service_grants WHERE product_id=$1 AND revision=$2`, product.ID, *run.ProductionRun.ProductServiceRevision).Scan(&grantDigest); err != nil {
		t.Fatal(err)
	}
	claims := &productionJWTClaims{}
	parsed, err := jwt.ParseWithClaims(run.FactoryJWT, claims, func(*jwt.Token) (any, error) { return []byte(productionSecret), nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience("factory-enroll"))
	if err != nil || !parsed.Valid || !slices.Equal(claims.ServiceOptions, options) || claims.ServiceGrantSHA256 != grantDigest {
		t.Fatalf("production JWT grant = %+v, error=%v", claims, err)
	}

	accountHTTP := httptest.NewServer(env.router)
	defer accountHTTP.Close()
	childCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(childCtx, binary, "-test.run=^TestFactoryApplicationWithExternalAccountManager$", "-test.timeout=50s")
	cmd.Env = append(os.Environ(), "TEST_FACTORY_EXTERNAL_AM=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	defer func() {
		if !finished {
			cancel()
			_ = stdin.Close()
			_ = cmd.Wait()
		}
	}()
	encoder, decoder := json.NewEncoder(stdin), json.NewDecoder(stdout)
	if err := encoder.Encode(map[string]any{
		"DSN": videoDSN, "AMURL": accountHTTP.URL, "AMToken": factoryCoordinationTestToken,
		"ProductionJWTSecret": productionSecret, "ProductionJWT": run.FactoryJWT,
		"RunID": run.ProductionRun.ID, "CloudID": owner.BrandCloudID, "ProductID": product.ID, "ServiceOptions": options,
	}); err != nil {
		t.Fatal(err)
	}
	var ready struct {
		Ready               bool
		DeviceID, RequestID string
	}
	if err := decoder.Decode(&ready); err != nil || !ready.Ready || ready.DeviceID == "" {
		t.Fatalf("factory application readiness: %+v, error=%v, stderr=%s", ready, err, stderr.String())
	}
	type witness struct {
		Code                                                               int
		ErrorCode, CertificateSHA256                                       string
		JournalStatus, ReservationID                                       string
		SignatureCount                                                     int
		EntitlementOptions                                                 []string
		ProductServiceRevision, ServiceGrantSHA256                         string
		TokenStatus, TokenProductServiceRevision, TokenEntitlementRevision int
		TokenOptions                                                       []string
		TokenSignatureVerified                                             bool
	}
	call := func(command map[string]any) witness {
		t.Helper()
		if err := encoder.Encode(command); err != nil {
			t.Fatal(err)
		}
		var out witness
		if err := decoder.Decode(&out); err != nil {
			t.Fatalf("read factory witness: %v, stderr=%s", err, stderr.String())
		}
		return out
	}
	if widened := call(map[string]any{"Action": "enroll", "ServiceOptions": []string{"mqtt", "test_capability", "ungranted_option"}}); widened.Code != http.StatusForbidden || widened.CertificateSHA256 != "" || widened.SignatureCount != 0 {
		t.Fatalf("factory accepted expanded option echo: %+v", widened)
	}
	valid := call(map[string]any{"Action": "enroll"})
	if valid.Code != http.StatusOK || valid.CertificateSHA256 == "" || valid.JournalStatus != "completed" || valid.SignatureCount != 1 ||
		!slices.Equal(valid.EntitlementOptions, options) || valid.ProductServiceRevision != strconv.FormatInt(*run.ProductionRun.ProductServiceRevision, 10) || valid.ServiceGrantSHA256 != grantDigest ||
		valid.TokenStatus != http.StatusOK || !valid.TokenSignatureVerified || !slices.Equal(valid.TokenOptions, options) ||
		valid.TokenProductServiceRevision != int(*run.ProductionRun.ProductServiceRevision) || valid.TokenEntitlementRevision < 1 {
		t.Fatalf("registered option did not reach durable factory grant: %+v", valid)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("factory application exit: %v, stderr=%s", err, stderr.String())
	}
	finished = true
	var issuedCount int
	if err := env.db.QueryRow(ctx, `SELECT issued_quantity FROM factory_production_runs WHERE id=$1`, run.ProductionRun.ID).Scan(&issuedCount); err != nil || issuedCount != 1 {
		t.Fatalf("Account Manager issuance count = %d, error=%v", issuedCount, err)
	}
	if strings.TrimSpace(valid.ReservationID) == "" {
		t.Fatal("factory result omitted Account Manager reservation")
	}
}

func registeredFactoryServiceIssuer(t *testing.T, now time.Time) (*x509.Certificate, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "registered-factory-test-service-issuer"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	encoded, err := x509.CreateCertificate(rand.Reader, certificate, certificate, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := x509.ParseCertificate(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return issuer, privateKey
}

func registeredFactoryServiceCert(t *testing.T, issuer *x509.Certificate, issuerKey ed25519.PrivateKey, now time.Time, subject string, serial int64) tls.Certificate {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: subject},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	encoded, err := x509.CreateCertificate(rand.Reader, certificate, issuer, publicKey, issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{encoded, issuer.Raw}, PrivateKey: privateKey}
}
