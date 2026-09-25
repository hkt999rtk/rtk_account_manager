package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestOwnerTransferLimitAdminAPIAndDenial(t *testing.T) {
	env := newIntegrationEnv(t)
	admin := legacyCustomerForTest(t, env, "limit-admin@example.test", "Admin Org")
	if _, err := env.db.Exec(context.Background(), `UPDATE users SET platform_admin=true WHERE id=$1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	source := verifiedDeveloperForTest(t, env, "limit-api-source@example.test")
	verifiedDeveloperForTest(t, env, "limit-api-target@example.test")
	path := "/v1/admin/brand-clouds/" + source.BrandCloudID + "/owner-transfer-limit"
	if res := performJSON(env.router, http.MethodGet, path, nil, source.AccessToken); res.Code != 403 {
		t.Fatalf("non-admin read=%d %s", res.Code, res.Body.String())
	}
	if res := performJSON(env.router, http.MethodPatch, path, map[string]any{"owner_transfer_limit": 0}, source.AccessToken); res.Code != 403 {
		t.Fatalf("non-admin write=%d %s", res.Code, res.Body.String())
	}
	if res := performJSON(env.router, http.MethodGet, path, nil, admin.Tokens.AccessToken); res.Code != 200 || !strings.Contains(res.Body.String(), `"owner_transfer_remaining":3`) {
		t.Fatalf("default admin read=%d %s", res.Code, res.Body.String())
	}
	for _, value := range []any{-1, 201, 1.5, "3"} {
		if res := performJSON(env.router, http.MethodPatch, path, map[string]any{"owner_transfer_limit": value}, admin.Tokens.AccessToken); res.Code != 400 {
			t.Fatalf("invalid %v=%d %s", value, res.Code, res.Body.String())
		}
	}
	if res := performJSON(env.router, http.MethodPatch, path, map[string]any{"owner_transfer_limit": 0}, admin.Tokens.AccessToken); res.Code != 200 || !strings.Contains(res.Body.String(), `"owner_transfer_remaining":0`) {
		t.Fatalf("admin update=%d %s", res.Code, res.Body.String())
	}
	configureAPIHandoffFixture(t, env)
	res := performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds/"+source.BrandCloudID+"/owner-transfer", map[string]any{"target_email": "limit-api-target@example.test"}, source.AccessToken)
	if res.Code != 409 || !strings.Contains(res.Body.String(), "owner_transfer_limit_reached") {
		t.Fatalf("direct API bypass=%d %s", res.Code, res.Body.String())
	}
}
