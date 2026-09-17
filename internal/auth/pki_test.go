package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPKIAssertionDoesNotManufactureMFA(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer := RS256TokenSigner{Signer: key, PublicKey: &key.PublicKey}
	s := NewServiceWithSigners(signer, signer, time.Minute, time.Hour)
	now := time.Now()
	for _, mfa := range []bool{false, true} {
		input := PKIAssertionInput{UserID: "user", Environment: "dev", MFA: mfa, AuthenticationTime: now.Unix()}
		token, err := s.SignPKIAssertion(input, now)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
		if err != nil {
			t.Fatal(err)
		}
		var claims map[string]any
		if err := json.Unmarshal(raw, &claims); err != nil {
			t.Fatal(err)
		}
		if claims["mfa"] != mfa || (!mfa && claims["auth_time"] != float64(0)) {
			t.Fatal("fabricated assurance", claims)
		}
	}
	if _, err := s.SignPKIAssertion(PKIAssertionInput{UserID: "user", MFA: true, AuthenticationTime: now.Add(-6 * time.Minute).Unix()}, now); err == nil {
		t.Fatal("stale assurance signed")
	}
}

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
