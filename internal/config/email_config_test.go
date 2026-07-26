package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestLoadEmailWorkerCanonicalConfiguration(t *testing.T) {
	t.Chdir(t.TempDir())
	setValidEmailEnvironment(t)
	cfg, err := LoadEmailWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMTPHost != "mail.example.com" || cfg.SMTPPort != "587" ||
		cfg.SMTPUsername != "no-reply@example.com" || cfg.SMTPFromName != "Realtek Connect" ||
		cfg.SMTPEncryption != "starttls" {
		t.Fatalf("unexpected SMTP config: %+v", cfg)
	}
	if cfg.EmailOutboxPollInterval != 5*time.Second || cfg.EmailOutboxBatchSize != 20 ||
		cfg.EmailOutboxMaxAttempts != 8 || cfg.EmailOutboxRetryBase != 30*time.Second ||
		cfg.EmailOutboxRetryMax != 30*time.Minute {
		t.Fatalf("unexpected email outbox config: %+v", cfg)
	}
}

func TestProductionEmailConfigurationFailsClosed(t *testing.T) {
	t.Chdir(t.TempDir())
	setValidEmailEnvironment(t)
	t.Setenv("AUTH_TOKEN_DELIVERY", "log")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "must be smtp") {
		t.Fatalf("production log delivery error = %v", err)
	}

	setValidEmailEnvironment(t)
	t.Setenv("SMTP_ENCRYPTION", "none")
	if _, err := LoadEmailWorker(); err == nil || !strings.Contains(err.Error(), "must be starttls") {
		t.Fatalf("production plaintext SMTP error = %v", err)
	}

	setValidEmailEnvironment(t)
	t.Setenv("EMAIL_OUTBOX_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte("short")))
	if _, err := LoadEmailWorker(); err == nil || !strings.Contains(err.Error(), "exactly 32 bytes") {
		t.Fatalf("short encryption key error = %v", err)
	}
}

func TestProductionSendMailHTTPConfiguration(t *testing.T) {
	t.Chdir(t.TempDir())
	setValidSendMailHTTPEnvironment(t)
	cfg, err := LoadEmailWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthTokenDelivery != "sendmail_http" ||
		cfg.SendMailHTTPBaseURL != "https://sm.example.com" ||
		cfg.SendMailHTTPBearerToken != "opaque-secret" ||
		cfg.SendMailHTTPTimeout != 15*time.Second {
		t.Fatalf("unexpected sendmail_http config: %+v", cfg)
	}
	if cfg.SMTPHost != "" || cfg.SMTPPassword != "" {
		t.Fatalf("sendmail_http unexpectedly requires SMTP config: %+v", cfg)
	}

	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{"missing token", "SENDMAIL_HTTP_BEARER_TOKEN", "", "BEARER_TOKEN is required"},
		{"plaintext production URL", "SENDMAIL_HTTP_BASE_URL", "http://sm.example.com", "must use https"},
		{"credential URL", "SENDMAIL_HTTP_BASE_URL", "https://user:pass@sm.example.com", "credential-free"},
		{"path URL", "SENDMAIL_HTTP_BASE_URL", "https://sm.example.com/send", "must not contain a path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setValidSendMailHTTPEnvironment(t)
			t.Setenv(test.key, test.value)
			if _, err := LoadEmailWorker(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func setValidEmailEnvironment(t *testing.T) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = 9
	}
	t.Setenv("ACCOUNT_MANAGER_ENV", "production")
	t.Setenv("AUTH_TOKEN_DELIVERY", "smtp")
	t.Setenv("AUTH_TOKEN_BASE_URL", "https://account.example.com")
	t.Setenv("SMTP_HOST", "mail.example.com")
	t.Setenv("SMTP_PORT", "587")
	t.Setenv("SMTP_USERNAME", "no-reply@example.com")
	t.Setenv("SMTP_PASSWORD", "secret")
	t.Setenv("SMTP_FROM", "no-reply@example.com")
	t.Setenv("SMTP_FROM_NAME", "Realtek Connect")
	t.Setenv("SMTP_ENCRYPTION", "starttls")
	t.Setenv("EMAIL_OUTBOX_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("JWT_ACCESS_SECRET", "access")
	t.Setenv("JWT_REFRESH_SECRET", "refresh")
}

func setValidSendMailHTTPEnvironment(t *testing.T) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = 7
	}
	t.Setenv("ACCOUNT_MANAGER_ENV", "production")
	t.Setenv("AUTH_TOKEN_DELIVERY", "sendmail_http")
	t.Setenv("AUTH_TOKEN_BASE_URL", "https://account.example.com")
	t.Setenv("SENDMAIL_HTTP_BASE_URL", "https://sm.example.com")
	t.Setenv("SENDMAIL_HTTP_BEARER_TOKEN", "opaque-secret")
	t.Setenv("SENDMAIL_HTTP_TIMEOUT", "15s")
	t.Setenv("SMTP_HOST", "")
	t.Setenv("SMTP_PASSWORD", "")
	t.Setenv("SMTP_FROM", "")
	t.Setenv("EMAIL_OUTBOX_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("JWT_ACCESS_SECRET", "access")
	t.Setenv("JWT_REFRESH_SECRET", "refresh")
}
