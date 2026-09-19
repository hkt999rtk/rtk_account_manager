package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
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

	"rtk_account_manager/internal/api"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
	"rtk_account_manager/internal/testutil"
)

// Exercises the real registry persistence behind its dedicated, CRL-checked
// mTLS listener. The TLS-only tests use a stub, and the store tests bypass HTTP.
func TestServiceRegistrationMTLSDrivesCatalogProductAndRun(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	testutil.LockIntegrationDatabase(t, db)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	unique := strconv.FormatInt(time.Now().UnixNano(), 10)
	environment := "integration-service-mtls-" + unique
	repository := store.New(db)
	var actorID string
	if err := db.QueryRow(ctx, `INSERT INTO users(email,password_hash) VALUES($1,'test-hash') RETURNING id::text`,
		"service-mtls-"+unique+"@example.com").Scan(&actorID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"platform_service_instances", "platform_service_workloads", "platform_services", "platform_service_manifests", "platform_service_registration_requests", "platform_service_catalog_revisions"} {
			if _, err := db.Exec(context.Background(), "DELETE FROM "+table+" WHERE environment=$1", environment); err != nil {
				t.Errorf("clean up %s: %v", table, err)
			}
		}
		if _, err := db.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, actorID); err != nil {
			t.Errorf("clean up user: %v", err)
		}
	})

	issuer, issuerKey := testServiceCRLIssuer(t, now)
	serverCert := issueServiceTLSLeaf(t, issuer, issuerKey, now, 10, "platform-register", true)
	mqttCert := issueServiceTLSLeaf(t, issuer, issuerKey, now, 41, "service:mqtt", false)
	shadowCert := issueServiceTLSLeaf(t, issuer, issuerKey, now, 42, "service:shadow", false)
	unapprovedCert := issueServiceTLSLeaf(t, issuer, issuerKey, now, 43, "service:other", false)
	roots := x509.NewCertPool()
	roots.AddCert(issuer)
	crlPath := filepath.Join(t.TempDir(), "service.crl")
	writeServiceCRL(t, crlPath, issuer, issuerKey, now, nil)
	tlsConfig, err := serviceRegistrationTLSConfig(serverCert, roots, crlPath)
	if err != nil {
		t.Fatal(err)
	}
	platform := api.New(nil, nil)
	platform.ConfigurePlatformServices(repository, environment)
	listener := httptest.NewUnstartedServer(serviceRegistrationCRLMiddleware(platform.ServiceRouter(), crlPath))
	listener.TLS = tlsConfig
	listener.StartTLS()
	defer listener.Close()
	client := func(cert tls.Certificate) *http.Client {
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}}
		t.Cleanup(transport.CloseIdleConnections)
		return &http.Client{Transport: transport}
	}
	request := func(httpClient *http.Client, method, serviceID, instanceID, suffix, body string, want int) {
		t.Helper()
		url := listener.URL + "/v1/platform/services/" + serviceID + "/instances/" + instanceID + suffix
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		payload, _ := io.ReadAll(response.Body)
		if response.StatusCode != want {
			t.Fatalf("%s %s status=%d, want %d: %s", serviceID, method, response.StatusCode, want, payload)
		}
	}
	fingerprint := sha256.Sum256(issuer.Raw)
	approve := func(serviceID, instanceID, subject, option string) {
		t.Helper()
		if err := repository.ApprovePlatformServiceWorkload(ctx, store.PlatformServiceWorkloadApproval{
			Environment: environment, ServiceID: serviceID, InstanceID: instanceID, CertificateSubject: subject,
			IssuerFingerprint: hex.EncodeToString(fingerprint[:]), AllowedOptionCodes: []string{option}, ApprovedBy: actorID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	approve("mqtt", "mqtt-1", "service:mqtt", "mqtt")
	mqttBody := `{"request_id":"mqtt-1","service_id":"mqtt","instance_id":"mqtt-1","manifest_version":"1","protocol_version":"1","endpoint_ref":"mqtt","ready":false,"options":[{"code":"mqtt","display_name":"MQTT"}]}`
	request(client(unapprovedCert), http.MethodPut, "mqtt", "mqtt-1", "", mqttBody, http.StatusForbidden)
	mqttClient := client(mqttCert)
	request(mqttClient, http.MethodPut, "mqtt", "mqtt-1", "", mqttBody, http.StatusOK)
	catalog, err := repository.ListPlatformServiceOptions(ctx, environment, time.Now().UTC())
	if err != nil || len(catalog.Options) != 1 || catalog.Options[0].Selectable {
		t.Fatalf("unready MQTT catalog = %+v, error=%v", catalog, err)
	}
	request(mqttClient, http.MethodPost, "mqtt", "mqtt-1", "/heartbeat", `{"manifest_version":"1","ready":true}`, http.StatusOK)

	approve("shadow", "shadow-1", "service:shadow", "iot_shadow")
	if videoCloudRoot := os.Getenv("VIDEO_CLOUD_SOURCE_ROOT"); videoCloudRoot != "" {
		// Optional cross-module path: use the production Video Cloud registrar
		// implementation in another process against this real Platform listener.
		key, ok := shadowCert.PrivateKey.(*ecdsa.PrivateKey)
		if !ok {
			t.Fatal("test certificate has no ECDSA private key")
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		certPath, keyPath, caPath := filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key"), filepath.Join(dir, "server-ca.crt")
		for path, content := range map[string][]byte{
			certPath: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: shadowCert.Certificate[0]}),
			keyPath:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
			caPath:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuer.Raw}),
		} {
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		probe := exec.CommandContext(probeCtx, "go", "run", "./internal/serviceregistration/testprobe", "-url", listener.URL, "-cert", certPath, "-key", keyPath, "-ca", caPath)
		probe.Dir = videoCloudRoot
		probe.Env = append(os.Environ(), "GOWORK=off")
		if output, err := probe.CombinedOutput(); err != nil {
			t.Fatalf("Video Cloud registrar probe: %v: %s", err, output)
		}
	} else {
		shadowBody := `{"request_id":"shadow-1","service_id":"shadow","instance_id":"shadow-1","manifest_version":"1","protocol_version":"1","endpoint_ref":"shadow","ready":false,"options":[{"code":"iot_shadow","display_name":"IoT Shadow","requires":["mqtt"]}]}`
		shadowClient := client(shadowCert)
		request(shadowClient, http.MethodPut, "shadow", "shadow-1", "", shadowBody, http.StatusOK)
		request(shadowClient, http.MethodPost, "shadow", "shadow-1", "/heartbeat", `{"manifest_version":"1","ready":true}`, http.StatusOK)
	}
	catalog, err = repository.ListPlatformServiceOptions(ctx, environment, time.Now().UTC())
	if err != nil || len(catalog.Options) != 2 || !catalog.Options[0].Selectable || catalog.Options[1].Selectable || catalog.Options[1].UnavailableReason != "service_suspended" {
		t.Fatalf("mTLS registered catalog = %+v, error=%v", catalog, err)
	}
	owner, err := repository.SignupDeveloper(ctx, store.DeveloperSignupInput{
		Email: "service-product-" + unique + "@example.com", PasswordHash: "test-hash", EmailVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The signup's billing-creation outbox entry is immutable, so its
		// Brand Cloud and owner must remain a consistent test-DB fixture.
		// Remove only the Product/run records this test adds to that cloud.
		for _, query := range []string{
			`DELETE FROM factory_production_runs WHERE brand_cloud_id=$1`,
			`DELETE FROM product_service_grants WHERE brand_cloud_id=$1`,
			`DELETE FROM device_item_profiles WHERE brand_cloud_id=$1`,
		} {
			if _, err := db.Exec(context.Background(), query, owner.BrandCloud.ID); err != nil {
				t.Errorf("clean up Product fixture: %v", err)
			}
		}
	})
	repository.ConfigurePlatformServiceProductWrites(environment, true)
	productInput := store.DeviceItemProfileCreateInput{
		ActorUserID: &owner.User.ID, BrandCloudID: owner.BrandCloud.ID,
		ProfileKey: "registered-shadow", DisplayName: "Registered Shadow Product",
		Category: model.DeviceCategoryIPCamera, CAProfile: "fixture-ca", IssuerProfile: "fixture-issuer",
		ServiceOptions: []string{"mqtt", "iot_shadow"}, CatalogRevision: catalog.CatalogRevision,
	}
	if _, err := repository.CreateDeviceItemProfileAsUser(ctx, productInput); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("suspended mTLS plugin selected by Product: %v", err)
	}
	if _, err := repository.SetPlatformServiceStatus(ctx, environment, "shadow", "active", actorID); err != nil {
		t.Fatal(err)
	}
	catalog, err = repository.ListPlatformServiceOptions(ctx, environment, time.Now().UTC())
	if err != nil || len(catalog.Options) != 2 || !catalog.Options[1].Selectable {
		t.Fatalf("activated catalog = %+v, error=%v", catalog, err)
	}
	if _, err := repository.CreateDeviceItemProfileAsUser(ctx, productInput); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale catalog revision selected plugin: %v", err)
	}
	productInput.CatalogRevision = catalog.CatalogRevision
	product, err := repository.CreateDeviceItemProfileAsUser(ctx, productInput)
	if err != nil || !slices.Equal(product.ServiceOptions, []string{"mqtt", "iot_shadow"}) {
		t.Fatalf("registered service Product = %+v, error=%v", product, err)
	}
	var signedOptions []string
	run, token, err := repository.IssueProductionRunAsUser(ctx, store.ProductionRunCreateInput{
		ActorUserID: &owner.User.ID, BrandCloudID: owner.BrandCloud.ID,
		DeviceItemProfileID: product.ID, AllowedQuantity: 1,
		ValidFrom: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour),
	}, func(_ model.ProductionRun, p model.DeviceItemProfile) (string, error) {
		signedOptions = slices.Clone(p.ServiceOptions)
		return "fixture-production-token", nil
	})
	if err != nil || token == "" || run.ProductServiceRevision == nil || run.ServiceGrantSHA256 == "" || !slices.Equal(signedOptions, []string{"iot_shadow", "mqtt"}) {
		t.Fatalf("mTLS registration Product/run chain = %+v, token=%q, options=%v, error=%v", run, token, signedOptions, err)
	}
}
