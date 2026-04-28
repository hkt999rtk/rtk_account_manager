package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"rtk_account_manager/internal/broker"
	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/store"
	"rtk_account_manager/internal/worker/outbox"
)

func main() {
	cfg, err := config.LoadWorker()
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	publisher, err := broker.NewPublisher(cfg.CrossServiceBroker, os.Stdout)
	if err != nil {
		log.Fatal(err)
	}

	service := outbox.NewService(store.New(db), publisher, outbox.Options{
		MaxAttempts:  cfg.CrossServiceMaxAttempts,
		PollInterval: cfg.CrossServicePollInterval,
		RetryDelay:   cfg.CrossServicePollInterval,
	})

	log.Printf("starting outbox worker with broker=%s poll_interval=%s", cfg.CrossServiceBroker, cfg.CrossServicePollInterval)
	if err := service.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
