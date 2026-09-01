package api

import (
	"net/http"
	"rtk_account_manager/internal/store"
	"testing"
)

func TestIntegrationCloudDeletionPreflightDefaultIsReadOnlyAndFailClosed(t *testing.T) {
	env := newIntegrationEnv(t)
	owner := verifiedDeveloperForTest(t, env, "delete-api-owner@example.com")
	other := verifiedDeveloperForTest(t, env, "delete-api-other@example.com")
	path := "/v1/developer/brand-clouds/" + owner.BrandCloudID + "/deletion-preflight"
	result := performJSON(env.router, http.MethodGet, path, nil, owner.AccessToken)
	if result.Code != 200 || result.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("preflight: %d %s", result.Code, result.Body.String())
	}
	view := decodeBody[store.CloudDeletionPreflight](t, result)
	if view.Eligible || len(view.Blockers) != 1 || view.Blockers[0].Code != "evidence_unavailable" {
		t.Fatalf("unconfigured ready: %+v", view)
	}
	newResponseContract(t).validate(t, http.MethodGet, path, result)
	for _, tc := range []struct {
		token string
		want  int
	}{{"", 401}, {other.AccessToken, 404}} {
		res := performJSON(env.router, http.MethodGet, path, nil, tc.token)
		if res.Code != tc.want {
			t.Fatalf("unauthorized preflight: %d %s", res.Code, res.Body.String())
		}
	}
	// Preflight does not install a fence or create a deletion operation.
	patch := managedCloudHTTP(t, env.router, http.MethodPatch, "/v1/developer/brand-clouds/"+owner.BrandCloudID, owner.AccessToken, "after-preflight", map[string]any{"name": "Still writable"})
	if patch.Code != 200 {
		t.Fatalf("preflight mutated lifecycle: %d %s", patch.Code, patch.Body.String())
	}
}
