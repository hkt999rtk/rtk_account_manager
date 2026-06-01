package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"rtk_account_manager/internal/broker"
	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/logging"
	"rtk_account_manager/internal/store"
	"rtk_account_manager/internal/worker/outbox"

	"go.uber.org/zap"
)

func main() {
	earlyLogger := logging.NewFromEnv(logging.ServiceOutboxWorker)
	cfg, err := config.LoadWorker()
	if err != nil {
		fatal(earlyLogger, "load worker config failed", err)
	}
	logging.Sync(earlyLogger)
	logger, err := logging.New(logging.ServiceOutboxWorker, cfg)
	if err != nil {
		fatal(earlyLogger, "create logger failed", err)
	}
	defer logging.Sync(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		fatal(logger, "database connection failed", err)
	}
	defer db.Close()

	publisher, err := broker.NewPublisher(cfg.CrossServiceBroker, broker.PublisherOptions{
		LogWriter:                      os.Stdout,
		AzureEventHubsConnectionString: cfg.AzureEventHubConnectionString,
		Stream:                         cfg.AccountVideoCommandsStream,
		NATSURL:                        cfg.CrossServiceNATSURL,
		NATSName:                       cfg.CrossServiceNATSName,
		PartitionCount:                 cfg.CrossServicePartitionCount,
	})
	if err != nil {
		fatal(logger, "publisher creation failed", err)
	}
	defer func() {
		if err := publisher.Close(context.Background()); err != nil {
			logger.Warn("publisher close failed", zap.Error(err))
		}
	}()

	service := outbox.NewService(store.New(db), publisher, outbox.Options{
		MaxAttempts:  cfg.CrossServiceMaxAttempts,
		PollInterval: cfg.CrossServicePollInterval,
		RetryDelay:   cfg.CrossServicePollInterval,
		Logger:       logger,
	})

	logger.Info("starting outbox worker", zap.String("broker", cfg.CrossServiceBroker), zap.Duration("poll_interval", cfg.CrossServicePollInterval))
	if err := service.Run(ctx); err != nil {
		fatal(logger, "outbox worker stopped", err)
	}
}

func fatal(logger *zap.Logger, message string, err error, fields ...zap.Field) {
	if err != nil {
		fields = append(fields, zap.Error(err))
	}
	logger.Error(message, fields...)
	logging.Sync(logger)
	os.Exit(1)
}
