package config

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func setPaymentWorkerEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PAYMENT_WORKER_ENABLED", "true")
	t.Setenv("PAYMENT_REFERENCE_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'p'}, 32)))
	t.Setenv("NEWEBPAY_ENABLED", "true")
	t.Setenv("NEWEBPAY_ENVIRONMENT", "sandbox")
	t.Setenv("NEWEBPAY_MERCHANT_ID", "MS123456789")
	t.Setenv("NEWEBPAY_HASH_KEY", strings.Repeat("k", 32))
	t.Setenv("NEWEBPAY_HASH_IV", strings.Repeat("i", 16))
	t.Setenv("NEWEBPAY_MERCHANT_INITIATED_CHARGE_ENABLED", "false")
}

func TestPaymentSimulatorRequiresSafeRunScopedConfiguration(t *testing.T) {
	cfg := Config{
		LogEnv: "staging", PaymentSimulatorEnabled: true,
		PaymentSimulatorBaseURL:        "http://payment-simulator:8081",
		PaymentSimulatorPublicBaseURL:  "https://payment-simulator.example.test",
		PaymentSimulatorCallbackURL:    "http://account-manager:8080/v1/internal/payment-simulator/setup-callback",
		PaymentSimulatorSharedSecret:   strings.Repeat("s", 32),
		PaymentSimulatorCallbackSecret: strings.Repeat("c", 32),
		PaymentSimulatorRunID:          "gh-123-1", PaymentSimulatorRetention: 7 * 24 * time.Hour,
	}
	if err := validatePaymentSimulatorConfig(cfg, true); err != nil {
		t.Fatal(err)
	}
	cfg.PaymentSimulatorRunID = "unsafe/run"
	if err := validatePaymentSimulatorConfig(cfg, true); err == nil || !strings.Contains(err.Error(), "PAYMENT_SIMULATOR_RUN_ID") {
		t.Fatalf("unsafe run ID err=%v", err)
	}
	cfg.PaymentSimulatorRunID = ""
	if err := validatePaymentSimulatorConfig(cfg, false); err == nil {
		t.Fatal("missing run ID must fail for provider clients")
	}
}

func TestPaymentWorkerDisabledByDefaultNeedsNoCredentials(t *testing.T) {
	t.Setenv("PAYMENT_WORKER_ENABLED", "false")
	t.Setenv("NEWEBPAY_ENABLED", "false")
	cfg, err := LoadPaymentWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PaymentWorkerEnabled || cfg.NewebPayEnabled {
		t.Fatalf("config=%+v", cfg)
	}
}

func TestPaymentWorkerValidatesSecretsWithoutEchoingThem(t *testing.T) {
	setPaymentWorkerEnv(t)
	secret := "very-sensitive-invalid-key"
	t.Setenv("NEWEBPAY_HASH_KEY", secret)
	_, err := LoadPaymentWorker()
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe error=%v", err)
	}

	setPaymentWorkerEnv(t)
	t.Setenv("PAYMENT_REFERENCE_ENCRYPTION_KEY", "not-base64-secret")
	_, err = LoadPaymentWorker()
	if err == nil || strings.Contains(err.Error(), "not-base64-secret") {
		t.Fatalf("unsafe reference-key error=%v", err)
	}
}

func TestPaymentWorkerRejectsUnsupportedAutoChargeAndConfigurationTypos(t *testing.T) {
	setPaymentWorkerEnv(t)
	t.Setenv("NEWEBPAY_MERCHANT_INITIATED_CHARGE_ENABLED", "true")
	if _, err := LoadPaymentWorker(); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("merchant-initiated charge err=%v", err)
	}

	setPaymentWorkerEnv(t)
	t.Setenv("NEWEBPAY_ENVIRONMENT", "staging-url")
	if _, err := LoadPaymentWorker(); err == nil || !strings.Contains(err.Error(), "sandbox or production") {
		t.Fatalf("environment err=%v", err)
	}

	setPaymentWorkerEnv(t)
	t.Setenv("PAYMENT_WORKER_ENABLED", "sometimes")
	if _, err := LoadPaymentWorker(); err == nil || !strings.Contains(err.Error(), "true or false") {
		t.Fatalf("boolean err=%v", err)
	}
}

func TestEnabledPaymentWorkerLoadsSafeDefaults(t *testing.T) {
	setPaymentWorkerEnv(t)
	cfg, err := LoadPaymentWorker()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PaymentWorkerEnabled || !cfg.NewebPayEnabled || cfg.NewebPayAutoChargeEnabled ||
		cfg.PaymentWorkerBatchSize != 20 || cfg.PaymentWorkerPollInterval <= 0 || cfg.NewebPayRequestTimeout <= 0 {
		t.Fatalf("config=%+v", cfg)
	}
}
