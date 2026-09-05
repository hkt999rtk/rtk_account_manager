package api

import (
	"context"
	"encoding/json"
	"rtk_account_manager/internal/store"
	"testing"
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
