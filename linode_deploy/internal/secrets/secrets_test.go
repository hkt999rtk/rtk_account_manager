package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRequiredSecretsAndRedact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account-manager-staging.env")
	body := `
ACCOUNT_MANAGER_DB_PASSWORD=db-secret
JWT_ACCESS_SECRET=access-secret
JWT_REFRESH_SECRET=refresh-secret
OIDC_CLIENT_SECRET=oidc-secret
SMTP_PASSWORD=smtp-secret
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	vals, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := vals.Require("ACCOUNT_MANAGER_DB_PASSWORD", "JWT_ACCESS_SECRET", "JWT_REFRESH_SECRET"); err != nil {
		t.Fatalf("Require: %v", err)
	}
	redacted := vals.Redact("postgres://rtk_account_manager:db-secret@10.42.1.30/db? jwt=access-secret oidc=oidc-secret smtp=smtp-secret")
	for _, secret := range []string{"db-secret", "access-secret", "oidc-secret", "smtp-secret"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("redaction leaked %s in %q", secret, redacted)
		}
	}
}

func TestRequireReportsMissingKeys(t *testing.T) {
	err := Values{"JWT_ACCESS_SECRET": "x"}.Require("JWT_ACCESS_SECRET", "JWT_REFRESH_SECRET")
	if err == nil || !strings.Contains(err.Error(), "JWT_REFRESH_SECRET") {
		t.Fatalf("expected missing key error, got %v", err)
	}
}
