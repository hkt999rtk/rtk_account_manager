package config

import (
	"strings"
	"testing"
)

func TestBillingCloudCreationConfigRequiresSeparateOriginAndCredential(t *testing.T) {
	if err := validateBillingCloudCreationConfig(Config{}); err != nil {
		t.Fatal(err)
	}
	cfg := Config{BillingCloudCreationBaseURL: "https://billing.example", BillingCloudCreationToken: strings.Repeat("n", 32)}
	if err := validateBillingCloudCreationConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Config){func(c *Config) { c.BillingCloudCreationToken = "" }, func(c *Config) { c.BillingCloudCreationBaseURL = "" }, func(c *Config) { c.BillingCloudCreationBaseURL = "http://billing.example" }, func(c *Config) { c.BillingHandoffToken = c.BillingCloudCreationToken }, func(c *Config) { c.AccessSecret = c.BillingCloudCreationToken }, func(c *Config) { c.FactoryEnrollmentToken = c.BillingCloudCreationToken }} {
		bad := cfg
		change(&bad)
		if err := validateBillingCloudCreationConfig(bad); err == nil {
			t.Fatal("invalid config")
		}
	}
}
