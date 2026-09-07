package auth

import (
	"testing"
	"time"
)

func TestPKIOIDCStepUpRequiresConfiguredVerifiedAssurance(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	claims := map[string]any{"acr": "urn:rtk:mfa", "auth_time": float64(now.Unix())}
	if got, ok := OIDCStepUp(claims, "urn:rtk:mfa", now); !ok || !got.Equal(now) {
		t.Fatal("valid assurance rejected")
	}
	if _, ok := OIDCStepUp(claims, "", now); ok {
		t.Fatal("unconfigured assurance accepted")
	}
	if _, ok := OIDCStepUp(claims, "another-provider-class", now); ok {
		t.Fatal("wrong assurance accepted")
	}
	if _, ok := OIDCStepUp(claims, "urn:rtk:mfa", now.Add(6*time.Minute)); ok {
		t.Fatal("stale step-up accepted")
	}
	claims["auth_time"] = float64(now.Add(time.Minute).Unix())
	if _, ok := OIDCStepUp(claims, "urn:rtk:mfa", now); ok {
		t.Fatal("future step-up accepted")
	}
}

func TestPKIRefreshDoesNotMintNewAssurance(t *testing.T) {
	s := NewService("access", "refresh", time.Hour, 24*time.Hour)
	token, _, err := s.IssueAssuredAccessToken("user", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	claims, err := s.ParseAccessToken(token)
	if err != nil || !claims.MFA || claims.AuthenticationTime == 0 {
		t.Fatalf("assured claims: %v", err)
	}
	refresh, _, err := s.IssueRefreshToken("user")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.ParseRefreshToken(refresh)
	if err != nil || r.MFA || r.AuthenticationTime != 0 {
		t.Fatal("refresh acquired MFA authority")
	}
	if _, err = s.SignPKIAssertion(PKIAssertionInput{UserID: "user", AuthenticationTime: time.Now().Unix()}, time.Now()); err == nil {
		t.Fatal("HS256 signer authorized PKI")
	}
}
