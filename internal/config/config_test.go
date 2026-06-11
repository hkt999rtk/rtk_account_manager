package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLoadDotEnvSetsMissingValuesAndPreservesExistingEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(`
# comment
DOTENV_ALPHA=from-file
DOTENV_BETA="quoted value"
DOTENV_GAMMA='single quoted'
export DOTENV_EXISTING=from-file
`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DOTENV_EXISTING", "from-env")
	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}

	if got := os.Getenv("DOTENV_ALPHA"); got != "from-file" {
		t.Fatalf("expected DOTENV_ALPHA from file, got %q", got)
	}
	if got := os.Getenv("DOTENV_BETA"); got != "quoted value" {
		t.Fatalf("expected DOTENV_BETA to be unquoted, got %q", got)
	}
	if got := os.Getenv("DOTENV_GAMMA"); got != "single quoted" {
		t.Fatalf("expected DOTENV_GAMMA to be unquoted, got %q", got)
	}
	if got := os.Getenv("DOTENV_EXISTING"); got != "from-env" {
		t.Fatalf("expected existing env to win, got %q", got)
	}
}

func TestLoadReadsEnvironmentAndDurations(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("JWT_ACCESS_SECRET", "access")
	t.Setenv("JWT_REFRESH_SECRET", "refresh")
	t.Setenv("ACCESS_TOKEN_TTL", "10m")
	t.Setenv("REFRESH_TOKEN_TTL", "24h")
	t.Setenv("PORT", "9090")
	t.Setenv("AUTH_TOKEN_DELIVERY", "log")
	t.Setenv("AUTH_TOKEN_BASE_URL", "https://admin.example.test")
	t.Setenv("SMTP_HOST", "smtp.example")
	t.Setenv("SMTP_PORT", "2525")
	t.Setenv("SMTP_USERNAME", "smtp-user")
	t.Setenv("SMTP_PASSWORD", "smtp-pass")
	t.Setenv("SMTP_FROM", "noreply@example.com")
	t.Setenv("AZURE_EVENTHUB_CONNECTION_STRING", "Endpoint=sb://example/")
	t.Setenv("AZURE_EVENTHUB_CHECKPOINT_FILE", "/tmp/eventhub-checkpoints.json")
	t.Setenv("OIDC_ENABLED", "true")
	t.Setenv("OIDC_PROVIDER_ID", "corp-keycloak")
	t.Setenv("OIDC_PROVIDER_NAME", "Corp Keycloak")
	t.Setenv("OIDC_ISSUER_URL", "https://sso.example.test/realms/account")
	t.Setenv("OIDC_CLIENT_ID", "rtk-account-manager")
	t.Setenv("OIDC_CLIENT_SECRET", "oidc-secret")
	t.Setenv("OIDC_REDIRECT_URL", "https://api.example.test/v1/auth/oidc/corp-keycloak/callback")
	t.Setenv("OIDC_SCOPES", "openid email profile offline_access")
	t.Setenv("OIDC_AUTO_LINK_EMAIL", "true")
	t.Setenv("ACCOUNT_MANAGER_ENV", "staging")
	t.Setenv("ACCOUNT_MANAGER_VERSION", "2026.06.01+test")
	t.Setenv("ACCOUNT_MANAGER_LOG_LEVEL", "debug")
	t.Setenv("ACCOUNT_MANAGER_LOG_DEVELOPMENT", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "postgres://example" {
		t.Fatalf("unexpected database URL: %q", cfg.DatabaseURL)
	}
	if cfg.AccessSecret != "access" || cfg.RefreshSecret != "refresh" {
		t.Fatalf("unexpected secrets: %+v", cfg)
	}
	if cfg.AccessTokenTTL != 10*time.Minute {
		t.Fatalf("unexpected access TTL: %s", cfg.AccessTokenTTL)
	}
	if cfg.RefreshTokenTTL != 24*time.Hour {
		t.Fatalf("unexpected refresh TTL: %s", cfg.RefreshTokenTTL)
	}
	if cfg.Port != "9090" {
		t.Fatalf("unexpected port: %q", cfg.Port)
	}
	if cfg.AuthTokenDelivery != "log" {
		t.Fatalf("unexpected auth token delivery: %q", cfg.AuthTokenDelivery)
	}
	if cfg.AuthTokenBaseURL != "https://admin.example.test" {
		t.Fatalf("unexpected auth token base URL: %q", cfg.AuthTokenBaseURL)
	}
	if cfg.SMTPHost != "smtp.example" || cfg.SMTPPort != "2525" || cfg.SMTPUsername != "smtp-user" || cfg.SMTPPassword != "smtp-pass" || cfg.SMTPFrom != "noreply@example.com" {
		t.Fatalf("unexpected smtp config: %+v", cfg)
	}
	if cfg.CrossServiceBroker != "log" {
		t.Fatalf("unexpected broker: %q", cfg.CrossServiceBroker)
	}
	if cfg.AzureEventHubConnectionString != "Endpoint=sb://example/" {
		t.Fatalf("unexpected azure connection string: %q", cfg.AzureEventHubConnectionString)
	}
	if cfg.AzureEventHubCheckpointFile != "/tmp/eventhub-checkpoints.json" {
		t.Fatalf("unexpected azure checkpoint file: %q", cfg.AzureEventHubCheckpointFile)
	}
	if cfg.AccountVideoCommandsStream != "account.video.commands" {
		t.Fatalf("unexpected command stream: %q", cfg.AccountVideoCommandsStream)
	}
	if cfg.VideoAccountEventsStream != "video.account.events" {
		t.Fatalf("unexpected event stream: %q", cfg.VideoAccountEventsStream)
	}
	if cfg.CrossServiceConsumerGroup != "rtk_account_manager" {
		t.Fatalf("unexpected consumer group: %q", cfg.CrossServiceConsumerGroup)
	}
	if cfg.CrossServiceMaxAttempts != 5 {
		t.Fatalf("unexpected max attempts: %d", cfg.CrossServiceMaxAttempts)
	}
	if cfg.CrossServicePollInterval != 5*time.Second {
		t.Fatalf("unexpected poll interval: %s", cfg.CrossServicePollInterval)
	}
	if !cfg.OIDCEnabled {
		t.Fatal("expected OIDC to be enabled")
	}
	if cfg.OIDCProviderID != "corp-keycloak" || cfg.OIDCProviderName != "Corp Keycloak" {
		t.Fatalf("unexpected OIDC provider metadata: %+v", cfg)
	}
	if cfg.OIDCIssuerURL != "https://sso.example.test/realms/account" || cfg.OIDCClientID != "rtk-account-manager" || cfg.OIDCClientSecret != "oidc-secret" {
		t.Fatalf("unexpected OIDC issuer/client config: %+v", cfg)
	}
	if cfg.OIDCRedirectURL != "https://api.example.test/v1/auth/oidc/corp-keycloak/callback" {
		t.Fatalf("unexpected OIDC redirect URL: %q", cfg.OIDCRedirectURL)
	}
	if !reflect.DeepEqual(cfg.OIDCScopes, []string{"openid", "email", "profile", "offline_access"}) {
		t.Fatalf("unexpected OIDC scopes: %#v", cfg.OIDCScopes)
	}
	if !cfg.OIDCAutoLinkEmail {
		t.Fatal("expected OIDC auto-link to be enabled")
	}
	if cfg.LogEnv != "staging" || cfg.LogVersion != "2026.06.01+test" || cfg.LogLevel != "debug" || !cfg.LogDevelopment {
		t.Fatalf("unexpected logging config: %+v", cfg)
	}
}

func TestLoadFallsBackForInvalidDurations(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("JWT_ACCESS_SECRET", "access")
	t.Setenv("JWT_REFRESH_SECRET", "refresh")
	t.Setenv("ACCESS_TOKEN_TTL", "invalid")
	t.Setenv("REFRESH_TOKEN_TTL", "invalid")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccessTokenTTL != 15*time.Minute {
		t.Fatalf("expected default access TTL, got %s", cfg.AccessTokenTTL)
	}
	if cfg.RefreshTokenTTL != 30*24*time.Hour {
		t.Fatalf("expected default refresh TTL, got %s", cfg.RefreshTokenTTL)
	}
}

func TestLoadWorkerAllowsMissingJWTSecrets(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("JWT_ACCESS_SECRET", "")
	t.Setenv("JWT_REFRESH_SECRET", "")
	t.Setenv("CROSS_SERVICE_BROKER", "azure_eventhubs")
	t.Setenv("CROSS_SERVICE_MAX_ATTEMPTS", "7")
	t.Setenv("CROSS_SERVICE_POLL_INTERVAL", "9s")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CrossServiceBroker != "azure_eventhubs" {
		t.Fatalf("unexpected broker: %q", cfg.CrossServiceBroker)
	}
	if cfg.CrossServiceMaxAttempts != 7 {
		t.Fatalf("unexpected max attempts: %d", cfg.CrossServiceMaxAttempts)
	}
	if cfg.CrossServicePollInterval != 9*time.Second {
		t.Fatalf("unexpected poll interval: %s", cfg.CrossServicePollInterval)
	}
}

func TestLoadWorkerFallsBackForInvalidMaxAttempts(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("CROSS_SERVICE_MAX_ATTEMPTS", "0")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CrossServiceMaxAttempts != 5 {
		t.Fatalf("expected default max attempts, got %d", cfg.CrossServiceMaxAttempts)
	}
}

func TestLoadUsesOIDCDefaultsAndBooleanFallbacks(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("OIDC_ENABLED", "not-bool")
	t.Setenv("OIDC_AUTO_LINK_EMAIL", "not-bool")
	t.Setenv("OIDC_SCOPES", "")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OIDCEnabled {
		t.Fatal("expected OIDC to default disabled for invalid boolean")
	}
	if cfg.OIDCAutoLinkEmail {
		t.Fatal("expected OIDC auto-link to default disabled for invalid boolean")
	}
	if cfg.OIDCProviderID != "keycloak" || cfg.OIDCProviderName != "Keycloak" {
		t.Fatalf("unexpected default provider metadata: %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.OIDCScopes, []string{"openid", "email", "profile"}) {
		t.Fatalf("unexpected default scopes: %#v", cfg.OIDCScopes)
	}
}

func TestLoadRequiresJWTSecrets(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("JWT_ACCESS_SECRET", "")
	t.Setenv("JWT_REFRESH_SECRET", "refresh")
	if _, err := Load(); err == nil {
		t.Fatal("expected missing access secret error")
	}

	t.Setenv("JWT_ACCESS_SECRET", "access")
	t.Setenv("JWT_REFRESH_SECRET", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected missing refresh secret error")
	}
}

func TestLoadAcceptsPEMJWTSignerWithoutSharedSecrets(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("JWT_SIGNER_PROVIDER", "pem")
	t.Setenv("JWT_ACCESS_PRIVATE_KEY_PATH", "/tmp/access.key")
	t.Setenv("JWT_ACCESS_PUBLIC_KEY_PATH", "/tmp/access.pub")
	t.Setenv("JWT_REFRESH_PRIVATE_KEY_PATH", "/tmp/refresh.key")
	t.Setenv("JWT_REFRESH_PUBLIC_KEY_PATH", "/tmp/refresh.pub")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.JWTSignerProvider != "pem" || cfg.JWTAccessPrivateKeyPath != "/tmp/access.key" || cfg.JWTRefreshPublicKeyPath != "/tmp/refresh.pub" {
		t.Fatalf("unexpected jwt signer config: %+v", cfg)
	}
}

func TestLoadRejectsIncompletePEMJWTSigner(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("JWT_SIGNER_PROVIDER", "pem")
	t.Setenv("JWT_ACCESS_PRIVATE_KEY_PATH", "/tmp/access.key")

	if _, err := Load(); err == nil {
		t.Fatal("expected incomplete PEM signer config error")
	}
}

func TestLoadRejectsUnknownJWTSignerProvider(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("JWT_SIGNER_PROVIDER", "hsm-direct")

	if _, err := Load(); err == nil {
		t.Fatal("expected unknown JWT signer provider error")
	}
}

func TestLoadAcceptsPKCS11JWTSignerWithoutSharedSecrets(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("JWT_SIGNER_PROVIDER", "pkcs11")
	t.Setenv("JWT_ACCESS_PKCS11_MODULE_PATH", "/usr/lib/softhsm/libsofthsm2.so")
	t.Setenv("JWT_ACCESS_PKCS11_TOKEN_LABEL", "access-token")
	t.Setenv("JWT_ACCESS_PKCS11_PIN", "1234")
	t.Setenv("JWT_ACCESS_PKCS11_KEY_LABEL", "access-key")
	t.Setenv("JWT_REFRESH_PKCS11_MODULE_PATH", "/usr/lib/softhsm/libsofthsm2.so")
	t.Setenv("JWT_REFRESH_PKCS11_SLOT_ID", "2")
	t.Setenv("JWT_REFRESH_PKCS11_PIN", "5678")
	t.Setenv("JWT_REFRESH_PKCS11_KEY_LABEL", "refresh-key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.JWTSignerProvider != "pkcs11" || cfg.JWTAccessPKCS11TokenLabel != "access-token" || cfg.JWTRefreshPKCS11SlotID != "2" {
		t.Fatalf("unexpected jwt pkcs11 config: %+v", cfg)
	}
}

func TestLoadRejectsIncompletePKCS11JWTSigner(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("JWT_SIGNER_PROVIDER", "pkcs11")
	setCompletePKCS11JWTSignerEnv(t)

	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "missing module", key: "JWT_ACCESS_PKCS11_MODULE_PATH"},
		{name: "missing token and slot", key: "JWT_ACCESS_PKCS11_TOKEN_LABEL"},
		{name: "missing pin", key: "JWT_ACCESS_PKCS11_PIN"},
		{name: "missing key label", key: "JWT_ACCESS_PKCS11_KEY_LABEL"},
		{name: "missing refresh key label", key: "JWT_REFRESH_PKCS11_KEY_LABEL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("JWT_SIGNER_PROVIDER", "pkcs11")
			setCompletePKCS11JWTSignerEnv(t)
			t.Setenv(tc.key, "")
			if tc.key == "JWT_ACCESS_PKCS11_TOKEN_LABEL" {
				t.Setenv("JWT_ACCESS_PKCS11_SLOT_ID", "")
			}

			if _, err := Load(); err == nil {
				t.Fatal("expected incomplete PKCS#11 signer config error")
			}
		})
	}
}

func setCompletePKCS11JWTSignerEnv(t *testing.T) {
	t.Helper()
	t.Setenv("JWT_ACCESS_PKCS11_MODULE_PATH", "/usr/lib/softhsm/libsofthsm2.so")
	t.Setenv("JWT_ACCESS_PKCS11_TOKEN_LABEL", "access-token")
	t.Setenv("JWT_ACCESS_PKCS11_PIN", "1234")
	t.Setenv("JWT_ACCESS_PKCS11_KEY_LABEL", "access-key")
	t.Setenv("JWT_REFRESH_PKCS11_MODULE_PATH", "/usr/lib/softhsm/libsofthsm2.so")
	t.Setenv("JWT_REFRESH_PKCS11_SLOT_ID", "2")
	t.Setenv("JWT_REFRESH_PKCS11_PIN", "5678")
	t.Setenv("JWT_REFRESH_PKCS11_KEY_LABEL", "refresh-key")
}
