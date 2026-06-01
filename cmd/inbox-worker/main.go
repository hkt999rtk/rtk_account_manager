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
	"rtk_account_manager/internal/worker/inbox"

	"go.uber.org/zap"
)

func main() {
	earlyLogger := logging.NewFromEnv(logging.ServiceInboxWorker)
	cfg, err := config.LoadWorker()
	if err != nil {
		fatal(earlyLogger, "load worker config failed", err)
	}
	logging.Sync(earlyLogger)
	logger, err := logging.New(logging.ServiceInboxWorker, cfg)
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

	consumer, err := broker.NewConsumer(cfg.CrossServiceBroker, broker.ConsumerOptions{
		LogReader:                      os.Stdin,
		AzureEventHubsConnectionString: cfg.AzureEventHubConnectionString,
		Stream:                         cfg.VideoAccountEventsStream,
		ConsumerGroup:                  cfg.CrossServiceConsumerGroup,
		ReceiveTimeout:                 cfg.CrossServicePollInterval,
		CheckpointFile:                 cfg.AzureEventHubCheckpointFile,
		NATSURL:                        cfg.CrossServiceNATSURL,
		NATSName:                       cfg.CrossServiceNATSName,
		PartitionCount:                 cfg.CrossServicePartitionCount,
	})
	if err != nil {
		fatal(logger, "consumer creation failed", err)
	}
	defer func() {
		if err := consumer.Close(context.Background()); err != nil {
			logger.Warn("consumer close failed", zap.Error(err))
		}
	}()

	service := inbox.NewService(store.New(db), consumer, inbox.Options{
		Stream:        cfg.VideoAccountEventsStream,
		ConsumerGroup: cfg.CrossServiceConsumerGroup,
		MaxAttempts:   cfg.CrossServiceMaxAttempts,
		PollInterval:  cfg.CrossServicePollInterval,
		Logger:        logger,
	})

	logger.Info("starting inbox worker", zap.String("broker", cfg.CrossServiceBroker), zap.String("stream", cfg.VideoAccountEventsStream), zap.String("consumer_group", cfg.CrossServiceConsumerGroup), zap.Duration("poll_interval", cfg.CrossServicePollInterval))
	if err := service.Run(ctx); err != nil {
		fatal(logger, "inbox worker stopped", err)
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
