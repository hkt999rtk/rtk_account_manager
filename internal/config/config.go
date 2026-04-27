package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
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
	if err := LoadDotEnv(".env"); err != nil {
		return Config{}, err
	}
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

func LoadDotEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		os.Setenv(key, unquote(strings.TrimSpace(value)))
	}
	return scanner.Err()
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

func unquote(value string) string {
	if len(value) < 2 {
		return value
	}
	if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
		return value[1 : len(value)-1]
	}
	return value
}
