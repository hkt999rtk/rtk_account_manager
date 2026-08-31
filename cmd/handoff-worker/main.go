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
	"rtk_account_manager/internal/worker/handoff"
)

func main() {
	const name = "rtk_account_manager_handoff_worker"
	logger := logging.NewFromEnv(name)
	cfg, err := config.LoadHandoffWorker()
	if err != nil {
		fatal(logger, "load handoff worker config failed", err)
	}
	logging.Sync(logger)
	logger, err = logging.New(name, cfg)
	if err != nil {
		fatal(logging.NewFromEnv(name), "create logger failed", err)
	}
	defer logging.Sync(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		fatal(logger, "connect handoff database failed", err)
	}
	defer db.Close()
	client, err := billinghandoff.New(billinghandoff.Config{BaseURL: cfg.BillingHandoffBaseURL, Token: cfg.BillingHandoffToken})
	if err != nil {
		fatal(logger, "configure handoff transport failed", err)
	}
	repo := store.New(db)
	if err := repo.ConfigureHandoffBilling(client); err != nil {
		fatal(logger, "configure handoff repository failed", err)
	}
	service, err := handoff.NewService(repo, handoff.Options{PollInterval: cfg.HandoffPollInterval, LeaseDuration: cfg.HandoffLeaseDuration, StepTimeout: cfg.HandoffStepTimeout, BatchSize: cfg.HandoffBatchSize, Logger: logger})
	if err != nil {
		fatal(logger, "configure handoff worker failed", err)
	}
	// Production producer transports are not yet installed. Never silently use
	// a successful no-op adapter or shrink an operation's persisted inventory.
	logger.Warn("resource participant adapters not installed; missing preparation or release evidence remains fenced")
	logger.Info("starting handoff recovery worker", zap.Duration("poll_interval", cfg.HandoffPollInterval))
	if err := service.Run(ctx); err != nil {
		fatal(logger, "handoff worker stopped", err)
	}
}
func fatal(logger *zap.Logger, message string, err error) {
	logger.Error(message, zap.Error(err))
	logging.Sync(logger)
	os.Exit(1)
}
