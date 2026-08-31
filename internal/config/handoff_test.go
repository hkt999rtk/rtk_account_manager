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
