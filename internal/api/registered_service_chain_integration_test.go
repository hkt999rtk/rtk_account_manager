package api

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"rtk_account_manager/internal/store"
)

func TestIntegrationRegisteredPluginReachesProductFactoryAndClaimGrant(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner := verifiedDeveloperForTest(t, env, "registered-plugin-owner@example.test")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, owner.UserID); err != nil {
		t.Fatal(err)
	}

	environment := "integration-registered-plugin-" + strconv.FormatInt(now.UnixNano(), 10)
	t.Cleanup(func() {
		for _, table := range []string{"platform_service_instances", "platform_service_workloads", "platform_services", "platform_service_manifests", "platform_service_registration_requests", "platform_service_catalog_revisions"} {
			if _, err := env.db.Exec(context.Background(), "DELETE FROM "+table+" WHERE environment=$1", environment); err != nil {
				t.Errorf("clean service registry fixture: %v", err)
			}
		}
	})
	env.store.ConfigurePlatformServiceProductWrites(environment, true)
	env.server.ConfigurePlatformServiceProductWrites(true)
	principal := store.PlatformServicePrincipal{Environment: environment, CertificateSubject: "service:mqtt", IssuerFingerprint: strings.Repeat("a", 64)}
	if err := env.store.ApprovePlatformServiceWorkload(ctx, store.PlatformServiceWorkloadApproval{
		Environment: environment, ServiceID: "mqtt", InstanceID: "mqtt-1", CertificateSubject: principal.CertificateSubject,
		IssuerFingerprint: principal.IssuerFingerprint, AllowedOptionCodes: []string{"mqtt"}, ApprovedBy: owner.UserID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.RegisterPlatformServiceInstance(ctx, store.PlatformServiceRegistration{
		RequestID: "mqtt-ready", ServiceID: "mqtt", InstanceID: "mqtt-1", ManifestVersion: "1", ProtocolVersion: "1", EndpointRef: "mqtt", Ready: true,
		Options: []store.PlatformServiceOption{{Code: "mqtt", DisplayName: "MQTT"}},
	}, principal, now); err != nil {
		t.Fatal(err)
	}

	principal.CertificateSubject = "service:test-plugin"
	if err := env.store.ApprovePlatformServiceWorkload(ctx, store.PlatformServiceWorkloadApproval{
		Environment: environment, ServiceID: "test-plugin", InstanceID: "plugin-1", CertificateSubject: principal.CertificateSubject,
		IssuerFingerprint: principal.IssuerFingerprint, AllowedOptionCodes: []string{"test_capability"}, ApprovedBy: owner.UserID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.RegisterPlatformServiceInstance(ctx, store.PlatformServiceRegistration{
		RequestID: "plugin-ready", ServiceID: "test-plugin", InstanceID: "plugin-1", ManifestVersion: "1", ProtocolVersion: "1", EndpointRef: "test-plugin", Ready: true,
		Options: []store.PlatformServiceOption{{Code: "test_capability", DisplayName: "Test capability", Requires: []string{"mqtt"}}},
	}, principal, now); err != nil {
		t.Fatal(err)
	}
	catalog, err := env.store.ListPlatformServiceOptions(ctx, environment, now)
	if err != nil || len(catalog.Options) != 2 || catalog.Options[1].Selectable {
		t.Fatalf("new plugin catalog = %+v, error=%v", catalog, err)
	}
	productPath := "/v1/orgs/" + owner.BrandCloudID + "/device-item-profiles"
	productBody := map[string]any{
		"profile_key": "test-plugin-product", "display_name": "Test plugin Product", "category": "ip_camera",
		"ca_profile": "fixture-ca", "issuer_profile": "fixture-issuer",
		"service_options": []string{"mqtt", "test_capability"}, "catalog_revision": catalog.CatalogRevision,
	}
	if r := performJSON(env.router, http.MethodPost, productPath, productBody, owner.AccessToken); r.Code != http.StatusConflict {
		t.Fatalf("suspended plugin Product = %d: %s", r.Code, r.Body.String())
	}
	if _, err := env.store.SetPlatformServiceStatus(ctx, environment, "test-plugin", "active", owner.UserID); err != nil {
		t.Fatal(err)
	}
	catalog, err = env.store.ListPlatformServiceOptions(ctx, environment, time.Now().UTC())
	if err != nil || len(catalog.Options) != 2 || !catalog.Options[1].Selectable {
		t.Fatalf("activated plugin catalog = %+v, error=%v", catalog, err)
	}
	productBody["catalog_revision"] = catalog.CatalogRevision
	created := performJSON(env.router, http.MethodPost, productPath, productBody, owner.AccessToken)
	if created.Code != http.StatusCreated {
		t.Fatalf("registered plugin Product = %d: %s", created.Code, created.Body.String())
	}
	product := decodeBody[deviceItemProfileBody](t, created).DeviceItemProfile
	if !slices.Equal(product.ServiceOptions, []string{"mqtt", "test_capability"}) {
		t.Fatalf("Product grant = %v", product.ServiceOptions)
	}
	readyDevicePKIFixture(t, env, product.ID)

	const jwtSecret = "registered-plugin-production-secret"
	env.server.ConfigureProductionJWT(jwtSecret, "factory-enroll")
	env.server.ConfigureFactoryEnrollmentToken(factoryCoordinationTestToken)
	runPath := productPath + "/" + product.ID + "/production-runs"
	issued := performJSON(env.router, http.MethodPost, runPath, map[string]any{
		"allowed_quantity": 1, "valid_from": now.Add(-time.Minute).Format(time.RFC3339),
		"valid_until": now.Add(time.Hour).Format(time.RFC3339),
	}, owner.AccessToken)
	if issued.Code != http.StatusCreated {
		t.Fatalf("registered plugin production run = %d: %s", issued.Code, issued.Body.String())
	}
	run := decodeBody[productionRunBody](t, issued)
	if run.ProductionRun.ProductServiceRevision == nil {
		t.Fatalf("production run omitted Product revision: %+v", run.ProductionRun)
	}
	claims := &productionJWTClaims{}
	parsed, err := jwt.ParseWithClaims(run.FactoryJWT, claims, func(*jwt.Token) (any, error) { return []byte(jwtSecret), nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience("factory-enroll"))
	var persistedDigest string
	if queryErr := env.db.QueryRow(ctx, `SELECT snapshot_sha256 FROM product_service_grants WHERE product_id=$1 AND revision=$2`,
		product.ID, *run.ProductionRun.ProductServiceRevision).Scan(&persistedDigest); queryErr != nil {
		t.Fatal(queryErr)
	}
	if err != nil || !parsed.Valid || claims.ProductServiceRevision == nil || claims.ServiceGrantSHA256 == "" ||
		!slices.Equal(claims.ServiceOptions, []string{"mqtt", "test_capability"}) ||
		claims.ServiceGrantSHA256 != persistedDigest {
		t.Fatalf("registered plugin production JWT = %+v, error=%v", claims, err)
	}
	reserved := performJSON(env.router, http.MethodPost, "/v1/internal/factory-enrollments/reserve", map[string]any{
		"production_run_id": run.ProductionRun.ID, "brand_cloud_id": owner.BrandCloudID,
		"device_item_profile_id": product.ID, "request_id": "plugin-factory-1", "devid": "plugin-device-1",
		"request_sha256": strings.Repeat("b", 64), "production_jwt": run.FactoryJWT,
	}, factoryCoordinationTestToken)
	if reserved.Code != http.StatusOK {
		t.Fatalf("registered plugin factory admission = %d: %s", reserved.Code, reserved.Body.String())
	}

	claimBody := map[string]any{
		"organization_id": owner.BrandCloudID, "device_item_profile_id": product.ID,
		"video_cloud_devid": "plugin-device-2", "activity_id": "plugin-activity", "clip_public_key": "plugin-key",
		"service_options": []string{"mqtt", "test_capability"}, "expires_at": now.Add(time.Hour).Format(time.RFC3339),
	}
	claim := performJSON(env.router, http.MethodPost, "/v1/admin/device-claim-tokens", claimBody, owner.AccessToken)
	if claim.Code != http.StatusCreated {
		t.Fatalf("registered plugin Claim Token = %d: %s", claim.Code, claim.Body.String())
	}
	claimToken := decodeBody[deviceClaimTokenAdminBody](t, claim).DeviceClaimToken
	if !slices.Equal(claimToken.ServiceOptions, []string{"mqtt", "test_capability"}) ||
		claimToken.Metadata["product_service_revision"] == nil || claimToken.Metadata["service_grant_sha256"] == nil {
		t.Fatalf("Product-bound Claim Token grant = %+v", claimToken)
	}
	claimBody["service_options"] = []string{"mqtt", "test_capability", "ungranted_option"}
	if r := performJSON(env.router, http.MethodPost, "/v1/admin/device-claim-tokens", claimBody, owner.AccessToken); r.Code != http.StatusBadRequest {
		t.Fatalf("Claim Token expanded Product grant = %d: %s", r.Code, r.Body.String())
	}
	delete(claimBody, "device_item_profile_id")
	claimBody["category"] = "ip_camera"
	claimBody["service_options"] = []string{"test_capability"}
	if r := performJSON(env.router, http.MethodPost, "/v1/admin/device-claim-tokens", claimBody, owner.AccessToken); r.Code != http.StatusBadRequest {
		t.Fatalf("legacy Claim Token accepted unbound option = %d: %s", r.Code, r.Body.String())
	}
}
