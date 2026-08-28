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
	if cfg.SendMailHTTPBaseURL != "https://sm.example.com" || cfg.SendMailHTTPBearerToken != "opaque-secret" {
		t.Fatalf("unexpected sendmail_http config: %+v", cfg)
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
	if cfg.SendMailHTTPBaseURL != "https://sm.example.com" ||
		cfg.SendMailHTTPBearerToken != "opaque-secret" ||
		cfg.SendMailHTTPTimeout != 15*time.Second {
		t.Fatalf("unexpected sendmail_http config: %+v", cfg)
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
		{"invalid timeout", "SENDMAIL_HTTP_TIMEOUT", "never", "positive duration"},
		{"zero timeout", "SENDMAIL_HTTP_TIMEOUT", "0s", "positive duration"},
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

func TestEmailOutboxRetryConfigurationFailsClosed(t *testing.T) {
	t.Chdir(t.TempDir())
	tests := []struct {
		key   string
		value string
		want  string
	}{
		{"EMAIL_OUTBOX_POLL_INTERVAL", "0s", "positive duration"},
		{"EMAIL_OUTBOX_BATCH_SIZE", "invalid", "positive integer"},
		{"EMAIL_OUTBOX_MAX_ATTEMPTS", "0", "positive integer"},
		{"EMAIL_OUTBOX_RETRY_BASE", "never", "positive duration"},
		{"EMAIL_OUTBOX_RETRY_MAX", "-1s", "positive duration"},
	}
	for _, test := range tests {
		t.Run(test.key, func(t *testing.T) {
			setValidEmailEnvironment(t)
			t.Setenv(test.key, test.value)
			if _, err := LoadEmailWorker(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}

	setValidEmailEnvironment(t)
	t.Setenv("EMAIL_OUTBOX_RETRY_BASE", "2m")
	t.Setenv("EMAIL_OUTBOX_RETRY_MAX", "1m")
	if _, err := LoadEmailWorker(); err == nil || !strings.Contains(err.Error(), "greater than or equal") {
		t.Fatalf("retry ordering error = %v", err)
	}
}

func setValidEmailEnvironment(t *testing.T) {
	setValidSendMailHTTPEnvironment(t)
}

func setValidSendMailHTTPEnvironment(t *testing.T) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = 7
	}
	t.Setenv("ACCOUNT_MANAGER_ENV", "production")
	t.Setenv("AUTH_TOKEN_BASE_URL", "https://account.example.com")
	t.Setenv("SENDMAIL_HTTP_BASE_URL", "https://sm.example.com")
	t.Setenv("SENDMAIL_HTTP_BEARER_TOKEN", "opaque-secret")
	t.Setenv("SENDMAIL_HTTP_TIMEOUT", "15s")
	t.Setenv("EMAIL_OUTBOX_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("JWT_ACCESS_SECRET", "access")
	t.Setenv("JWT_REFRESH_SECRET", "refresh")
}
