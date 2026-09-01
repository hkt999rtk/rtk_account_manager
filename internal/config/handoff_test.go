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

func TestFactoryHandoffConfigurationIsPairedAndIsolated(t *testing.T) {
	if err := validateFactoryHandoffConfig(Config{}); err != nil {
		t.Fatal(err)
	}
	valid := Config{FactoryHandoffBaseURL: "https://factory.example", FactoryHandoffToken: strings.Repeat("f", 32)}
	if err := validateFactoryHandoffConfig(valid); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.FactoryHandoffBaseURL = "" }, func(c *Config) { c.FactoryHandoffToken = "" },
		func(c *Config) { c.FactoryHandoffBaseURL = "http://factory.example" }, func(c *Config) { c.FactoryHandoffBaseURL = "https://user:pass@factory.example" },
		func(c *Config) { c.InternalAuthToken = c.FactoryHandoffToken }, func(c *Config) { c.BillingHandoffToken = c.FactoryHandoffToken },
		func(c *Config) { c.FactoryEnrollmentToken = c.FactoryHandoffToken }, func(c *Config) { c.EmailOutboxEncryptionKey = c.FactoryHandoffToken },
	} {
		cfg := valid
		change(&cfg)
		if err := validateFactoryHandoffConfig(cfg); err == nil {
			t.Fatal("unsafe factory handoff configuration accepted")
		}
	}
}

func TestResourceHandoffConfigurationIsPairedDistinctAndReviewed(t *testing.T) {
	if err := validateResourceHandoffConfig(Config{}); err != nil {
		t.Fatal(err)
	}
	valid := Config{
		VideoControlPlaneHandoffBaseURL: "https://video.example",
		VideoControlPlaneHandoffToken:   strings.Repeat("v", 32),
		MQTTUsageHandoffBaseURL:         "https://mqtt.example",
		MQTTUsageHandoffToken:           strings.Repeat("m", 32),
	}
	if err := validateResourceHandoffConfig(valid); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.VideoControlPlaneHandoffBaseURL = "" },
		func(c *Config) { c.VideoControlPlaneHandoffToken = "" },
		func(c *Config) { c.MQTTUsageHandoffBaseURL = "http://mqtt.example" },
		func(c *Config) { c.InternalAuthToken = c.VideoControlPlaneHandoffToken },
		func(c *Config) { c.MQTTUsageHandoffToken = c.VideoControlPlaneHandoffToken },
	} {
		cfg := valid
		change(&cfg)
		if err := validateResourceHandoffConfig(cfg); err == nil {
			t.Fatal("unsafe resource handoff configuration accepted")
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
	if _, err := LoadHandoffWorker(); err == nil {
		t.Fatal("worker started without factory participant transport")
	}
	t.Setenv("FACTORY_HANDOFF_BASE_URL", "https://factory.example")
	t.Setenv("FACTORY_HANDOFF_TOKEN", strings.Repeat("f", 32))
	if _, err := LoadHandoffWorker(); err == nil {
		t.Fatal("worker started without Video Cloud participant transports")
	}
	t.Setenv("VIDEO_CONTROL_PLANE_HANDOFF_BASE_URL", "https://video.example")
	t.Setenv("VIDEO_CONTROL_PLANE_HANDOFF_TOKEN", strings.Repeat("v", 32))
	t.Setenv("MQTT_USAGE_HANDOFF_BASE_URL", "https://mqtt.example")
	t.Setenv("MQTT_USAGE_HANDOFF_TOKEN", strings.Repeat("m", 32))
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
