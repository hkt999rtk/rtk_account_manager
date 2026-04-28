package config

import (
	"os"
	"path/filepath"
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
	if cfg.CrossServiceBroker != "log" {
		t.Fatalf("unexpected broker: %q", cfg.CrossServiceBroker)
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
	t.Setenv("CROSS_SERVICE_BROKER", "log")
	t.Setenv("CROSS_SERVICE_MAX_ATTEMPTS", "7")
	t.Setenv("CROSS_SERVICE_POLL_INTERVAL", "9s")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CrossServiceMaxAttempts != 7 {
		t.Fatalf("unexpected max attempts: %d", cfg.CrossServiceMaxAttempts)
	}
	if cfg.CrossServicePollInterval != 9*time.Second {
		t.Fatalf("unexpected poll interval: %s", cfg.CrossServicePollInterval)
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
