package config

import (
	"strings"
	"testing"
)

func TestHandoffBillingConfigurationIsPairedAndIsolated(t *testing.T) {
	if err := validateHandoffBillingConfig(Config{}); err != nil {
		t.Fatal(err)
	}
	valid := Config{BillingHandoffBaseURL: "https://billing.example", BillingHandoffToken: strings.Repeat("h", 32)}
	if err := validateHandoffBillingConfig(valid); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.BillingHandoffBaseURL = "" }, func(c *Config) { c.BillingHandoffToken = "" },
		func(c *Config) { c.BillingHandoffBaseURL = "http://billing.example" }, func(c *Config) { c.BillingHandoffBaseURL = "https://user:pass@billing.example" },
		func(c *Config) { c.InternalAuthToken = c.BillingHandoffToken }, func(c *Config) { c.AccessSecret = c.BillingHandoffToken },
		func(c *Config) { c.SendMailHTTPBearerToken = c.BillingHandoffToken }, func(c *Config) { c.EmailOutboxEncryptionKey = c.BillingHandoffToken },
	} {
		cfg := valid
		change(&cfg)
		if err := validateHandoffBillingConfig(cfg); err == nil {
			t.Fatal("unsafe handoff configuration accepted")
		}
	}
}

func TestLoadHandoffWorkerRequiresTransportAndBoundedLease(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("JWT_ACCESS_SECRET", "access")
	t.Setenv("JWT_REFRESH_SECRET", "refresh")
	t.Setenv("BILLING_HANDOFF_BASE_URL", "")
	t.Setenv("BILLING_HANDOFF_TOKEN", "")
	if _, err := LoadHandoffWorker(); err == nil {
		t.Fatal("unconfigured worker started")
	}
	t.Setenv("BILLING_HANDOFF_BASE_URL", "https://billing.example")
	t.Setenv("BILLING_HANDOFF_TOKEN", strings.Repeat("h", 32))
	if _, err := LoadHandoffWorker(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HANDOFF_WORKER_LEASE_DURATION", "30s")
	if _, err := LoadHandoffWorker(); err == nil {
		t.Fatal("lease shorter than remote step accepted")
	}
	t.Setenv("HANDOFF_WORKER_STEP_TIMEOUT", "20s")
	if _, err := LoadHandoffWorker(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HANDOFF_WORKER_BATCH_SIZE", "129")
	if _, err := LoadHandoffWorker(); err == nil {
		t.Fatal("unbounded batch accepted")
	}
}
