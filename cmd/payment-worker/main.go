package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"

	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/logging"
	"rtk_account_manager/internal/payment"
	"rtk_account_manager/internal/paymentcrypto"
	"rtk_account_manager/internal/paymentprovider/newebpay"
	"rtk_account_manager/internal/paymentservice"
	"rtk_account_manager/internal/paymentstore"
)

func main() {
	earlyLogger := logging.NewFromEnv(logging.ServicePaymentWorker)
	cfg, err := config.LoadPaymentWorker()
	if err != nil {
		fatal(earlyLogger, "load payment worker config failed", err)
	}
	logging.Sync(earlyLogger)
	logger, err := logging.New(logging.ServicePaymentWorker, cfg)
	if err != nil {
		fatal(earlyLogger, "create logger failed", err)
	}
	defer logging.Sync(logger)
	if !cfg.PaymentWorkerEnabled {
		logger.Info("payment worker disabled")
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		fatal(logger, "database connection failed", err)
	}
	defer db.Close()
	referenceCipher, err := paymentcrypto.New(cfg.PaymentReferenceEncryptionKey)
	if err != nil {
		fatal(logger, "payment reference cipher setup failed", err)
	}
	provider, err := newebpay.New(newebpay.Config{
		Enabled: cfg.NewebPayEnabled, Environment: cfg.NewebPayEnvironment,
		MerchantID: cfg.NewebPayMerchantID, HashKey: cfg.NewebPayHashKey,
		HashIV: cfg.NewebPayHashIV, Timeout: cfg.NewebPayRequestTimeout,
	})
	if err != nil {
		fatal(logger, "NewebPay adapter setup failed", err)
	}
	service, err := paymentservice.New(paymentservice.Options{
		Store: paymentstore.New(db), Providers: []payment.PaymentProvider{provider},
		ReferenceResolver: referenceCipher, LeaseOwner: workerIdentity(),
		LeaseDuration:       cfg.PaymentWorkerLeaseDuration,
		ReconciliationDelay: cfg.PaymentReconciliationDelay,
		BatchSize:           cfg.PaymentWorkerBatchSize,
		// Deliberately false until NewebPay confirms and enables variable-time
		// merchant-initiated charging for this merchant.
		ChargeEnabled: map[string]bool{"newebpay": false},
	})
	if err != nil {
		fatal(logger, "payment service setup failed", err)
	}
	logger.Info("starting payment worker",
		zap.String("provider", provider.Name()),
		zap.String("provider_environment", cfg.NewebPayEnvironment),
		zap.Bool("merchant_initiated_charge", false),
		zap.Duration("poll_interval", cfg.PaymentWorkerPollInterval),
	)
	if err := service.Run(ctx, cfg.PaymentWorkerPollInterval); err != nil {
		fatal(logger, "payment worker stopped", err)
	}
}

func workerIdentity() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return "payment-worker"
	}
	return "payment-worker/" + hostname
}

func fatal(logger *zap.Logger, message string, err error) {
	logger.Error(message, zap.Error(err))
	logging.Sync(logger)
	os.Exit(1)
}
