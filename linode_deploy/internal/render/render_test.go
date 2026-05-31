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
		"ACCOUNT_MANAGER_DB_PASSWORD": "db-secret",
		"JWT_ACCESS_SECRET":           "access-secret",
		"JWT_REFRESH_SECRET":          "refresh-secret",
		"OIDC_CLIENT_SECRET":          "oidc-secret",
	}
	env, report := RuntimeEnv(m, vals, Options{SkipOIDC: false})
	for _, want := range []string{
		"PORT=18081",
		"DATABASE_URL=postgres://rtk_account_manager:db-secret@10.42.1.30:5432/rtk_account_manager?sslmode=disable",
		"OIDC_REDIRECT_URL=https://account-manager-staging.realtekconnect.com/v1/auth/oidc/keycloak/callback",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("env missing %q:\n%s", want, env)
		}
	}
	for _, leaked := range []string{"db-secret", "access-secret", "refresh-secret", "oidc-secret"} {
		if strings.Contains(report, leaked) {
			t.Fatalf("report leaked %q:\n%s", leaked, report)
		}
	}
	if !strings.Contains(report, "OIDC_CLIENT_SECRET=<redacted>") {
		t.Fatalf("report missing redacted OIDC key:\n%s", report)
	}
}

func TestRuntimeEnvRendersNATSWhenWorkersAreEnabled(t *testing.T) {
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
		"CROSS_SERVICE_BROKER=nats",
		"CROSS_SERVICE_NATS_URL=nats://10.42.1.30:4222",
		"CROSS_SERVICE_NATS_NAME=account-manager-staging-account-manager",
		"CROSS_SERVICE_PARTITION_COUNT=4",
		"ACCOUNT_VIDEO_COMMANDS_STREAM=account.video.commands",
		"VIDEO_ACCOUNT_EVENTS_STREAM=video.account.events",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("worker env missing %q:\n%s", want, env)
		}
	}
}
