package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

var (
	ErrSocialProviderNotFound      = errors.New("social login provider not found")
	ErrSocialProviderMisconfigured = errors.New("social login provider is misconfigured")
	ErrInvalidSocialIdentity       = errors.New("invalid social identity")
	ErrSocialEmailUnverified       = errors.New("social login email is not verified")
)

type SocialProvider struct {
	ID           string
	Name         string
	Protocol     string
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Enabled      bool
}

func (p SocialProvider) Validate() error {
	if !p.Enabled {
		return ErrSocialProviderNotFound
	}
	if p.ID != "google" && p.ID != "github" {
		return ErrSocialProviderNotFound
	}
	if strings.TrimSpace(p.ClientID) == "" || strings.TrimSpace(p.ClientSecret) == "" || strings.TrimSpace(p.RedirectURL) == "" {
		return ErrSocialProviderMisconfigured
	}
	callback, err := url.ParseRequestURI(p.RedirectURL)
	if err != nil || callback.Scheme == "" || callback.Host == "" {
		return ErrSocialProviderMisconfigured
	}
	return nil
}

func DerivePKCE(state, secret string) (verifier, challenge string, err error) {
	if len(secret) < 32 || state == "" {
		return "", "", ErrSocialProviderMisconfigured
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(state))
	verifier = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	digest := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(digest[:])
	return verifier, challenge, nil
}

type SocialClient struct {
	HTTPClient *http.Client
	OIDC       OIDCClient
}

func (c SocialClient) AuthorizationURL(ctx context.Context, provider SocialProvider, state, nonce, challenge string) (string, error) {
	if err := provider.Validate(); err != nil {
		return "", err
	}
	switch provider.ID {
	case "google":
		return c.OIDC.AuthorizationURLWithPKCE(ctx, OIDCProvider{
			ProviderID: provider.ID, Name: provider.Name, IssuerURL: provider.IssuerURL,
			ClientID: provider.ClientID, ClientSecret: provider.ClientSecret,
			RedirectURL: provider.RedirectURL, Scopes: []string{"openid", "email", "profile"}, Enabled: true,
		}, state, nonce, challenge)
	case "github":
		values := url.Values{
			"client_id":             {provider.ClientID},
			"redirect_uri":          {provider.RedirectURL},
			"scope":                 {"user:email"},
			"state":                 {state},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
		}
		return "https://github.com/login/oauth/authorize?" + values.Encode(), nil
	default:
		return "", ErrSocialProviderNotFound
	}
}

func (c SocialClient) ExchangeAndIdentify(ctx context.Context, provider SocialProvider, code, nonceHash, verifier string) (OIDCIdentity, error) {
	if err := provider.Validate(); err != nil {
		return OIDCIdentity{}, err
	}
	switch provider.ID {
	case "google":
		oidcProvider := OIDCProvider{
			ProviderID: provider.ID, Name: provider.Name, IssuerURL: provider.IssuerURL,
			ClientID: provider.ClientID, ClientSecret: provider.ClientSecret,
			RedirectURL: provider.RedirectURL, Scopes: []string{"openid", "email", "profile"}, Enabled: true,
		}
		tokens, err := c.OIDC.ExchangeCodeWithPKCE(ctx, oidcProvider, code, verifier)
		if err != nil {
			return OIDCIdentity{}, err
		}
		return c.OIDC.ValidateIDTokenNonceHash(ctx, oidcProvider, tokens.IDToken, nonceHash)
	case "github":
		return c.exchangeGitHub(ctx, provider, code, verifier)
	default:
		return OIDCIdentity{}, ErrSocialProviderNotFound
	}
}

func (c SocialClient) exchangeGitHub(ctx context.Context, provider SocialProvider, code, verifier string) (OIDCIdentity, error) {
	values := url.Values{
		"client_id":     {provider.ClientID},
		"client_secret": {provider.ClientSecret},
		"code":          {code},
		"redirect_uri":  {provider.RedirectURL},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(values.Encode()))
	if err != nil {
		return OIDCIdentity{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "rtk-account-manager")
	var token struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := c.doJSON(req, &token); err != nil || token.AccessToken == "" || token.Error != "" {
		return OIDCIdentity{}, ErrInvalidSocialIdentity
	}
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := c.githubGET(ctx, "https://api.github.com/user", token.AccessToken, &user); err != nil || user.ID == 0 {
		return OIDCIdentity{}, ErrInvalidSocialIdentity
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := c.githubGET(ctx, "https://api.github.com/user/emails", token.AccessToken, &emails); err != nil {
		return OIDCIdentity{}, ErrInvalidSocialIdentity
	}
	primary := ""
	for _, candidate := range emails {
		if candidate.Primary && candidate.Verified {
			primary = strings.ToLower(strings.TrimSpace(candidate.Email))
			break
		}
	}
	if primary == "" {
		return OIDCIdentity{}, ErrSocialEmailUnverified
	}
	return OIDCIdentity{
		Issuer: "https://github.com", Subject: strconv.FormatInt(user.ID, 10), Email: primary, EmailVerified: true,
		Claims: map[string]any{"login": user.Login, "name": user.Name},
	}, nil
}

func (c SocialClient) githubGET(ctx context.Context, endpoint, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "rtk-account-manager")
	return c.doJSON(req, out)
}

func (c SocialClient) doJSON(req *http.Request, out any) error {
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("social provider returned status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}
