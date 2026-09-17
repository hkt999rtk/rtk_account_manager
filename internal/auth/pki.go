package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// OIDCStepUp consumes only claims returned after ID-token verification. An
// operator must configure the provider's MFA assurance class explicitly.
func OIDCStepUp(claims map[string]any, requiredACR string, now time.Time) (time.Time, bool) {
	if requiredACR == "" || claims["acr"] != requiredACR {
		return time.Time{}, false
	}
	authTime, ok := claims["auth_time"].(float64)
	if !ok || authTime != float64(int64(authTime)) {
		return time.Time{}, false
	}
	t := time.Unix(int64(authTime), 0)
	if t.After(now.Add(30*time.Second)) || now.Sub(t) > 5*time.Minute {
		return time.Time{}, false
	}
	return t, true
}

func (s *Service) IssueAssuredAccessToken(userID string, authenticationTime time.Time) (string, time.Time, error) {
	return s.issue(Claims{UserID: userID, SubjectType: SubjectTypeUser, MFA: true, AuthenticationTime: authenticationTime.Unix()}, userID, TokenKindAccess, s.accessSecret, s.accessSigner, s.accessTTL)
}

type PKIAssertionInput struct {
	MFA                bool
	UserID             string
	Roles              []string
	AuthenticationTime int64
	Environment        string
	Method             string
	Path               string
	Body               []byte
	IdempotencyKey     string
	ActiveContext      bool
}

func (s *Service) SignPKIAssertion(input PKIAssertionInput, now time.Time) (string, error) {
	if s.accessSigner == nil || s.accessSigner.Alg() != "RS256" {
		return "", fmt.Errorf("PKI requires an RS256 platform signer")
	}
	if input.UserID == "" || (input.MFA && (input.AuthenticationTime > now.Unix()+30 || input.AuthenticationTime < now.Add(-5*time.Minute).Unix())) {
		return "", fmt.Errorf("recent MFA authentication required")
	}
	if !input.MFA {
		input.AuthenticationTime = 0
	}
	h := sha256.Sum256(input.Body)
	claims := map[string]any{"sub": input.UserID, "iss": "account-manager", "aud": "pki-controller", "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "auth_time": input.AuthenticationTime, "mfa": input.MFA, "roles": input.Roles, "environment": input.Environment, "method": input.Method, "path": input.Path, "body_sha256": hex.EncodeToString(h[:]), "idempotency_key": input.IdempotencyKey, "active_context": input.ActiveContext}
	raw, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(raw)
	signature, err := s.accessSigner.SignToken(payload)
	if err != nil {
		return "", err
	}
	return payload + "." + signature, nil
}
