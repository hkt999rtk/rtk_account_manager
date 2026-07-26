package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/emaildelivery"
	"rtk_account_manager/internal/logging"
	"rtk_account_manager/internal/store"
	"rtk_account_manager/internal/worker/emailoutbox"
)

func main() {
	earlyLogger := logging.NewFromEnv(logging.ServiceEmailWorker)
	cfg, err := config.LoadEmailWorker()
	if err != nil {
		fatal(earlyLogger, "load email worker config failed", err)
	}
	logging.Sync(earlyLogger)
	logger, err := logging.New(logging.ServiceEmailWorker, cfg)
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
	cipher, err := emaildelivery.NewCipher(cfg.EmailOutboxEncryptionKey)
	if err != nil {
		fatal(logger, "email outbox cipher setup failed", err)
	}
	smtpClient, err := emaildelivery.NewSMTPClient(emaildelivery.SMTPConfig{
		Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUsername,
		Password: cfg.SMTPPassword, Encryption: cfg.SMTPEncryption, Timeout: 15 * time.Second,
	})
	if err != nil {
		fatal(logger, "SMTP client setup failed", err)
	}
	repository := store.New(db)
	repository.ConfigureEmailOutboxCipher(cipher)
	service := emailoutbox.NewService(repository, cipher, emaildelivery.Renderer{
		From: cfg.SMTPFrom, FromName: cfg.SMTPFromName, BaseURL: cfg.AuthTokenBaseURL,
	}, smtpClient, emailoutbox.Options{
		MaxAttempts: cfg.EmailOutboxMaxAttempts, PollInterval: cfg.EmailOutboxPollInterval,
		RetryBase: cfg.EmailOutboxRetryBase, RetryMax: cfg.EmailOutboxRetryMax,
		BatchSize: cfg.EmailOutboxBatchSize, Logger: logger,
	})
	logger.Info("starting email worker", zap.Duration("poll_interval", cfg.EmailOutboxPollInterval))
	if err := service.Run(ctx); err != nil {
		fatal(logger, "email worker stopped", err)
	}
}

func fatal(logger *zap.Logger, message string, err error) {
	logger.Error(message, zap.Error(err))
	logging.Sync(logger)
	os.Exit(1)
}
