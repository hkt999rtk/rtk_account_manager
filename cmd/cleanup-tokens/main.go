package main

import (
	"context"
	"os"
	"time"

	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/logging"
	"rtk_account_manager/internal/store"

	"go.uber.org/zap"
)

func main() {
	earlyLogger := logging.NewFromEnv(logging.ServiceCleanupTokens)
	cfg, err := config.Load()
	if err != nil {
		fatal(earlyLogger, "load config failed", err)
	}
	logging.Sync(earlyLogger)
	logger, err := logging.New(logging.ServiceCleanupTokens, cfg)
	if err != nil {
		fatal(earlyLogger, "create logger failed", err)
	}
	defer logging.Sync(logger)

	ctx := context.Background()
	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		fatal(logger, "database connection failed", err)
	}
	defer db.Close()

	count, err := store.New(db).CleanupRefreshTokens(ctx, time.Now().UTC())
	if err != nil {
		fatal(logger, "refresh token cleanup failed", err)
	}
	logger.Info("refresh tokens cleaned up", zap.Int64("deleted_count", count))
}

func fatal(logger *zap.Logger, message string, err error, fields ...zap.Field) {
	if err != nil {
		fields = append(fields, zap.Error(err))
	}
	logger.Error(message, fields...)
	logging.Sync(logger)
	os.Exit(1)
}
