package auth

import (
	"context"
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }
