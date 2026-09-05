package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

func TestSocialLoginHTTPRoundTripCreatesAndReusesAccount(t *testing.T) {
	env := newIntegrationEnv(t)
	providerClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := ""
		switch req.URL.String() {
		case "https://github.com/login/oauth/access_token":
			body = `{"access_token":"fixture-token"}`
		case "https://api.github.com/user":
			body = `{"id":42,"login":"fixture-user","name":"Fixture User"}`
		case "https://api.github.com/user/emails":
			body = `[{"email":"social-login@example.test","primary":true,"verified":true}]`
		default:
			t.Fatalf("unexpected social provider request: %s %s", req.Method, req.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}
	env.server.ConfigureSocialLogin(SocialLoginOptions{
		Providers: []auth.SocialProvider{{
			ID: "github", Name: "GitHub", Protocol: "oauth2", IssuerURL: "https://github.com",
			ClientID: "fixture-client", ClientSecret: "fixture-secret",
			RedirectURL: "https://console.example.test/v1/auth/social/callback", Enabled: true,
		}},
		HTTPClient: providerClient, StateSecret: strings.Repeat("s", 32),
	})
	if env.server.socialStateTTL != 10*time.Minute {
		t.Fatalf("expected default social state TTL, got %s", env.server.socialStateTTL)
	}

	for attempt := 0; attempt < 2; attempt++ {
		start := performJSON(env.router, http.MethodPost, "/v1/auth/social/start", map[string]any{
			"provider_id": "GITHUB",
			"next":        "/console/clouds/cloud-1/test-lab",
		}, "")
		if start.Code != http.StatusOK {
			t.Fatalf("attempt %d start status = %d: %s", attempt, start.Code, start.Body.String())
		}
		var startBody struct {
			RedirectURL string `json:"redirect_url"`
		}
		if err := json.Unmarshal(start.Body.Bytes(), &startBody); err != nil {
			t.Fatal(err)
		}
		redirect, err := url.Parse(startBody.RedirectURL)
		if err != nil {
			t.Fatal(err)
		}
		state := redirect.Query().Get("state")
		if state == "" || redirect.Query().Get("code_challenge") == "" {
			t.Fatalf("start response is missing OAuth state or PKCE challenge: %s", startBody.RedirectURL)
		}

		callback := performJSON(env.router, http.MethodPost, "/v1/auth/social/callback", map[string]any{
			"code": "fixture-code", "state": state,
		}, "")
		if callback.Code != http.StatusOK {
			t.Fatalf("attempt %d callback status = %d: %s", attempt, callback.Code, callback.Body.String())
		}
		var callbackBody struct {
			User struct {
				Email         string `json:"email"`
				EmailVerified bool   `json:"email_verified"`
			} `json:"user"`
			ReturnPath string `json:"return_path"`
		}
		if err := json.Unmarshal(callback.Body.Bytes(), &callbackBody); err != nil {
			t.Fatal(err)
		}
		if callbackBody.User.Email != "social-login@example.test" || !callbackBody.User.EmailVerified {
			t.Fatalf("unexpected social user: %+v", callbackBody.User)
		}
		if callbackBody.ReturnPath != "/console/clouds/cloud-1/test-lab" {
			t.Fatalf("return path = %q", callbackBody.ReturnPath)
		}
	}
}

func TestSocialLoginPublicValidationAndHelpers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := New(nil, nil)
	server.ConfigureSocialLogin(SocialLoginOptions{Providers: []auth.SocialProvider{
		{ID: "github", Name: "GitHub", Protocol: "oauth2", ClientID: "client", ClientSecret: "secret", RedirectURL: "https://console.example.test/callback", Enabled: true},
		{ID: "google", Name: "Google", Enabled: false},
	}, StateSecret: strings.Repeat("s", 32), StateTTL: time.Minute})
	router := server.Router()

	providers := performJSON(router, http.MethodGet, "/v1/auth/social/providers", nil, "")
	if providers.Code != http.StatusOK || !strings.Contains(providers.Body.String(), `"id":"github"`) || strings.Contains(providers.Body.String(), `"id":"google"`) {
		t.Fatalf("unexpected provider catalog: status=%d body=%s", providers.Code, providers.Body.String())
	}
	malformed := performRaw(router, http.MethodPost, "/v1/auth/social/start", []byte(`{`), "")
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed start status = %d", malformed.Code)
	}
	unknown := performJSON(router, http.MethodPost, "/v1/auth/social/start", map[string]any{"provider_id": "unknown"}, "")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown provider status = %d", unknown.Code)
	}
	cancelled := performJSON(router, http.MethodPost, "/v1/auth/social/callback", map[string]any{"error": "access_denied"}, "")
	if cancelled.Code != http.StatusBadRequest || !strings.Contains(cancelled.Body.String(), "social_login_cancelled") {
		t.Fatalf("cancelled callback: status=%d body=%s", cancelled.Code, cancelled.Body.String())
	}
	invalidState := performJSON(router, http.MethodPost, "/v1/auth/social/callback", map[string]any{"state": "state-only"}, "")
	if invalidState.Code != http.StatusBadRequest || !strings.Contains(invalidState.Body.String(), "invalid_social_state") {
		t.Fatalf("invalid callback: status=%d body=%s", invalidState.Code, invalidState.Body.String())
	}

	for raw, want := range map[string]string{
		" /console/clouds/cloud-1/test-lab?ignored=1 ": "/console/clouds/cloud-1/test-lab",
		"/admin/../admin/members":                      "/admin/members",
		"https://attacker.example/console":             "",
		"//attacker.example/console":                   "",
		`/console\redirect`:                            "",
		"/untrusted":                                   "",
	} {
		if got := safeSocialNext(raw); got != want {
			t.Errorf("safeSocialNext(%q) = %q, want %q", raw, got, want)
		}
	}
	if got := socialDisplayName(map[string]any{"name": "  Fixture User  "}); got == nil || *got != "Fixture User" {
		t.Fatalf("display name = %v", got)
	}
	if got := socialDisplayName(map[string]any{"name": "", "login": " fixture-login "}); got == nil || *got != "fixture-login" {
		t.Fatalf("fallback display name = %v", got)
	}
	if socialDisplayName(map[string]any{"name": 42}) != nil {
		t.Fatal("non-string display name should be ignored")
	}
	value := "value"
	if stringValue(nil) != "" || stringValue(&value) != value {
		t.Fatal("stringValue did not preserve pointer semantics")
	}
}

func TestWriteSocialLoginErrorMapsPublicFailures(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "provider missing", err: auth.ErrSocialProviderNotFound, status: http.StatusNotFound, code: "social_provider_not_found"},
		{name: "provider invalid", err: auth.ErrSocialProviderMisconfigured, status: http.StatusServiceUnavailable, code: "social_provider_misconfigured"},
		{name: "state invalid", err: store.ErrOIDCStateInvalid, status: http.StatusBadRequest, code: "invalid_social_state"},
		{name: "state expired", err: store.ErrOIDCStateExpired, status: http.StatusBadRequest, code: "invalid_social_state"},
		{name: "email unverified", err: auth.ErrSocialEmailUnverified, status: http.StatusForbidden, code: "social_email_unverified"},
		{name: "oidc email unverified", err: auth.ErrUnverifiedOIDCEmail, status: http.StatusForbidden, code: "social_email_unverified"},
		{name: "user unavailable", err: errOIDCUserNotProvisioned, status: http.StatusForbidden, code: "user_not_provisioned"},
		{name: "identity invalid", err: auth.ErrInvalidSocialIdentity, status: http.StatusUnauthorized, code: "invalid_social_identity"},
		{name: "token invalid", err: auth.ErrInvalidOIDCToken, status: http.StatusUnauthorized, code: "invalid_social_identity"},
		{name: "store conflict", err: store.ErrConflict, status: http.StatusConflict, code: "conflict"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			writeSocialLoginError(ctx, tc.err)
			if response.Code != tc.status || !strings.Contains(response.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestSocialLoginActivatesExistingPendingUser(t *testing.T) {
	for _, linkedBeforeLogin := range []bool{false, true} {
		name := "matched_by_email"
		if linkedBeforeLogin {
			name = "matched_by_existing_identity"
		}
		t.Run(name, func(t *testing.T) {
			env := newIntegrationEnv(t)
			ctx := context.Background()
			email := name + "@example.test"
			signup, err := env.store.SignupDeveloper(ctx, store.DeveloperSignupInput{
				Email: email, PasswordHash: "pending-password-hash", OrganizationName: "Pending Cloud",
				SignupPendingVerification: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := env.store.CreateEmailVerificationToken(ctx, signup.User.ID, "pending-verification-token", time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}

			secretRef := "env:GOOGLE_CLIENT_SECRET"
			provider, err := env.store.CreateIdentityProvider(ctx, store.IdentityProviderCreateInput{
				ProviderID: "google", Name: "Google", Type: model.IdentityProviderTypeOIDC,
				IssuerURL: "https://accounts.google.com", ClientID: "client", ClientSecretRef: &secretRef,
				Scopes: []string{"openid", "email", "profile"}, Enabled: true, Now: time.Now().UTC(),
			})
			if err != nil {
				t.Fatal(err)
			}
			identity := auth.OIDCIdentity{
				Issuer: provider.IssuerURL, Subject: "subject-" + name, Email: email, EmailVerified: true,
				Claims: map[string]any{"sub": "subject-" + name, "email": email, "email_verified": true},
			}
			if linkedBeforeLogin {
				if _, err := env.store.CreateUserIdentity(ctx, store.UserIdentityCreateInput{
					UserID: signup.User.ID, ProviderID: provider.ID, IssuerURL: identity.Issuer,
					Subject: identity.Subject, Email: email, EmailVerified: true, Claims: identity.Claims, Now: time.Now().UTC(),
				}); err != nil {
					t.Fatal(err)
				}
			}

			ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
			ginContext.Request = httptest.NewRequest("GET", "/v1/auth/social/callback", nil)
			user, err := env.server.resolveSocialUser(ginContext, provider, identity)
			if err != nil {
				t.Fatal(err)
			}
			if !user.EmailVerified || user.EmailVerifiedAt == nil || user.SignupPendingVerification {
				t.Fatalf("expected social login to activate pending user, got %+v", user)
			}
			linked, err := env.store.GetUserIdentityByProviderSubject(ctx, provider.ID, identity.Subject)
			if err != nil || linked.UserID != signup.User.ID {
				t.Fatalf("expected identity linked to existing user: identity=%+v err=%v", linked, err)
			}
			var activeVerificationTokens int
			if err := env.db.QueryRow(ctx, `SELECT count(*) FROM auth_tokens WHERE user_id=$1 AND purpose='email_verification' AND consumed_at IS NULL`, signup.User.ID).Scan(&activeVerificationTokens); err != nil {
				t.Fatal(err)
			}
			if activeVerificationTokens != 0 {
				t.Fatalf("expected old verification tokens to be invalidated, got %d active", activeVerificationTokens)
			}
		})
	}
}

func TestSocialLoginDoesNotReactivateDisabledUser(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()
	signup, err := env.store.SignupDeveloper(ctx, store.DeveloperSignupInput{
		Email: "disabled-social@example.test", PasswordHash: "hash", OrganizationName: "Disabled Cloud",
		EmailVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	secretRef := "env:GITHUB_CLIENT_SECRET"
	provider, err := env.store.CreateIdentityProvider(ctx, store.IdentityProviderCreateInput{
		ProviderID: "github", Name: "GitHub", Type: model.IdentityProviderTypeOAuth2,
		IssuerURL: "https://github.com", ClientID: "client", ClientSecretRef: &secretRef,
		Enabled: true, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := auth.OIDCIdentity{Issuer: provider.IssuerURL, Subject: "disabled-subject", Email: signup.User.Email, EmailVerified: true}
	if _, err := env.store.CreateUserIdentity(ctx, store.UserIdentityCreateInput{
		UserID: signup.User.ID, ProviderID: provider.ID, IssuerURL: identity.Issuer, Subject: identity.Subject,
		Email: identity.Email, EmailVerified: true, Now: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, signup.User.ID); err != nil {
		t.Fatal(err)
	}
	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginContext.Request = httptest.NewRequest("GET", "/v1/auth/social/callback", nil)
	if _, err := env.server.resolveSocialUser(ginContext, provider, identity); !errors.Is(err, errOIDCUserNotProvisioned) {
		t.Fatalf("expected disabled account to remain blocked, got %v", err)
	}
}
