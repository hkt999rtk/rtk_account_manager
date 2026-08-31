package api

import (
	"context"
	"net/http"
	"strconv"
	"testing"
)

func TestIntegrationPendingGlobalUserCannotUseExistingTokens(t *testing.T) {
	for _, verified := range []bool{false, true} {
		t.Run("verified-"+strconv.FormatBool(verified), func(t *testing.T) {
			env := newIntegrationEnv(t)
			owner := verifiedDeveloperForTest(t, env, "pending-state@example.com")
			login := performJSON(env.router, http.MethodPost, "/v1/auth/login",
				map[string]any{"email": "pending-state@example.com", "password": "password123"}, "")
			if login.Code != http.StatusOK {
				t.Fatalf("fixture login status=%d", login.Code)
			}
			tokens := decodeBody[tokenBody](t, login).Tokens
			if _, err := env.db.Exec(context.Background(), `UPDATE users SET signup_pending_verification=true,email_verified=$2 WHERE id=$1`, owner.UserID, verified); err != nil {
				t.Fatal(err)
			}
			for _, req := range []struct{ method, path string }{
				{http.MethodGet, "/v1/me"},
				{http.MethodPost, "/v1/orgs"},
			} {
				res := performJSON(env.router, req.method, req.path, map[string]any{"name": "Must not be created"}, tokens.AccessToken)
				if res.Code != http.StatusUnauthorized {
					t.Errorf("pending user's valid JWT: %s %s status=%d, want 401", req.method, req.path, res.Code)
				}
			}
			res := performJSON(env.router, http.MethodPost, "/v1/auth/refresh", map[string]any{"refresh_token": tokens.RefreshToken}, "")
			if res.Code != http.StatusUnauthorized {
				t.Errorf("pending user's existing refresh token: status=%d, want 401", res.Code)
			}
		})
	}
}
