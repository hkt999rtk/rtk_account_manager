package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	DatabaseURL     string
	AccessSecret    string
	RefreshSecret   string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	Port            string
}

func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:     getenv("DATABASE_URL", "postgres://rtk:rtk_password@localhost:5432/rtk_account_manager?sslmode=disable"),
		AccessSecret:    os.Getenv("JWT_ACCESS_SECRET"),
		RefreshSecret:   os.Getenv("JWT_REFRESH_SECRET"),
		AccessTokenTTL:  duration("ACCESS_TOKEN_TTL", 15*time.Minute),
		RefreshTokenTTL: duration("REFRESH_TOKEN_TTL", 30*24*time.Hour),
		Port:            getenv("PORT", "8080"),
	}
	if cfg.AccessSecret == "" {
		return Config{}, fmt.Errorf("JWT_ACCESS_SECRET is required")
	}
	if cfg.RefreshSecret == "" {
		return Config{}, fmt.Errorf("JWT_REFRESH_SECRET is required")
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func duration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}
