package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestDerivePKCEIsStableAndStateBound(t *testing.T) {
	secret := strings.Repeat("s", 32)
	verifier, challenge, err := DerivePKCE("state-one", secret)
	if err != nil {
		t.Fatal(err)
	}
	if len(verifier) != 43 || len(challenge) != 43 {
		t.Fatalf("unexpected PKCE sizes: verifier=%d challenge=%d", len(verifier), len(challenge))
	}
	other, _, err := DerivePKCE("state-two", secret)
	if err != nil || other == verifier {
		t.Fatalf("PKCE verifier must be state-bound: other=%q err=%v", other, err)
	}
}

func TestSocialProviderValidationAndPKCEErrors(t *testing.T) {
	valid := SocialProvider{ID: "github", ClientID: "client", ClientSecret: "secret", RedirectURL: "https://admin.example.test/api/auth/social/callback", Enabled: true}
	cases := []struct {
		name     string
		provider SocialProvider
		want     error
	}{
		{name: "disabled", provider: SocialProvider{ID: "github"}, want: ErrSocialProviderNotFound},
		{name: "unknown", provider: SocialProvider{ID: "other", ClientID: "client", ClientSecret: "secret", RedirectURL: valid.RedirectURL, Enabled: true}, want: ErrSocialProviderNotFound},
		{name: "missing client", provider: SocialProvider{ID: "github", ClientSecret: "secret", RedirectURL: valid.RedirectURL, Enabled: true}, want: ErrSocialProviderMisconfigured},
		{name: "missing secret", provider: SocialProvider{ID: "github", ClientID: "client", RedirectURL: valid.RedirectURL, Enabled: true}, want: ErrSocialProviderMisconfigured},
		{name: "missing redirect", provider: SocialProvider{ID: "github", ClientID: "client", ClientSecret: "secret", Enabled: true}, want: ErrSocialProviderMisconfigured},
		{name: "relative redirect", provider: SocialProvider{ID: "github", ClientID: "client", ClientSecret: "secret", RedirectURL: "/callback", Enabled: true}, want: ErrSocialProviderMisconfigured},
		{name: "valid", provider: valid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.provider.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() error = %v, want %v", err, tc.want)
			}
		})
	}
	if _, _, err := DerivePKCE("", strings.Repeat("s", 32)); !errors.Is(err, ErrSocialProviderMisconfigured) {
		t.Fatalf("empty state error = %v", err)
	}
	if _, _, err := DerivePKCE("state", "short"); !errors.Is(err, ErrSocialProviderMisconfigured) {
		t.Fatalf("short secret error = %v", err)
	}
	if _, err := (SocialClient{}).AuthorizationURL(t.Context(), SocialProvider{}, "state", "nonce", "challenge"); !errors.Is(err, ErrSocialProviderNotFound) {
		t.Fatalf("AuthorizationURL error = %v", err)
	}
	if _, err := (SocialClient{}).ExchangeAndIdentify(t.Context(), SocialProvider{}, "code", "nonce", "verifier"); !errors.Is(err, ErrSocialProviderNotFound) {
		t.Fatalf("ExchangeAndIdentify error = %v", err)
	}
}

func TestGitHubAuthorizationURLUsesPKCEAndEmailScope(t *testing.T) {
	provider := SocialProvider{ID: "github", Name: "GitHub", Protocol: "oauth2", IssuerURL: "https://github.com", ClientID: "client", ClientSecret: "secret", RedirectURL: "https://admin.example.test/api/auth/social/callback", Enabled: true}
	location, err := (SocialClient{}).AuthorizationURL(context.Background(), provider, "state", "nonce", "challenge")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "github.com" || parsed.Query().Get("scope") != "user:email" || parsed.Query().Get("code_challenge") != "challenge" || parsed.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("unexpected GitHub authorization URL: %s", location)
	}
}

func TestGitHubAppAuthorizationURLUsesConfiguredPermissions(t *testing.T) {
	provider := SocialProvider{ID: "github", Name: "GitHub", Protocol: "oauth2", IssuerURL: "https://github.com", ClientID: "Iv23liExampleClient", ClientSecret: "secret", RedirectURL: "https://admin.example.test/api/auth/social/callback", Enabled: true}
	location, err := (SocialClient{}).AuthorizationURL(context.Background(), provider, "state", "nonce", "challenge")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	if scope := parsed.Query().Get("scope"); scope != "" {
		t.Fatalf("GitHub App authorization must use configured permissions, got scope %q", scope)
	}
	if parsed.Query().Get("code_challenge") != "challenge" || parsed.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("GitHub App authorization URL is missing PKCE: %s", location)
	}
}

func TestGitHubExchangeUsesStableIDAndVerifiedPrimaryEmail(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{}`
		switch req.URL.String() {
		case "https://github.com/login/oauth/access_token":
			body = `{"access_token":"temporary-provider-token"}`
		case "https://api.github.com/user":
			if req.Header.Get("Authorization") != "Bearer temporary-provider-token" {
				t.Fatalf("missing provider bearer header")
			}
			body = `{"id":12345,"login":"octocat","name":"Octo Cat"}`
		case "https://api.github.com/user/emails":
			body = `[{"email":"secondary@example.test","primary":false,"verified":true},{"email":"OWNER@EXAMPLE.TEST","primary":true,"verified":true}]`
		default:
			t.Fatalf("unexpected request: %s", req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	provider := SocialProvider{ID: "github", Name: "GitHub", Protocol: "oauth2", IssuerURL: "https://github.com", ClientID: "client", ClientSecret: "secret", RedirectURL: "https://admin.example.test/api/auth/social/callback", Enabled: true}
	identity, err := (SocialClient{HTTPClient: client}).ExchangeAndIdentify(context.Background(), provider, "code", "", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Subject != "12345" || identity.Email != "owner@example.test" || !identity.EmailVerified {
		t.Fatalf("unexpected GitHub identity: %+v", identity)
	}
}

func TestGitHubExchangeRejectsInvalidProviderResponses(t *testing.T) {
	provider := SocialProvider{ID: "github", Name: "GitHub", Protocol: "oauth2", IssuerURL: "https://github.com", ClientID: "client", ClientSecret: "secret", RedirectURL: "https://admin.example.test/api/auth/social/callback", Enabled: true}
	cases := []struct {
		name        string
		tokenStatus int
		tokenBody   string
		userStatus  int
		userBody    string
		emailStatus int
		emailBody   string
		want        error
	}{
		{name: "token status", tokenStatus: http.StatusBadGateway, want: ErrInvalidSocialIdentity},
		{name: "token malformed", tokenBody: `{`, want: ErrInvalidSocialIdentity},
		{name: "token provider error", tokenBody: `{"error":"bad_verification_code"}`, want: ErrInvalidSocialIdentity},
		{name: "token missing", tokenBody: `{}`, want: ErrInvalidSocialIdentity},
		{name: "user status", userStatus: http.StatusBadGateway, want: ErrInvalidSocialIdentity},
		{name: "user id missing", userBody: `{}`, want: ErrInvalidSocialIdentity},
		{name: "email status", emailStatus: http.StatusBadGateway, want: ErrInvalidSocialIdentity},
		{name: "email unverified", emailBody: `[{"email":"private@example.test","primary":true,"verified":false}]`, want: ErrSocialEmailUnverified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				status, body := http.StatusOK, ""
				switch req.URL.String() {
				case "https://github.com/login/oauth/access_token":
					status, body = tc.tokenStatus, tc.tokenBody
					if status == 0 {
						status = http.StatusOK
					}
					if body == "" {
						body = `{"access_token":"temporary-provider-token"}`
					}
				case "https://api.github.com/user":
					status, body = tc.userStatus, tc.userBody
					if status == 0 {
						status = http.StatusOK
					}
					if body == "" {
						body = `{"id":12345}`
					}
				case "https://api.github.com/user/emails":
					status, body = tc.emailStatus, tc.emailBody
					if status == 0 {
						status = http.StatusOK
					}
					if body == "" {
						body = `[{"email":"owner@example.test","primary":true,"verified":true}]`
					}
				default:
					t.Fatalf("unexpected request: %s", req.URL)
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})}
			_, err := (SocialClient{HTTPClient: client}).ExchangeAndIdentify(t.Context(), provider, "code", "", "verifier")
			if !errors.Is(err, tc.want) {
				t.Fatalf("ExchangeAndIdentify() error = %v, want %v", err, tc.want)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }
