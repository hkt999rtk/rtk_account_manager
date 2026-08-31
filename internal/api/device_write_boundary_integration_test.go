package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type admittedDeviceRequestBody struct {
	ctx     context.Context
	ready   chan struct{}
	release <-chan struct{}
	once    sync.Once
	reader  io.Reader
}

func (b *admittedDeviceRequestBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.ready) })
	select {
	case <-b.release:
		return b.reader.Read(p)
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
}

func TestIntegrationDeviceWritesReauthorizeAfterMiddlewareAdmission(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := verifiedDeveloperForTest(t, env, "device-http-owner@example.test")
	target := verifiedDeveloperForTest(t, env, "device-http-target@example.test")
	for _, raw := range []string{"http-existing-claim", "http-late-claim"} {
		if _, err := env.store.CreateDeviceClaimToken(ctx, store.DeviceClaimTokenCreateInput{
			OrganizationID: &owner.BrandCloudID, TokenHash: auth.HashToken(raw), Category: model.DeviceCategoryIPCamera,
			VideoCloudDevid: raw, ActivityID: "activity", ClipPublicKey: "key", ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := env.store.ResolveDeviceClaimToken(ctx, store.DeviceClaimResolveInput{
		OrganizationID: owner.BrandCloudID, RequestedBy: owner.UserID, TokenHash: auth.HashToken("http-existing-claim"), DeviceName: "Original",
	})
	if err != nil {
		t.Fatal(err)
	}
	d := claimed.Device
	base := "/v1/orgs/" + owner.BrandCloudID + "/devices"
	release := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 7)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, base, `{"name":"Late creation","category":"ip_camera"}`},
		{http.MethodPatch, base + "/" + d.ID, `{"name":"Late update","category":"ip_camera"}`},
		{http.MethodPatch, base + "/" + d.ID + "/status", `{"status":"online"}`},
		{http.MethodPost, base + "/" + d.ID + "/provision", `{"video_cloud_devid":"http-lifecycle-device","activity_id":"activity","clip_public_key":"key"}`},
		{http.MethodPost, base + "/" + d.ID + "/deactivate", `{"reason":"user_request"}`},
		{http.MethodPost, base + "/" + d.ID + "/unprovision", `{"reason":"user_request"}`},
		{http.MethodPost, base + "/claim/resolve", `{"claim_token":"http-late-claim","device_name":"Late claimed device"}`},
	} {
		body := &admittedDeviceRequestBody{ctx: ctx, ready: make(chan struct{}), release: release, reader: strings.NewReader(tc.body)}
		request := httptest.NewRequest(tc.method, tc.path, body).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+owner.AccessToken)
		go func() {
			response := httptest.NewRecorder()
			env.router.ServeHTTP(response, request)
			results <- response
		}()
		// Handler body binding happens only after auth/permission middleware.
		select {
		case <-body.ready:
		case response := <-results:
			t.Fatalf("request failed before admission: %d %s", response.Code, response.Body)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	tx, err := env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// Synthetic owner switch tests the HTTP authorization boundary; real Billing
	// prepare/commit/finalize is covered separately, not bypassed in production.
	if _, err := tx.Exec(ctx, `DELETE FROM organization_members WHERE organization_id=$1 AND user_id=$2`, owner.BrandCloudID, owner.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'owner')`, owner.BrandCloudID, target.UserID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	close(release)
	for range 7 {
		select {
		case response := <-results:
			if response.Code != http.StatusNotFound {
				t.Fatalf("stale admitted request wrote after owner switch: %d %s", response.Code, response.Body)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	got, err := env.store.GetDevice(ctx, owner.BrandCloudID, d.ID)
	if err != nil || got.Name != d.Name || got.Status != d.Status {
		t.Fatalf("stale request changed device: %+v %v", got, err)
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM devices WHERE organization_id=$1`, owner.BrandCloudID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("stale request created device: %d %v", count, err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM device_operations WHERE organization_id=$1`, owner.BrandCloudID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stale request queued lifecycle work: %d %v", count, err)
	}
}

func TestIntegrationPlatformUnprovisionRechecksRevokedPrivilege(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := verifiedDeveloperForTest(t, env, "override-http-owner@example.test")
	operator := verifiedDeveloperForTest(t, env, "override-http-operator@example.test")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, operator.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CreateDeviceClaimToken(ctx, store.DeviceClaimTokenCreateInput{
		OrganizationID: &owner.BrandCloudID, TokenHash: auth.HashToken("override-http-claim"), Category: model.DeviceCategoryIPCamera,
		VideoCloudDevid: "override-http-device", ActivityID: "activity", ClipPublicKey: "key", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := env.store.ResolveDeviceClaimToken(ctx, store.DeviceClaimResolveInput{OrganizationID: owner.BrandCloudID, RequestedBy: owner.UserID, TokenHash: auth.HashToken("override-http-claim")})
	if err != nil {
		t.Fatal(err)
	}
	target, err := env.store.CreateOrganization(ctx, operator.UserID, "Legacy support recovery")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	result := make(chan *httptest.ResponseRecorder, 3)
	for _, tc := range []struct{ path, body string }{
		{"/v1/admin/devices/" + claimed.Device.ID + "/unprovision", `{"reason":"support verified","evidence":{"ticket":"isolated-test"}}`},
		{"/v1/admin/device-claims/" + claimed.Claim.ID + "/transfer", `{"target_organization_id":"` + target.ID + `","reason":"support verified","evidence":{"ticket":"isolated-test"}}`},
		{"/v1/admin/device-claim-tokens/" + claimed.Claim.TokenID + "/reclaim", `{"target_organization_id":"` + target.ID + `","reason":"support verified","evidence":{"ticket":"isolated-test"}}`},
	} {
		body := &admittedDeviceRequestBody{ctx: ctx, ready: make(chan struct{}), release: release, reader: strings.NewReader(tc.body)}
		req := httptest.NewRequest(http.MethodPost, tc.path, body).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+operator.AccessToken)
		go func() { r := httptest.NewRecorder(); env.router.ServeHTTP(r, req); result <- r }()
		select {
		case <-body.ready:
		case r := <-result:
			t.Fatalf("request failed before admission: %d %s", r.Code, r.Body)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=false WHERE id=$1`, operator.UserID); err != nil {
		t.Fatal(err)
	}
	close(release)
	for range 3 {
		select {
		case r := <-result:
			if r.Code != http.StatusNotFound {
				t.Fatalf("revoked platform authority mutated: %d %s", r.Code, r.Body)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if _, err := env.store.GetDevice(ctx, owner.BrandCloudID, claimed.Device.ID); err != nil {
		t.Fatal("revoked operator removed device", err)
	}
	var queued int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM device_operations WHERE organization_id=$1`, owner.BrandCloudID).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("revoked operator queued work: %d %v", queued, err)
	}
	var claimCloud, tokenCloud, status string
	if err := env.db.QueryRow(ctx, `SELECT c.organization_id::text,t.organization_id::text,c.status FROM device_claims c JOIN device_claim_tokens t ON t.id=c.claim_token_id WHERE c.id=$1`, claimed.Claim.ID).Scan(&claimCloud, &tokenCloud, &status); err != nil || claimCloud != owner.BrandCloudID || tokenCloud != owner.BrandCloudID || status != "resolved" {
		t.Fatalf("revoked operator changed claim: %s/%s/%s %v", claimCloud, tokenCloud, status, err)
	}
}
