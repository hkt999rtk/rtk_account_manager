package api

import (
	"context"
	"net/http"
	"testing"
)

func TestMultiCloudRegisterSignupOnboardingParityIntegration(t *testing.T) {
	for _, route := range []string{"/v1/auth/signup", "/v1/auth/register"} {
		t.Run(route, func(t *testing.T) {
			env := newIntegrationEnv(t)
			ctx := context.Background()
			contract := newResponseContract(t)
			const email = "alias-owner@example.com"
			res := performJSON(env.router, http.MethodPost, route, map[string]any{"email": "ALIAS-OWNER@example.com"}, "")
			if res.Code != http.StatusAccepted {
				t.Fatalf("signup=%d %s", res.Code, res.Body.String())
			}
			contract.validate(t, http.MethodPost, route, res)
			body := decodeBody[developerSignupBody](t, res)
			raw := decodeBody[map[string]any](t, res)
			if _, ok := raw["tokens"]; ok {
				t.Fatal("registration returned session tokens")
			}
			if !body.User.SignupPendingVerification || body.User.EmailVerified || body.User.Email != email || body.BrandCloud.Role != "owner" || body.BrandCloud.OrganizationKind != "brand_cloud" {
				t.Fatalf("invalid pending owner: %+v", body)
			}
			var members, owners, outbox, refresh int
			if err := env.db.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM organization_members WHERE organization_id=$1),
				(SELECT count(*) FROM organization_members WHERE organization_id=$1 AND user_id=$2 AND role='owner'),
				(SELECT count(*) FROM email_outbox WHERE message_type='email_verification'),
				(SELECT count(*) FROM refresh_tokens WHERE user_id=$2)`, body.BrandCloud.ID, body.User.ID).Scan(&members, &owners, &outbox, &refresh); err != nil {
				t.Fatal(err)
			}
			if members != 1 || owners != 1 || outbox != 1 || refresh != 0 {
				t.Fatalf("atomic bootstrap counts=%d/%d/%d/%d", members, owners, outbox, refresh)
			}
			login := func() int {
				return performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{"email": email, "password": "password123"}, "").Code
			}
			if login() != http.StatusUnauthorized {
				t.Fatal("pending owner logged in")
			}
			firstToken := latestAuthToken(t, env.tokenObserver, email, "email_verification")
			otherRoute := "/v1/auth/register"
			if route == otherRoute {
				otherRoute = "/v1/auth/signup"
			}
			duplicate := performJSON(env.router, http.MethodPost, otherRoute, map[string]any{"email": email}, "")
			if duplicate.Code != http.StatusConflict {
				t.Fatalf("live pending duplicate=%d", duplicate.Code)
			}
			if _, err := env.db.Exec(ctx, `UPDATE auth_tokens SET expires_at=now()-interval '1 minute' WHERE user_id=$1`, body.User.ID); err != nil {
				t.Fatal(err)
			}
			retry := performJSON(env.router, http.MethodPost, otherRoute, map[string]any{"email": email}, "")
			if retry.Code != http.StatusAccepted {
				t.Fatalf("expired retry=%d %s", retry.Code, retry.Body.String())
			}
			retried := decodeBody[developerSignupBody](t, retry)
			if retried.User.ID != body.User.ID || retried.BrandCloud.ID != body.BrandCloud.ID {
				t.Fatal("retry created a second identity/cloud")
			}
			old := performJSON(env.router, http.MethodPost, "/v1/auth/verify-email", map[string]any{"token": firstToken, "new_password": "password123"}, "")
			if old.Code != http.StatusBadRequest {
				t.Fatalf("expired activation accepted: %d", old.Code)
			}
			token := latestAuthToken(t, env.tokenObserver, email, "email_verification")
			verified := performJSON(env.router, http.MethodPost, "/v1/auth/verify-email", map[string]any{"token": token, "new_password": "password123"}, "")
			if verified.Code != http.StatusOK || login() != http.StatusOK {
				t.Fatalf("activation/login failed: %d %s", verified.Code, verified.Body.String())
			}
			legacy := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/"+body.BrandCloud.TenantSlug+"/auth/login", map[string]any{"email": email, "password": "password123"}, "")
			if legacy.Code != http.StatusNotFound {
				t.Fatalf("tenant authentication revived: %d", legacy.Code)
			}
		})
	}
}

func TestMultiCloudRegisterSignupEmailFailureRollsBackIntegration(t *testing.T) {
	for _, route := range []string{"/v1/auth/register", "/v1/auth/signup"} {
		t.Run(route, func(t *testing.T) {
			env := newIntegrationEnv(t)
			env.store.ConfigureEmailOutboxCipher(nil)
			res := performJSON(env.router, http.MethodPost, route, map[string]any{"email": "atomic-failure@example.com"}, "")
			if res.Code < 500 {
				t.Fatalf("email failure did not fail closed: %d %s", res.Code, res.Body.String())
			}
			var users, clouds, members, tokens int
			if err := env.db.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM users),(SELECT count(*) FROM organizations),(SELECT count(*) FROM organization_members),(SELECT count(*) FROM auth_tokens)`).Scan(&users, &clouds, &members, &tokens); err != nil {
				t.Fatal(err)
			}
			if users+clouds+members+tokens != 0 {
				t.Fatalf("partial registration: %d/%d/%d/%d", users, clouds, members, tokens)
			}
		})
	}
}
