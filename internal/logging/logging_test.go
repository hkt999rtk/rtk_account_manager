package logging

import (
	"testing"

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

func TestSyncHandlesNilLogger(t *testing.T) {
	Sync(nil)
}
