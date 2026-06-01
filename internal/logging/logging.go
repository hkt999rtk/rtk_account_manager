package logging

import (
	"os"
	"strconv"

	"github.com/hkt999rtk/rtk_cloud_logger"
	"go.uber.org/zap"

	"rtk_account_manager/internal/config"
)

const (
	ServiceAPI           = "rtk_account_manager_api"
	ServiceMigrate       = "rtk_account_manager_migrate"
	ServiceOutboxWorker  = "rtk_account_manager_outbox_worker"
	ServiceInboxWorker   = "rtk_account_manager_inbox_worker"
	ServiceCleanupTokens = "rtk_account_manager_cleanup_tokens"
)

func New(service string, cfg config.Config) (*zap.Logger, error) {
	return cloudlogger.New(cloudlogger.Config{
		Service:     service,
		Env:         cfg.LogEnv,
		Version:     cfg.LogVersion,
		Level:       cfg.LogLevel,
		Development: cfg.LogDevelopment,
	})
}

func NewFromEnv(service string) *zap.Logger {
	logger, err := cloudlogger.New(cloudlogger.Config{
		Service:     service,
		Env:         getenv("ACCOUNT_MANAGER_ENV", "local"),
		Version:     getenv("ACCOUNT_MANAGER_VERSION", "dev"),
		Level:       getenv("ACCOUNT_MANAGER_LOG_LEVEL", "info"),
		Development: boolValue("ACCOUNT_MANAGER_LOG_DEVELOPMENT", false),
	})
	if err != nil {
		return zap.NewNop()
	}
	return logger
}

func Sync(logger *zap.Logger) {
	if logger == nil {
		return
	}
	_ = logger.Sync()
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func boolValue(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
