package main

import (
	"context"
	"net/http"
	"os"

	"go.uber.org/zap"

	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/paymentsimulator"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	cfg, err := config.LoadPaymentSimulator()
	if err != nil {
		logger.Fatal("load payment simulator config failed", zap.Error(err))
	}
	db, err := database.Connect(context.Background(), cfg.DatabaseURL)
	if err != nil {
		logger.Fatal("connect payment simulator database failed", zap.Error(err))
	}
	defer db.Close()
	server, err := paymentsimulator.New(db, paymentsimulator.Config{
		Environment: cfg.LogEnv, PublicBaseURL: cfg.PaymentSimulatorPublicBaseURL,
		CallbackURL: cfg.PaymentSimulatorCallbackURL, SharedSecret: cfg.PaymentSimulatorSharedSecret,
		CallbackSecret: cfg.PaymentSimulatorCallbackSecret, Retention: cfg.PaymentSimulatorRetention,
	})
	if err != nil {
		logger.Fatal("configure payment simulator failed", zap.Error(err))
	}
	address := ":" + cfg.Port
	logger.Info("payment simulator listening", zap.String("address", address), zap.String("environment", cfg.LogEnv))
	if err := http.ListenAndServe(address, server.Handler()); err != nil {
		logger.Error("payment simulator stopped", zap.Error(err))
		os.Exit(1)
	}
}
