package render

import (
	"strings"
	"testing"

	"rtk_account_manager/linode_deploy/internal/manifest"
	"rtk_account_manager/linode_deploy/internal/secrets"
)

func TestRuntimeEnvRendersDefaultsAndSecretsWithoutLeakingRawValuesInReport(t *testing.T) {
	m, err := manifest.Load("../../configs/account-manager-staging.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	vals := secrets.Values{
		"ACCOUNT_MANAGER_DB_PASSWORD":         "db-secret",
		"JWT_ACCESS_SECRET":                   "access-secret",
		"JWT_REFRESH_SECRET":                  "refresh-secret",
		"JWT_SIGNER_PROVIDER":                 "pkcs11",
		"JWT_ACCESS_PKCS11_MODULE_PATH":       "/usr/lib/softhsm/libsofthsm2.so",
		"JWT_ACCESS_PKCS11_TOKEN_LABEL":       "jwt-access",
		"JWT_ACCESS_PKCS11_PIN":               "access-pin",
		"JWT_ACCESS_PKCS11_KEY_LABEL":         "jwt-access-key",
		"JWT_REFRESH_PKCS11_MODULE_PATH":      "/usr/lib/softhsm/libsofthsm2.so",
		"JWT_REFRESH_PKCS11_TOKEN_LABEL":      "jwt-refresh",
		"JWT_REFRESH_PKCS11_PIN":              "refresh-pin",
		"JWT_REFRESH_PKCS11_KEY_LABEL":        "jwt-refresh-key",
		"OIDC_CLIENT_SECRET":                  "oidc-secret",
		"ACCOUNT_MANAGER_INTERNAL_AUTH_TOKEN": "internal-auth-secret",
	}
	env, report := RuntimeEnv(m, vals, Options{SkipOIDC: false})
	for _, want := range []string{
		"PORT=18081",
		"DATABASE_URL=postgres://rtk_account_manager:db-secret@10.42.1.30:5432/rtk_account_manager?sslmode=disable",
		"ACCOUNT_MANAGER_INTERNAL_AUTH_TOKEN=internal-auth-secret",
		"JWT_SIGNER_PROVIDER=pkcs11",
		"JWT_ACCESS_PKCS11_KEY_LABEL=jwt-access-key",
		"OIDC_REDIRECT_URL=https://account-manager-staging.realtekconnect.com/v1/auth/oidc/keycloak/callback",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("env missing %q:\n%s", want, env)
		}
	}
	for _, leaked := range []string{"db-secret", "access-secret", "refresh-secret", "access-pin", "refresh-pin", "oidc-secret", "internal-auth-secret"} {
		if strings.Contains(report, leaked) {
			t.Fatalf("report leaked %q:\n%s", leaked, report)
		}
	}
	if !strings.Contains(report, "JWT_ACCESS_PKCS11_PIN=<redacted>") || !strings.Contains(report, "JWT_REFRESH_PKCS11_PIN=<redacted>") {
		t.Fatalf("report missing redacted pkcs11 pins:\n%s", report)
	}
	if !strings.Contains(report, "ACCOUNT_MANAGER_INTERNAL_AUTH_TOKEN=<redacted>") {
		t.Fatalf("report missing redacted internal auth token:\n%s", report)
	}
	if !strings.Contains(report, "OIDC_CLIENT_SECRET=<redacted>") {
		t.Fatalf("report missing redacted OIDC key:\n%s", report)
	}
}

func TestRuntimeEnvKeepsLogBrokerWhenWorkersAreEnabled(t *testing.T) {
	m, err := manifest.Load("../../configs/account-manager-staging.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	vals := secrets.Values{
		"ACCOUNT_MANAGER_DB_PASSWORD": "db-secret",
		"JWT_ACCESS_SECRET":           "access-secret",
		"JWT_REFRESH_SECRET":          "refresh-secret",
	}
	env, _ := RuntimeEnv(m, vals, Options{SkipOIDC: true, EnableWorkers: true})
	for _, want := range []string{
		"CROSS_SERVICE_BROKER=log",
		"ACCOUNT_VIDEO_COMMANDS_STREAM=account.video.commands",
		"VIDEO_ACCOUNT_EVENTS_STREAM=video.account.events",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("worker env missing %q:\n%s", want, env)
		}
	}
	for _, removed := range []string{"CROSS_SERVICE_NATS_URL", "CROSS_SERVICE_NATS_NAME", "CROSS_SERVICE_PARTITION_COUNT"} {
		if strings.Contains(env, removed) {
			t.Fatalf("worker env still contains removed %q:\n%s", removed, env)
		}
	}
}
