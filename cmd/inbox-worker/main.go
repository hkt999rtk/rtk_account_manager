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
	"rtk_account_manager/internal/worker/inbox"
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
		log.Fatal(err)
	}
	defer func() {
		if err := consumer.Close(context.Background()); err != nil {
			log.Printf("close consumer: %v", err)
		}
	}()

	service := inbox.NewService(store.New(db), consumer, inbox.Options{
		Stream:        cfg.VideoAccountEventsStream,
		ConsumerGroup: cfg.CrossServiceConsumerGroup,
		MaxAttempts:   cfg.CrossServiceMaxAttempts,
		PollInterval:  cfg.CrossServicePollInterval,
	})

	log.Printf("starting inbox worker with broker=%s stream=%s consumer_group=%s poll_interval=%s", cfg.CrossServiceBroker, cfg.VideoAccountEventsStream, cfg.CrossServiceConsumerGroup, cfg.CrossServicePollInterval)
	if err := service.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
