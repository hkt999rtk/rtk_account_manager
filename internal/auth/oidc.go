package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"rtk_account_manager/internal/model"
)

var (
	ErrOIDCDisabled              = errors.New("oidc is disabled")
	ErrOIDCProviderNotFound      = errors.New("oidc provider not found")
	ErrOIDCProviderMisconfigured = errors.New("oidc provider is misconfigured")
	ErrInvalidOIDCToken          = errors.New("invalid oidc token")
	ErrUnverifiedOIDCEmail       = errors.New("unverified oidc email")
)

type OIDCEnvConfig struct {
	Enabled       bool
	ProviderID    string
	ProviderName  string
	IssuerURL     string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	Scopes        []string
	AutoLinkEmail bool
}

type OIDCProvider struct {
	ID            string
	ProviderID    string
	Name          string
	IssuerURL     string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	Scopes        []string
	Enabled       bool
	AutoLinkEmail bool
}

type EnabledIdentityProviderLookup interface {
	GetEnabledIdentityProvider(ctx context.Context) (model.IdentityProvider, error)
}

type ProviderResolver struct {
	Store      EnabledIdentityProviderLookup
	Env        OIDCEnvConfig
	IsNotFound func(error) bool
	Getenv     func(string) string
}

func (r ProviderResolver) Resolve(ctx context.Context) (OIDCProvider, error) {
	if !r.Env.Enabled {
		return OIDCProvider{}, ErrOIDCDisabled
	}
	if r.Store != nil {
		provider, err := r.Store.GetEnabledIdentityProvider(ctx)
		if err == nil {
			return r.fromModel(provider)
		}
		if r.IsNotFound == nil || !r.IsNotFound(err) {
			return OIDCProvider{}, err
		}
	}
	return r.fromEnv()
}

func (r ProviderResolver) fromModel(provider model.IdentityProvider) (OIDCProvider, error) {
	if provider.Type != model.IdentityProviderTypeOIDC || !provider.Enabled {
		return OIDCProvider{}, ErrOIDCProviderNotFound
	}
	resolved := OIDCProvider{
		ID:            provider.ID,
		ProviderID:    provider.ProviderID,
		Name:          provider.Name,
		IssuerURL:     provider.IssuerURL,
		ClientID:      provider.ClientID,
		RedirectURL:   r.Env.RedirectURL,
		Scopes:        append([]string(nil), provider.Scopes...),
		Enabled:       provider.Enabled,
		AutoLinkEmail: r.Env.AutoLinkEmail,
	}
	if provider.ClientSecretRef != nil {
		secret, err := r.resolveSecretRef(*provider.ClientSecretRef)
		if err != nil {
			return OIDCProvider{}, err
		}
		resolved.ClientSecret = secret
	}
	if len(resolved.Scopes) == 0 {
		resolved.Scopes = append([]string(nil), r.Env.Scopes...)
	}
	if err := resolved.Validate(); err != nil {
		return OIDCProvider{}, err
	}
	return resolved, nil
}

func (r ProviderResolver) fromEnv() (OIDCProvider, error) {
	provider := OIDCProvider{
		ProviderID:    defaultString(r.Env.ProviderID, "keycloak"),
		Name:          defaultString(r.Env.ProviderName, "Keycloak"),
		IssuerURL:     r.Env.IssuerURL,
		ClientID:      r.Env.ClientID,
		ClientSecret:  r.Env.ClientSecret,
		RedirectURL:   r.Env.RedirectURL,
		Scopes:        append([]string(nil), r.Env.Scopes...),
		Enabled:       r.Env.Enabled,
		AutoLinkEmail: r.Env.AutoLinkEmail,
	}
	if len(provider.Scopes) == 0 {
		provider.Scopes = []string{"openid", "email", "profile"}
	}
	if err := provider.Validate(); err != nil {
		return OIDCProvider{}, err
	}
	return provider, nil
}

func (r ProviderResolver) resolveSecretRef(ref string) (string, error) {
	const prefix = "env:"
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("%w: unsupported client secret reference", ErrOIDCProviderMisconfigured)
	}
	key := strings.TrimPrefix(ref, prefix)
	if key == "" {
		return "", fmt.Errorf("%w: empty client secret reference", ErrOIDCProviderMisconfigured)
	}
	getenv := r.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	value := getenv(key)
	if value == "" {
		return "", fmt.Errorf("%w: client secret reference is unset", ErrOIDCProviderMisconfigured)
	}
	return value, nil
}

func (p OIDCProvider) Validate() error {
	if !p.Enabled {
		return ErrOIDCDisabled
	}
	if strings.TrimSpace(p.ProviderID) == "" || strings.TrimSpace(p.IssuerURL) == "" || strings.TrimSpace(p.ClientID) == "" || strings.TrimSpace(p.ClientSecret) == "" || strings.TrimSpace(p.RedirectURL) == "" {
		return ErrOIDCProviderMisconfigured
	}
	if _, err := url.ParseRequestURI(p.RedirectURL); err != nil {
		return fmt.Errorf("%w: invalid redirect url", ErrOIDCProviderMisconfigured)
	}
	issuer, err := url.ParseRequestURI(p.IssuerURL)
	if err != nil || issuer.Scheme == "" || issuer.Host == "" {
		return fmt.Errorf("%w: invalid issuer url", ErrOIDCProviderMisconfigured)
	}
	return nil
}

type OIDCClient struct {
	HTTPClient *http.Client
	Now        func() time.Time
}

type OIDCDiscovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

type OIDCTokenResponse struct {
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
}

type OIDCIdentity struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	Claims        map[string]any
}

type oidcClaims struct {
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Nonce         string `json:"nonce"`
	jwt.RegisteredClaims
}

func (c OIDCClient) AuthorizationURL(ctx context.Context, provider OIDCProvider, state, nonce string) (string, error) {
	discovery, err := c.Discover(ctx, provider)
	if err != nil {
		return "", err
	}
	values := url.Values{}
	values.Set("response_type", "code")
	values.Set("client_id", provider.ClientID)
	values.Set("redirect_uri", provider.RedirectURL)
	values.Set("scope", strings.Join(provider.Scopes, " "))
	values.Set("state", state)
	values.Set("nonce", nonce)
	authURL := discovery.AuthorizationEndpoint
	separator := "?"
	if strings.Contains(authURL, "?") {
		separator = "&"
	}
	return authURL + separator + values.Encode(), nil
}

func (c OIDCClient) ExchangeAndValidate(ctx context.Context, provider OIDCProvider, code, expectedNonce string) (OIDCIdentity, OIDCTokenResponse, error) {
	tokens, err := c.ExchangeCode(ctx, provider, code)
	if err != nil {
		return OIDCIdentity{}, OIDCTokenResponse{}, err
	}
	identity, err := c.ValidateIDToken(ctx, provider, tokens.IDToken, expectedNonce)
	if err != nil {
		return OIDCIdentity{}, OIDCTokenResponse{}, err
	}
	return identity, tokens, nil
}

func (c OIDCClient) ExchangeCode(ctx context.Context, provider OIDCProvider, code string) (OIDCTokenResponse, error) {
	discovery, err := c.Discover(ctx, provider)
	if err != nil {
		return OIDCTokenResponse{}, err
	}
	values := url.Values{}
	values.Set("grant_type", "authorization_code")
	values.Set("code", code)
	values.Set("redirect_uri", provider.RedirectURL)
	values.Set("client_id", provider.ClientID)
	values.Set("client_secret", provider.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, discovery.TokenEndpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return OIDCTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return OIDCTokenResponse{}, fmt.Errorf("%w: token exchange failed", ErrOIDCProviderMisconfigured)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return OIDCTokenResponse{}, fmt.Errorf("%w: token endpoint returned %d", ErrOIDCProviderMisconfigured, resp.StatusCode)
	}
	var tokens OIDCTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return OIDCTokenResponse{}, fmt.Errorf("%w: invalid token response", ErrOIDCProviderMisconfigured)
	}
	if tokens.IDToken == "" {
		return OIDCTokenResponse{}, fmt.Errorf("%w: missing id token", ErrInvalidOIDCToken)
	}
	return tokens, nil
}

func (c OIDCClient) ValidateIDToken(ctx context.Context, provider OIDCProvider, idToken, expectedNonce string) (OIDCIdentity, error) {
	discovery, err := c.Discover(ctx, provider)
	if err != nil {
		return OIDCIdentity{}, err
	}
	keySet, err := c.fetchJWKS(ctx, discovery.JWKSURI)
	if err != nil {
		return OIDCIdentity{}, err
	}
	claims := &oidcClaims{}
	token, err := jwt.ParseWithClaims(idToken, claims, keySet.keyfunc, jwt.WithIssuer(normalizeIssuer(provider.IssuerURL)), jwt.WithAudience(provider.ClientID), jwt.WithExpirationRequired(), jwt.WithTimeFunc(c.now))
	if err != nil {
		return OIDCIdentity{}, fmt.Errorf("%w: %v", ErrInvalidOIDCToken, err)
	}
	if !token.Valid {
		return OIDCIdentity{}, ErrInvalidOIDCToken
	}
	if claims.Nonce != expectedNonce {
		return OIDCIdentity{}, fmt.Errorf("%w: invalid nonce", ErrInvalidOIDCToken)
	}
	if claims.Subject == "" || claims.Email == "" {
		return OIDCIdentity{}, fmt.Errorf("%w: missing subject or email", ErrInvalidOIDCToken)
	}
	if !claims.EmailVerified {
		return OIDCIdentity{}, ErrUnverifiedOIDCEmail
	}
	allClaims := map[string]any{}
	parts := strings.Split(idToken, ".")
	if len(parts) == 3 {
		if payload, decodeErr := base64.RawURLEncoding.DecodeString(parts[1]); decodeErr == nil {
			_ = json.Unmarshal(payload, &allClaims)
		}
	}
	return OIDCIdentity{
		Issuer:        claims.Issuer,
		Subject:       claims.Subject,
		Email:         strings.ToLower(claims.Email),
		EmailVerified: claims.EmailVerified,
		Claims:        allClaims,
	}, nil
}

func (c OIDCClient) Discover(ctx context.Context, provider OIDCProvider) (OIDCDiscovery, error) {
	if err := provider.Validate(); err != nil {
		return OIDCDiscovery{}, err
	}
	discoveryURL := normalizeIssuer(provider.IssuerURL) + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return OIDCDiscovery{}, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return OIDCDiscovery{}, fmt.Errorf("%w: discovery failed", ErrOIDCProviderMisconfigured)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return OIDCDiscovery{}, fmt.Errorf("%w: discovery returned %d", ErrOIDCProviderMisconfigured, resp.StatusCode)
	}
	var discovery OIDCDiscovery
	if err := json.NewDecoder(resp.Body).Decode(&discovery); err != nil {
		return OIDCDiscovery{}, fmt.Errorf("%w: invalid discovery document", ErrOIDCProviderMisconfigured)
	}
	if discovery.Issuer != normalizeIssuer(provider.IssuerURL) || discovery.AuthorizationEndpoint == "" || discovery.TokenEndpoint == "" || discovery.JWKSURI == "" {
		return OIDCDiscovery{}, fmt.Errorf("%w: incomplete discovery document", ErrOIDCProviderMisconfigured)
	}
	return discovery, nil
}

func (c OIDCClient) fetchJWKS(ctx context.Context, jwksURI string) (jwkSet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return jwkSet{}, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return jwkSet{}, fmt.Errorf("%w: jwks fetch failed", ErrOIDCProviderMisconfigured)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return jwkSet{}, fmt.Errorf("%w: jwks endpoint returned %d", ErrOIDCProviderMisconfigured, resp.StatusCode)
	}
	var set jwkSet
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return jwkSet{}, fmt.Errorf("%w: invalid jwks response", ErrOIDCProviderMisconfigured)
	}
	if len(set.Keys) == 0 {
		return jwkSet{}, fmt.Errorf("%w: empty jwks response", ErrOIDCProviderMisconfigured)
	}
	return set, nil
}

func (c OIDCClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c OIDCClient) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	KeyID     string `json:"kid"`
	KeyType   string `json:"kty"`
	Algorithm string `json:"alg"`
	Use       string `json:"use"`
	N         string `json:"n"`
	E         string `json:"e"`
}

func (s jwkSet) keyfunc(token *jwt.Token) (any, error) {
	if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
		return nil, fmt.Errorf("%w: unexpected signing method", ErrInvalidOIDCToken)
	}
	keyID, _ := token.Header["kid"].(string)
	for _, key := range s.Keys {
		if key.KeyID != keyID || key.KeyType != "RSA" {
			continue
		}
		publicKey, err := key.publicKey()
		if err != nil {
			return nil, fmt.Errorf("%w: invalid jwk", ErrOIDCProviderMisconfigured)
		}
		return publicKey, nil
	}
	return nil, fmt.Errorf("%w: signing key not found", ErrInvalidOIDCToken)
}

func (k jwk) publicKey() (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, err
	}
	exponent := big.NewInt(0).SetBytes(eBytes).Int64()
	if exponent <= 0 {
		return nil, fmt.Errorf("invalid exponent")
	}
	return &rsa.PublicKey{
		N: big.NewInt(0).SetBytes(nBytes),
		E: int(exponent),
	}, nil
}

func normalizeIssuer(issuer string) string {
	return strings.TrimRight(issuer, "/")
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
