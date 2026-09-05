package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
	"strings"
	"testing"
	"time"
)

func TestLabConsoleIdentityAPI(t *testing.T) {
	t.Setenv("TEST_LAB_ENABLED", "true")
	t.Setenv("ACCOUNT_MANAGER_ENV", "dev")
	env := newIntegrationEnv(t)
	ctx := context.Background()
	env.server.ConfigureTestLab(env.store, "http://runtime.invalid", "fixture-token")
	owner := verifiedDeveloperForTest(t, env, "lab-console@example.test")
	other := verifiedDeveloperForTest(t, env, "lab-other@example.test")
	existing, err := env.store.CreateEndUser(ctx, store.EndUserCreateInput{Email: "lab-console@example.test", PasswordHash: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/developer/brand-clouds/" + owner.BrandCloudID + "/test-lab/accounts"
	r := performJSON(env.router, "POST", path, map[string]any{}, owner.AccessToken)
	if r.Code != 200 {
		t.Fatalf("Console bootstrap: %d %s", r.Code, r.Body)
	}
	var a, b store.LabAccount
	if err = json.Unmarshal(r.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if a.EndUserID == existing.ID || a.Email != "lab-console@example.test" {
		t.Fatal("App account adopted by email or wrong display identity")
	}
	r = performJSON(env.router, "POST", path, map[string]any{}, owner.AccessToken)
	if r.Code != 200 {
		t.Fatal(r.Code)
	}
	if err = json.Unmarshal(r.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || a.EndUserID != b.EndUserID {
		t.Fatal("Reload changed identity or delegation")
	}
	for _, token := range []string{"", other.AccessToken} {
		if r = performJSON(env.router, "POST", path, map[string]any{}, token); r.Code < 400 {
			t.Fatal("Foreign or anonymous identity admitted")
		}
	}
	if r = performJSON(env.router, "POST", path, map[string]any{"email": "arbitrary@example.test", "password": "not-accepted"}, owner.AccessToken); r.Code != 400 {
		t.Fatal("Old password/impersonation input accepted", r.Code)
	}
	t.Setenv("ACCOUNT_MANAGER_ENV", "production")
	if r = performJSON(env.router, "POST", path, map[string]any{}, owner.AccessToken); r.Code != 404 {
		t.Fatal("Production bootstrap enabled")
	}
	t.Setenv("ACCOUNT_MANAGER_ENV", "dev")
	if _, err = env.db.Exec(ctx, `UPDATE organization_members SET disabled_at=now() WHERE organization_id=$1 AND user_id=$2`, owner.BrandCloudID, owner.UserID); err != nil {
		t.Fatal(err)
	}
	if r = performJSON(env.router, "POST", path, map[string]any{}, owner.AccessToken); r.Code < 400 {
		t.Fatal("Revoked developer admitted")
	}
}

func TestLabDeviceAndSessionAPIHappyPath(t *testing.T) {
	t.Setenv("TEST_LAB_ENABLED", "true")
	t.Setenv("ACCOUNT_MANAGER_ENV", "dev")
	env := newIntegrationEnv(t)
	ctx := context.Background()

	runtimeCalls := 0
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtimeCalls++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/internal/account-manager/test-lab/token" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("unexpected runtime request: %s %s %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"runtime-token","scope":"app","mqtt":{},"aws_credentials":{}}`))
	}))
	t.Cleanup(runtime.Close)
	env.server.ConfigureTestLab(env.store, runtime.URL, "fixture-token")

	owner := verifiedDeveloperForTest(t, env, "lab-api@example.test")
	product, err := env.store.CreateDeviceItemProfileAsUser(ctx, store.DeviceItemProfileCreateInput{
		ActorUserID:    &owner.UserID,
		BrandCloudID:   owner.BrandCloudID,
		ProfileKey:     "lab-api-product",
		DisplayName:    "Lab API Product",
		Category:       model.DeviceCategoryIPCamera,
		CAProfile:      "ca",
		IssuerProfile:  "issuer",
		ServiceOptions: []string{"mqtt", "iot_shadow", "video_streaming"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run, _, err := env.store.IssueProductionRunAsUser(ctx, store.ProductionRunCreateInput{
		ActorUserID:         &owner.UserID,
		BrandCloudID:        owner.BrandCloudID,
		DeviceItemProfileID: product.ID,
		FactoryID:           "developer-console",
		BatchID:             "pki-test-api",
		AllowedQuantity:     1,
		ValidFrom:           now.Add(-time.Minute),
		ValidUntil:          now.Add(time.Hour),
	}, func(model.ProductionRun, model.DeviceItemProfile) (string, error) { return "fixture", nil })
	if err != nil {
		t.Fatal(err)
	}
	device, err := env.store.CreateDeviceAsUser(ctx, owner.UserID, owner.BrandCloudID, store.DeviceInput{
		Name:                "Lab API camera",
		Category:            product.Category,
		DeviceItemProfileID: &product.ID,
		Metadata:            map[string]any{"purpose": "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	admission := store.FactoryEnrollmentAdmission{
		RunID: run.ID, CloudID: owner.BrandCloudID, ProductID: product.ID,
		RequestID: "lab-api-issuance", DeviceID: device.ID, RequestSHA256: strings.Repeat("a", 64),
	}
	reservation, err := env.store.ReserveFactoryEnrollment(ctx, admission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.store.CompleteFactoryEnrollment(ctx, store.FactoryEnrollmentResult{
		CloudID: owner.BrandCloudID, RunID: run.ID, ReservationID: reservation.ID,
		RequestSHA256: admission.RequestSHA256, Status: "issued", EvidenceSHA256: strings.Repeat("b", 64),
	}); err != nil {
		t.Fatal(err)
	}

	base := "/v1/developer/brand-clouds/" + owner.BrandCloudID + "/test-lab"
	r := performJSON(env.router, http.MethodPost, base+"/accounts", map[string]any{}, owner.AccessToken)
	if r.Code != http.StatusOK {
		t.Fatalf("create account: %d %s", r.Code, r.Body)
	}
	var account store.LabAccount
	if err = json.Unmarshal(r.Body.Bytes(), &account); err != nil {
		t.Fatal(err)
	}

	devicePath := base + "/devices/" + device.ID
	scope := map[string]any{"product_id": product.ID, "account_id": account.ID}
	if r = performJSON(env.router, http.MethodGet, base+"/devices", nil, owner.AccessToken); r.Code != http.StatusBadRequest {
		t.Fatalf("unscoped device list: %d %s", r.Code, r.Body)
	}
	if r = performJSON(env.router, http.MethodPost, devicePath+"/unknown", scope, owner.AccessToken); r.Code != http.StatusNotFound {
		t.Fatalf("unknown device action: %d %s", r.Code, r.Body)
	}
	if r = performJSON(env.router, http.MethodPost, devicePath+"/bind", scope, owner.AccessToken); r.Code != http.StatusBadRequest {
		t.Fatalf("bind without claim: %d %s", r.Code, r.Body)
	}
	invalidProvision := map[string]any{
		"product_id": product.ID, "account_id": account.ID,
		"operation_id": "lab-api-operation", "activity_id": "lab-api-activity", "clip_public_key": "not-pem",
	}
	if r = performJSON(env.router, http.MethodPost, devicePath+"/provision", invalidProvision, owner.AccessToken); r.Code != http.StatusBadRequest {
		t.Fatalf("invalid provision key: %d %s", r.Code, r.Body)
	}
	r = performJSON(env.router, http.MethodPost, devicePath+"/grant", scope, owner.AccessToken)
	if r.Code != http.StatusCreated {
		t.Fatalf("grant: %d %s", r.Code, r.Body)
	}
	var grant struct {
		ClaimToken string `json:"claim_token"`
	}
	if err = json.Unmarshal(r.Body.Bytes(), &grant); err != nil || len(grant.ClaimToken) != 64 {
		t.Fatalf("grant response: %q %v", grant.ClaimToken, err)
	}
	scope["claim_token"] = grant.ClaimToken
	r = performJSON(env.router, http.MethodPost, devicePath+"/bind", scope, owner.AccessToken)
	if r.Code != http.StatusOK {
		t.Fatalf("bind: %d %s", r.Code, r.Body)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	provision := map[string]any{
		"product_id": product.ID, "account_id": account.ID,
		"operation_id": "lab-api-operation", "activity_id": "lab-api-activity",
		"clip_public_key": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
	}
	r = performJSON(env.router, http.MethodPost, devicePath+"/provision", provision, owner.AccessToken)
	if r.Code != http.StatusAccepted {
		t.Fatalf("provision: %d %s", r.Code, r.Body)
	}
	if _, err = env.db.Exec(ctx, `UPDATE devices SET metadata=metadata||'{"video_cloud_activation_status":"activated"}'::jsonb WHERE id=$1`, device.ID); err != nil {
		t.Fatal(err)
	}

	query := "?product_id=" + product.ID + "&account_id=" + account.ID
	r = performJSON(env.router, http.MethodGet, base+"/devices"+query, nil, owner.AccessToken)
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), device.ID) {
		t.Fatalf("list devices: %d %s", r.Code, r.Body)
	}
	r = performJSON(env.router, http.MethodGet, devicePath+query, nil, owner.AccessToken)
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"runtime_ready":true`) {
		t.Fatalf("device readiness: %d %s", r.Code, r.Body)
	}

	r = performJSON(env.router, http.MethodPost, base+"/sessions", map[string]any{
		"product_id": product.ID, "device_id": device.ID, "account_id": account.ID,
	}, owner.AccessToken)
	if r.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", r.Code, r.Body)
	}
	var session store.TestLabSession
	if err = json.Unmarshal(r.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	r = performJSON(env.router, http.MethodPost, base+"/sessions/"+session.ID+"/credentials", map[string]any{}, owner.AccessToken)
	if r.Code != http.StatusOK || runtimeCalls != 1 || !strings.Contains(r.Body.String(), "runtime-token") {
		t.Fatalf("credentials: %d calls=%d %s", r.Code, runtimeCalls, r.Body)
	}
	r = performJSON(env.router, http.MethodDelete, base+"/sessions/"+session.ID, nil, owner.AccessToken)
	if r.Code != http.StatusNoContent {
		t.Fatalf("close session: %d %s", r.Code, r.Body)
	}
	r = performJSON(env.router, http.MethodPost, base+"/sessions/"+session.ID+"/credentials", map[string]any{}, owner.AccessToken)
	if r.Code != http.StatusNotFound || runtimeCalls != 1 {
		t.Fatalf("revoked session credentials: %d calls=%d %s", r.Code, runtimeCalls, r.Body)
	}
	r = performJSON(env.router, http.MethodPost, devicePath+"/unbind", scope, owner.AccessToken)
	if r.Code != http.StatusOK {
		t.Fatalf("unbind: %d %s", r.Code, r.Body)
	}
	r = performJSON(env.router, http.MethodDelete, base+"/accounts/"+account.ID, nil, owner.AccessToken)
	if r.Code != http.StatusNoContent {
		t.Fatalf("close account: %d %s", r.Code, r.Body)
	}
}
