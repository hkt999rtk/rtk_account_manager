package config

import (
	"strings"
	"testing"
)

func TestSocialLoginConfigAllowsDisabledProvidersWithoutCredentials(t *testing.T) {
	if err := validateSocialLoginConfig(Config{}); err != nil {
		t.Fatal(err)
	}
}

func TestSocialLoginConfigRequiresCompleteEnabledProvider(t *testing.T) {
	cfg := Config{GoogleLoginEnabled: true, SocialLoginCallbackURL: "https://admin.example.test/api/auth/social/callback", SocialOAuthStateSecret: strings.Repeat("s", 32)}
	if err := validateSocialLoginConfig(cfg); err == nil || !strings.Contains(err.Error(), "GOOGLE_OAUTH_CLIENT_ID") {
		t.Fatalf("expected incomplete Google config error, got %v", err)
	}
	cfg.GoogleOAuthClientID = "client"
	cfg.GoogleOAuthClientSecret = "secret"
	if err := validateSocialLoginConfig(cfg); err != nil {
		t.Fatal(err)
	}
}
