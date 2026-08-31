package config

import (
	"strings"
	"testing"
)

func TestFactoryEnrollmentCredentialIsDedicated(t *testing.T) {
	if err := validateFactoryEnrollmentConfig(Config{}); err != nil {
		t.Fatal(err)
	}
	valid := Config{FactoryEnrollmentToken: strings.Repeat("f", 32)}
	if err := validateFactoryEnrollmentConfig(valid); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.FactoryEnrollmentToken = "short" }, func(c *Config) { c.FactoryEnrollmentToken += " " },
		func(c *Config) { c.AccessSecret = c.FactoryEnrollmentToken }, func(c *Config) { c.RefreshSecret = c.FactoryEnrollmentToken },
		func(c *Config) { c.InternalAuthToken = c.FactoryEnrollmentToken }, func(c *Config) { c.FactoryProductionJWTSecret = c.FactoryEnrollmentToken },
		func(c *Config) { c.BillingHandoffToken = c.FactoryEnrollmentToken }, func(c *Config) { c.VideoCloudLifecycleToken = c.FactoryEnrollmentToken },
	} {
		cfg := valid
		change(&cfg)
		if err := validateFactoryEnrollmentConfig(cfg); err == nil {
			t.Fatal("unsafe factory credential accepted")
		}
	}
}

func TestLoadFactoryEnrollmentCredentialValidation(t *testing.T) {
	t.Chdir(t.TempDir())
	setValidSendMailHTTPEnvironment(t)
	t.Setenv("AUTH_TOKEN_BASE_URL", "https://admin.example.test")
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("JWT_ACCESS_SECRET", "access")
	t.Setenv("JWT_REFRESH_SECRET", "refresh")
	t.Setenv("ACCOUNT_MANAGER_FACTORY_ENROLLMENT_TOKEN", "short")
	if _, err := Load(); err == nil {
		t.Fatal("short factory credential accepted")
	}
	t.Setenv("ACCOUNT_MANAGER_FACTORY_ENROLLMENT_TOKEN", strings.Repeat("f", 32))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FactoryEnrollmentToken != strings.Repeat("f", 32) {
		t.Fatal("factory credential not loaded")
	}
}
