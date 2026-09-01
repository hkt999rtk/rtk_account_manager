package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"
	"rtk_account_manager/internal/billinghandoff"
	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/logging"
	"rtk_account_manager/internal/store"
	"rtk_account_manager/internal/worker/clouddeletion"
)

func main() {
	const name = "rtk_account_manager_cloud_deletion_worker"
	logger := logging.NewFromEnv(name)
	cfg, err := config.LoadCloudDeletionWorker()
	if err != nil {
		fatal(logger, "invalid cloud deletion worker configuration")
	}
	logging.Sync(logger)
	logger, err = logging.New(name, cfg)
	if err != nil {
		fatal(logging.NewFromEnv(name), "create cloud deletion logger failed")
	}
	defer logging.Sync(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		fatal(logger, "connect cloud deletion database failed")
	}
	defer db.Close()
	var migrated bool
	if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version='063_cloud_deletion_worker_wake.sql')`).Scan(&migrated); err != nil || !migrated {
		fatal(logger, "cloud deletion recovery schema is not ready; apply reviewed migrations separately")
	}
	client, err := billinghandoff.New(billinghandoff.Config{BaseURL: cfg.BillingHandoffBaseURL, Token: cfg.BillingHandoffToken})
	if err != nil {
		fatal(logger, "configure cloud deletion Billing transport failed")
	}
	repo := store.New(db)
	if err := repo.ConfigureCloudDeletionRecovery(store.CloudDeletionOptions{Billing: client}); err != nil {
		fatal(logger, "configure cloud deletion recovery failed")
	}
	service, err := clouddeletion.NewService(repo, clouddeletion.Options{PollInterval: cfg.CloudDeletionPollInterval, LeaseDuration: cfg.CloudDeletionLeaseDuration, StepTimeout: cfg.CloudDeletionStepTimeout, BatchSize: cfg.CloudDeletionBatchSize, Logger: logger})
	if err != nil {
		fatal(logger, "configure cloud deletion worker failed")
	}
	// No migrations, new DELETE admission, fake observers or no-op release.
	// Missing persisted participants block at their proof/release stage.
	logger.Warn("recovery only; resource adapters are not installed and missing evidence remains fenced")
	logger.Info("starting cloud deletion recovery worker")
	if err := service.Run(ctx); err != nil {
		fatal(logger, "cloud deletion worker stopped")
	}
}

func fatal(logger *zap.Logger, message string) {
	// Raw transport/config/database errors can include credentials or payer data.
	logger.Error(message)
	logging.Sync(logger)
	os.Exit(1)
}
