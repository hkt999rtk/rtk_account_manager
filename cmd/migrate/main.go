package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"

	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/logging"

	"go.uber.org/zap"
)

func main() {
	identityPreflight := flag.Bool("identity-preflight", false, "report forward identity correction on a restored write-frozen database; always roll back")
	flag.Parse()
	earlyLogger := logging.NewFromEnv(logging.ServiceMigrate)
	cfg, err := config.Load()
	if err != nil {
		fatal(earlyLogger, "load config failed", err)
	}
	logging.Sync(earlyLogger)
	logger, err := logging.New(logging.ServiceMigrate, cfg)
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

	if *identityPreflight {
		report, err := database.PreflightIdentityCorrection(ctx, db)
		if err != nil {
			fatal(logger, "identity preflight failed", err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			fatal(logger, "write identity preflight report failed", err)
		}
		if !report.Ready {
			logging.Sync(logger)
			os.Exit(1)
		}
		return
	}

	if err := database.Migrate(ctx, db); err != nil {
		fatal(logger, "database migration failed", err)
	}
	logger.Info("migrations applied")
}

func fatal(logger *zap.Logger, message string, err error, fields ...zap.Field) {
	if err != nil {
		fields = append(fields, zap.Error(err))
	}
	logger.Error(message, fields...)
	logging.Sync(logger)
	os.Exit(1)
}
