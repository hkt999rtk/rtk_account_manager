package logging

import (
	"errors"
	"testing"

	"github.com/hkt999rtk/rtk_cloud_logger"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"rtk_account_manager/internal/config"
)

func TestNewBuildsServiceLoggerFromConfig(t *testing.T) {
	logger, err := New(ServiceAPI, config.Config{
		LogEnv:         "test",
		LogVersion:     "build-1",
		LogLevel:       "debug",
		LogDevelopment: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer Sync(logger)

	if !logger.Core().Enabled(zapcore.DebugLevel) {
		t.Fatal("expected debug logging to be enabled")
	}
}

func TestServiceUnitMapping(t *testing.T) {
	tests := map[string]string{
		ServiceAPI:           "rtk_account_manager_api.service",
		ServiceMigrate:       "rtk_account_manager_migrate.service",
		ServiceOutboxWorker:  "rtk_account_manager_outbox_worker.service",
		ServiceInboxWorker:   "rtk_account_manager_inbox_worker.service",
		ServiceEmailWorker:   "rtk_account_manager_email_worker.service",
		ServicePaymentWorker: "rtk_account_manager_payment_worker.service",
		ServiceCleanupTokens: "rtk_account_manager_cleanup_tokens.service",
		"custom_worker":      "custom_worker.service",
	}
	for service, want := range tests {
		if got := unitName(service); got != want {
			t.Fatalf("unitName(%q) = %q, want %q", service, got, want)
		}
	}
}

func TestNewFromEnvReadsLoggingConfigWithFallbacks(t *testing.T) {
	t.Setenv("ACCOUNT_MANAGER_ENV", "staging")
	t.Setenv("ACCOUNT_MANAGER_VERSION", "build-2")
	t.Setenv("ACCOUNT_MANAGER_LOG_LEVEL", "warn")
	t.Setenv("ACCOUNT_MANAGER_LOG_DEVELOPMENT", "not-bool")

	logger := NewFromEnv(ServiceOutboxWorker)
	defer Sync(logger)

	if logger.Core().Enabled(zapcore.InfoLevel) {
		t.Fatal("expected info logging to be disabled at warn level")
	}
	if !logger.Core().Enabled(zapcore.WarnLevel) {
		t.Fatal("expected warn logging to be enabled")
	}
}

func TestNewFromEnvReadsDevelopmentFlag(t *testing.T) {
	t.Setenv("ACCOUNT_MANAGER_LOG_LEVEL", "debug")
	t.Setenv("ACCOUNT_MANAGER_LOG_DEVELOPMENT", "true")

	logger := NewFromEnv("custom_worker")
	defer Sync(logger)

	if !logger.Core().Enabled(zapcore.DebugLevel) {
		t.Fatal("expected debug logging to be enabled")
	}
}

func TestNewFromEnvFallsBackToNopLoggerOnConstructorError(t *testing.T) {
	original := newCloudLogger
	newCloudLogger = func(cloudlogger.Config) (*zap.Logger, error) {
		return nil, errors.New("boom")
	}
	t.Cleanup(func() {
		newCloudLogger = original
	})

	logger := NewFromEnv(ServiceAPI)
	defer Sync(logger)

	if logger.Core().Enabled(zapcore.ErrorLevel) {
		t.Fatal("expected constructor error to fall back to a nop logger")
	}
}

func TestEnvHelpersUseFallbacks(t *testing.T) {
	t.Setenv("ACCOUNT_MANAGER_LOG_DEVELOPMENT", "")

	if got := getenv("ACCOUNT_MANAGER_MISSING", "fallback"); got != "fallback" {
		t.Fatalf("getenv fallback = %q, want fallback", got)
	}
	if got := boolValue("ACCOUNT_MANAGER_LOG_DEVELOPMENT", true); !got {
		t.Fatal("expected empty bool env to use true fallback")
	}
}

func TestSyncHandlesNilLogger(t *testing.T) {
	Sync(nil)
}
