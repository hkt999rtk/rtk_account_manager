package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"rtk_account_manager/internal/model"
)

func TestProviderResolverPrefersEnabledDBProvider(t *testing.T) {
	secretRef := "env:OIDC_CLIENT_SECRET"
	resolver := ProviderResolver{
		Store: fakeProviderStore{provider: model.IdentityProvider{
			ID:              "provider-db-id",
			ProviderID:      "db-keycloak",
			Name:            "DB Keycloak",
			Type:            model.IdentityProviderTypeOIDC,
			IssuerURL:       "https://db.example.test/realms/account",
			ClientID:        "db-client",
			ClientSecretRef: &secretRef,
			Scopes:          []string{"openid", "email"},
			Enabled:         true,
		}},
		Env: OIDCEnvConfig{
			Enabled:       true,
			ProviderID:    "env-keycloak",
			ProviderName:  "Env Keycloak",
			IssuerURL:     "https://env.example.test/realms/account",
			ClientID:      "env-client",
			ClientSecret:  "env-secret",
			RedirectURL:   "https://api.example.test/v1/auth/oidc/db-keycloak/callback",
			Scopes:        []string{"openid", "profile"},
			AutoLinkEmail: true,
		},
		Getenv: func(key string) string {
			if key == "OIDC_CLIENT_SECRET" {
				return "db-secret"
			}
			return ""
		},
	}

	provider, err := resolver.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if provider.ProviderID != "db-keycloak" || provider.ClientID != "db-client" || provider.ClientSecret != "db-secret" {
		t.Fatalf("expected DB provider to win, got %+v", provider)
	}
	if !provider.AutoLinkEmail {
		t.Fatal("expected env auto-link policy to be applied")
	}
}

func TestProviderResolverFallsBackToEnvProviderWhenDBProviderMissing(t *testing.T) {
	notFound := errors.New("not found")
	resolver := ProviderResolver{
		Store: fakeProviderStore{err: notFound},
		IsNotFound: func(err error) bool {
			return errors.Is(err, notFound)
		},
		Env: OIDCEnvConfig{
			Enabled:      true,
			ProviderID:   "env-keycloak",
			ProviderName: "Env Keycloak",
			IssuerURL:    "https://env.example.test/realms/account",
			ClientID:     "env-client",
			ClientSecret: "env-secret",
			RedirectURL:  "https://api.example.test/v1/auth/oidc/env-keycloak/callback",
			Scopes:       []string{"openid", "email", "profile"},
		},
	}

	provider, err := resolver.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if provider.ProviderID != "env-keycloak" || provider.ClientID != "env-client" {
		t.Fatalf("expected env provider, got %+v", provider)
	}
}

func TestProviderResolverRejectsDisabledOrMisconfiguredProvider(t *testing.T) {
	_, err := (ProviderResolver{Env: OIDCEnvConfig{Enabled: false}}).Resolve(context.Background())
	if !errors.Is(err, ErrOIDCDisabled) {
		t.Fatalf("expected disabled error, got %v", err)
	}

	_, err = (ProviderResolver{Env: OIDCEnvConfig{
		Enabled:     true,
		ProviderID:  "keycloak",
		IssuerURL:   "https://issuer.example.test",
		RedirectURL: "https://api.example.test/callback",
	}}).Resolve(context.Background())
	if !errors.Is(err, ErrOIDCProviderMisconfigured) {
		t.Fatalf("expected misconfigured error, got %v", err)
	}
}

func TestProviderResolverRejectsUnsupportedOrUnsetSecretRefs(t *testing.T) {
	for name, secretRef := range map[string]string{
		"raw":         "raw-secret",
		"empty_env":   "env:",
		"missing_env": "env:MISSING_OIDC_SECRET",
	} {
		t.Run(name, func(t *testing.T) {
			resolver := ProviderResolver{
				Store: fakeProviderStore{provider: model.IdentityProvider{
					ID:              "provider-db-id",
					ProviderID:      "db-keycloak",
					Name:            "DB Keycloak",
					Type:            model.IdentityProviderTypeOIDC,
					IssuerURL:       "https://db.example.test/realms/account",
					ClientID:        "db-client",
					ClientSecretRef: &secretRef,
					Enabled:         true,
				}},
				Env: OIDCEnvConfig{
					Enabled:     true,
					RedirectURL: "https://api.example.test/callback",
					Scopes:      []string{"openid"},
				},
				Getenv: func(string) string { return "" },
			}
			_, err := resolver.Resolve(context.Background())
			if !errors.Is(err, ErrOIDCProviderMisconfigured) {
				t.Fatalf("expected misconfigured error, got %v", err)
			}
		})
	}
}

func TestOIDCProviderValidateRejectsMalformedURLs(t *testing.T) {
	provider := OIDCProvider{
		ProviderID:   "keycloak",
		IssuerURL:    "https://issuer.example.test",
		ClientID:     "client",
		ClientSecret: "secret",
		RedirectURL:  "://bad",
		Enabled:      true,
	}
	if err := provider.Validate(); !errors.Is(err, ErrOIDCProviderMisconfigured) {
		t.Fatalf("expected invalid redirect error, got %v", err)
	}
	provider.RedirectURL = "https://api.example.test/callback"
	provider.IssuerURL = "not a url"
	if err := provider.Validate(); !errors.Is(err, ErrOIDCProviderMisconfigured) {
		t.Fatalf("expected invalid issuer error, got %v", err)
	}
}

func TestOIDCClientAuthorizationURL(t *testing.T) {
	fake := newFakeOIDCServer(t)
	defer fake.close()

	client := OIDCClient{HTTPClient: fake.server.Client(), Now: fake.nowFn}
	authURL, err := client.AuthorizationURL(context.Background(), fake.provider(), "state-1", "nonce-1")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/authorize" {
		t.Fatalf("unexpected authorization path: %s", parsed.Path)
	}
	query := parsed.Query()
	for key, want := range map[string]string{
		"response_type": "code",
		"client_id":     "rtk-account-manager",
		"redirect_uri":  "https://api.example.test/v1/auth/oidc/keycloak/callback",
		"scope":         "openid email profile",
		"state":         "state-1",
		"nonce":         "nonce-1",
	} {
		if got := query.Get(key); got != want {
			t.Fatalf("unexpected %s: got %q want %q", key, got, want)
		}
	}
}

func TestOIDCClientExchangeAndValidateIDToken(t *testing.T) {
	fake := newFakeOIDCServer(t)
	defer fake.close()
	fake.idToken = fake.signToken(t, tokenFixture{Subject: "sub-1", Email: "User@Example.com", EmailVerified: true, Nonce: "nonce-1"})

	identity, tokens, err := (OIDCClient{HTTPClient: fake.server.Client(), Now: fake.nowFn}).ExchangeAndValidate(context.Background(), fake.provider(), "auth-code", "nonce-1")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "provider-access-token" || tokens.RefreshToken != "provider-refresh-token" {
		t.Fatalf("unexpected token response: %+v", tokens)
	}
	if identity.Issuer != fake.server.URL || identity.Subject != "sub-1" || identity.Email != "user@example.com" || !identity.EmailVerified {
		t.Fatalf("unexpected identity: %+v", identity)
	}
}

func TestOIDCClientRejectsInvalidIssuer(t *testing.T) {
	fake := newFakeOIDCServer(t)
	defer fake.close()
	fake.idToken = fake.signToken(t, tokenFixture{Issuer: "https://evil.example.test", Subject: "sub-1", Email: "user@example.com", EmailVerified: true, Nonce: "nonce-1"})

	_, err := (OIDCClient{HTTPClient: fake.server.Client(), Now: fake.nowFn}).ValidateIDToken(context.Background(), fake.provider(), fake.idToken, "nonce-1")
	if !errors.Is(err, ErrInvalidOIDCToken) {
		t.Fatalf("expected invalid token error, got %v", err)
	}
}

func TestOIDCClientRejectsInvalidAudience(t *testing.T) {
	fake := newFakeOIDCServer(t)
	defer fake.close()
	fake.idToken = fake.signToken(t, tokenFixture{Audience: []string{"other-client"}, Subject: "sub-1", Email: "user@example.com", EmailVerified: true, Nonce: "nonce-1"})

	_, err := (OIDCClient{HTTPClient: fake.server.Client(), Now: fake.nowFn}).ValidateIDToken(context.Background(), fake.provider(), fake.idToken, "nonce-1")
	if !errors.Is(err, ErrInvalidOIDCToken) {
		t.Fatalf("expected invalid token error, got %v", err)
	}
}

func TestOIDCClientRejectsInvalidSignature(t *testing.T) {
	fake := newFakeOIDCServer(t)
	defer fake.close()
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fake.idToken = signOIDCTestToken(t, otherKey, "oidc-test-key", tokenFixture{Issuer: fake.server.URL, Audience: []string{"rtk-account-manager"}, Subject: "sub-1", Email: "user@example.com", EmailVerified: true, Nonce: "nonce-1", Now: fake.now})

	_, err = (OIDCClient{HTTPClient: fake.server.Client(), Now: fake.nowFn}).ValidateIDToken(context.Background(), fake.provider(), fake.idToken, "nonce-1")
	if !errors.Is(err, ErrInvalidOIDCToken) {
		t.Fatalf("expected invalid token error, got %v", err)
	}
}

func TestOIDCClientRejectsExpiredToken(t *testing.T) {
	fake := newFakeOIDCServer(t)
	defer fake.close()
	fake.idToken = fake.signToken(t, tokenFixture{Subject: "sub-1", Email: "user@example.com", EmailVerified: true, Nonce: "nonce-1", ExpiresAt: fake.now.Add(-time.Minute)})

	_, err := (OIDCClient{HTTPClient: fake.server.Client(), Now: fake.nowFn}).ValidateIDToken(context.Background(), fake.provider(), fake.idToken, "nonce-1")
	if !errors.Is(err, ErrInvalidOIDCToken) {
		t.Fatalf("expected invalid token error, got %v", err)
	}
}

func TestOIDCClientRejectsInvalidNonce(t *testing.T) {
	fake := newFakeOIDCServer(t)
	defer fake.close()
	fake.idToken = fake.signToken(t, tokenFixture{Subject: "sub-1", Email: "user@example.com", EmailVerified: true, Nonce: "nonce-1"})

	_, err := (OIDCClient{HTTPClient: fake.server.Client(), Now: fake.nowFn}).ValidateIDToken(context.Background(), fake.provider(), fake.idToken, "other-nonce")
	if !errors.Is(err, ErrInvalidOIDCToken) {
		t.Fatalf("expected invalid token error, got %v", err)
	}
}

func TestOIDCClientRejectsUnverifiedEmail(t *testing.T) {
	fake := newFakeOIDCServer(t)
	defer fake.close()
	fake.idToken = fake.signToken(t, tokenFixture{Subject: "sub-1", Email: "user@example.com", EmailVerified: false, Nonce: "nonce-1"})

	_, err := (OIDCClient{HTTPClient: fake.server.Client(), Now: fake.nowFn}).ValidateIDToken(context.Background(), fake.provider(), fake.idToken, "nonce-1")
	if !errors.Is(err, ErrUnverifiedOIDCEmail) {
		t.Fatalf("expected unverified email error, got %v", err)
	}
}

func TestOIDCClientReturnsTypedMisconfiguredError(t *testing.T) {
	fake := newFakeOIDCServer(t)
	defer fake.close()
	fake.discoveryIssuer = "https://wrong-issuer.example.test"

	_, err := (OIDCClient{HTTPClient: fake.server.Client(), Now: fake.nowFn}).Discover(context.Background(), fake.provider())
	if !errors.Is(err, ErrOIDCProviderMisconfigured) {
		t.Fatalf("expected misconfigured error, got %v", err)
	}
}

func TestOIDCClientExchangeCodeRejectsBadTokenResponses(t *testing.T) {
	for name, setup := range map[string]func(*fakeOIDCServer){
		"bad_status": func(fake *fakeOIDCServer) { fake.tokenStatus = http.StatusBadGateway },
		"bad_json":   func(fake *fakeOIDCServer) { fake.tokenRaw = `{"id_token":` },
		"missing_id": func(fake *fakeOIDCServer) { fake.tokenRaw = `{"access_token":"access"}` },
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeOIDCServer(t)
			defer fake.close()
			setup(fake)

			_, err := (OIDCClient{HTTPClient: fake.server.Client(), Now: fake.nowFn}).ExchangeCode(context.Background(), fake.provider(), "auth-code")
			if name == "missing_id" {
				if !errors.Is(err, ErrInvalidOIDCToken) {
					t.Fatalf("expected invalid token error, got %v", err)
				}
				return
			}
			if !errors.Is(err, ErrOIDCProviderMisconfigured) {
				t.Fatalf("expected misconfigured error, got %v", err)
			}
		})
	}
}

func TestOIDCClientDiscoverRejectsBadDiscoveryResponses(t *testing.T) {
	for name, setup := range map[string]func(*fakeOIDCServer){
		"bad_status": func(fake *fakeOIDCServer) { fake.discoveryStatus = http.StatusInternalServerError },
		"bad_json":   func(fake *fakeOIDCServer) { fake.discoveryRaw = `{"issuer":` },
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeOIDCServer(t)
			defer fake.close()
			setup(fake)

			_, err := (OIDCClient{HTTPClient: fake.server.Client(), Now: fake.nowFn}).Discover(context.Background(), fake.provider())
			if !errors.Is(err, ErrOIDCProviderMisconfigured) {
				t.Fatalf("expected misconfigured error, got %v", err)
			}
		})
	}
}

func TestOIDCClientValidateRejectsBadJWKSResponses(t *testing.T) {
	for name, setup := range map[string]func(*fakeOIDCServer){
		"bad_status": func(fake *fakeOIDCServer) { fake.jwksStatus = http.StatusBadGateway },
		"bad_json":   func(fake *fakeOIDCServer) { fake.jwksRaw = `{"keys":` },
		"empty":      func(fake *fakeOIDCServer) { fake.jwksRaw = `{"keys":[]}` },
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeOIDCServer(t)
			defer fake.close()
			fake.idToken = fake.signToken(t, tokenFixture{Subject: "sub-1", Email: "user@example.com", EmailVerified: true, Nonce: "nonce-1"})
			setup(fake)

			_, err := (OIDCClient{HTTPClient: fake.server.Client(), Now: fake.nowFn}).ValidateIDToken(context.Background(), fake.provider(), fake.idToken, "nonce-1")
			if !errors.Is(err, ErrOIDCProviderMisconfigured) {
				t.Fatalf("expected misconfigured error, got %v", err)
			}
		})
	}
}

func TestOIDCClientRejectsUnexpectedSigningMethod(t *testing.T) {
	fake := newFakeOIDCServer(t)
	defer fake.close()
	claims := oidcClaims{
		Email:         "user@example.com",
		EmailVerified: true,
		Nonce:         "nonce-1",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    fake.server.URL,
			Subject:   "sub-1",
			Audience:  jwt.ClaimStrings{"rtk-account-manager"},
			ExpiresAt: jwt.NewNumericDate(fake.now.Add(time.Hour)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = fake.keyID
	var err error
	fake.idToken, err = token.SignedString([]byte("wrong-method"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = (OIDCClient{HTTPClient: fake.server.Client(), Now: fake.nowFn}).ValidateIDToken(context.Background(), fake.provider(), fake.idToken, "nonce-1")
	if !errors.Is(err, ErrInvalidOIDCToken) {
		t.Fatalf("expected invalid token error, got %v", err)
	}
}

type fakeProviderStore struct {
	provider model.IdentityProvider
	err      error
}

func (s fakeProviderStore) GetIdentityProviderByProviderID(context.Context, string) (model.IdentityProvider, error) {
	if s.err != nil {
		return model.IdentityProvider{}, s.err
	}
	return s.provider, nil
}

type fakeOIDCServer struct {
	server          *httptest.Server
	key             *rsa.PrivateKey
	keyID           string
	now             time.Time
	idToken         string
	discoveryIssuer string
	discoveryStatus int
	discoveryRaw    string
	tokenStatus     int
	tokenRaw        string
	jwksStatus      int
	jwksRaw         string
}

func newFakeOIDCServer(t *testing.T) *fakeOIDCServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeOIDCServer{
		key:   key,
		keyID: "oidc-test-key",
		now:   time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if fake.discoveryStatus != 0 {
			http.Error(w, "discovery failed", fake.discoveryStatus)
			return
		}
		if fake.discoveryRaw != "" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(fake.discoveryRaw))
			return
		}
		issuer := fake.discoveryIssuer
		if issuer == "" {
			issuer = fake.server.URL
		}
		writeJSON(t, w, map[string]any{
			"issuer":                 issuer,
			"authorization_endpoint": fake.server.URL + "/authorize",
			"token_endpoint":         fake.server.URL + "/token",
			"jwks_uri":               fake.server.URL + "/jwks",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if fake.tokenStatus != 0 {
			http.Error(w, "token failed", fake.tokenStatus)
			return
		}
		if fake.tokenRaw != "" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(fake.tokenRaw))
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "auth-code" || r.Form.Get("client_id") != "rtk-account-manager" || r.Form.Get("client_secret") != "client-secret" {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		writeJSON(t, w, OIDCTokenResponse{
			TokenType:    "Bearer",
			ExpiresIn:    3600,
			AccessToken:  "provider-access-token",
			RefreshToken: "provider-refresh-token",
			IDToken:      fake.idToken,
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		if fake.jwksStatus != 0 {
			http.Error(w, "jwks failed", fake.jwksStatus)
			return
		}
		if fake.jwksRaw != "" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(fake.jwksRaw))
			return
		}
		writeJSON(t, w, map[string]any{"keys": []jwk{publicJWK(fake.keyID, &fake.key.PublicKey)}})
	})
	fake.server = httptest.NewServer(mux)
	return fake
}

func (f *fakeOIDCServer) close() {
	f.server.Close()
}

func (f *fakeOIDCServer) nowFn() time.Time {
	return f.now
}

func (f *fakeOIDCServer) provider() OIDCProvider {
	return OIDCProvider{
		ProviderID:   "keycloak",
		Name:         "Keycloak",
		IssuerURL:    f.server.URL,
		ClientID:     "rtk-account-manager",
		ClientSecret: "client-secret",
		RedirectURL:  "https://api.example.test/v1/auth/oidc/keycloak/callback",
		Scopes:       []string{"openid", "email", "profile"},
		Enabled:      true,
	}
}

func (f *fakeOIDCServer) signToken(t *testing.T, fixture tokenFixture) string {
	t.Helper()
	if fixture.Issuer == "" {
		fixture.Issuer = f.server.URL
	}
	if len(fixture.Audience) == 0 {
		fixture.Audience = []string{"rtk-account-manager"}
	}
	if fixture.Now.IsZero() {
		fixture.Now = f.now
	}
	return signOIDCTestToken(t, f.key, f.keyID, fixture)
}

type tokenFixture struct {
	Issuer        string
	Audience      []string
	Subject       string
	Email         string
	EmailVerified bool
	Nonce         string
	Now           time.Time
	ExpiresAt     time.Time
}

func signOIDCTestToken(t *testing.T, key *rsa.PrivateKey, keyID string, fixture tokenFixture) string {
	t.Helper()
	now := fixture.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	expiresAt := fixture.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = now.Add(time.Hour)
	}
	claims := oidcClaims{
		Email:         fixture.Email,
		EmailVerified: fixture.EmailVerified,
		Nonce:         fixture.Nonce,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    fixture.Issuer,
			Subject:   fixture.Subject,
			Audience:  jwt.ClaimStrings(fixture.Audience),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = keyID
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func publicJWK(keyID string, key *rsa.PublicKey) jwk {
	return jwk{
		KeyID:     keyID,
		KeyType:   "RSA",
		Algorithm: "RS256",
		Use:       "sig",
		N:         base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		E:         base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func TestOIDCTokenErrorsDoNotContainProviderTokens(t *testing.T) {
	fake := newFakeOIDCServer(t)
	defer fake.close()
	fake.idToken = strings.Repeat("not-a-jwt.", 2) + "not-a-jwt"

	_, err := (OIDCClient{HTTPClient: fake.server.Client(), Now: fake.nowFn}).ValidateIDToken(context.Background(), fake.provider(), fake.idToken, "nonce-1")
	if err == nil {
		t.Fatal("expected invalid token error")
	}
	if strings.Contains(err.Error(), "provider-access-token") || strings.Contains(err.Error(), "provider-refresh-token") {
		t.Fatalf("provider tokens leaked in error: %v", err)
	}
}
