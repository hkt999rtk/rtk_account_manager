package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL                string
	AccessSecret               string
	RefreshSecret              string
	AccessTokenTTL             time.Duration
	RefreshTokenTTL            time.Duration
	Port                       string
	CrossServiceBroker         string
	AccountVideoCommandsStream string
	VideoAccountEventsStream   string
	CrossServiceConsumerGroup  string
	CrossServiceMaxAttempts    int
	CrossServicePollInterval   time.Duration
}

func Load() (Config, error) {
	cfg, err := load()
	if err != nil {
		return Config{}, err
	}
	if cfg.AccessSecret == "" {
		return Config{}, fmt.Errorf("JWT_ACCESS_SECRET is required")
	}
	if cfg.RefreshSecret == "" {
		return Config{}, fmt.Errorf("JWT_REFRESH_SECRET is required")
	}
	return cfg, nil
}

func LoadWorker() (Config, error) {
	return load()
}

func load() (Config, error) {
	if err := LoadDotEnv(".env"); err != nil {
		return Config{}, err
	}
	cfg := Config{
		DatabaseURL:                getenv("DATABASE_URL", "postgres://rtk:rtk_password@localhost:5432/rtk_account_manager?sslmode=disable"),
		AccessSecret:               os.Getenv("JWT_ACCESS_SECRET"),
		RefreshSecret:              os.Getenv("JWT_REFRESH_SECRET"),
		AccessTokenTTL:             duration("ACCESS_TOKEN_TTL", 15*time.Minute),
		RefreshTokenTTL:            duration("REFRESH_TOKEN_TTL", 30*24*time.Hour),
		Port:                       getenv("PORT", "8080"),
		CrossServiceBroker:         getenv("CROSS_SERVICE_BROKER", "log"),
		AccountVideoCommandsStream: getenv("ACCOUNT_VIDEO_COMMANDS_STREAM", "account.video.commands"),
		VideoAccountEventsStream:   getenv("VIDEO_ACCOUNT_EVENTS_STREAM", "video.account.events"),
		CrossServiceConsumerGroup:  getenv("CROSS_SERVICE_CONSUMER_GROUP", "rtk_account_manager"),
		CrossServiceMaxAttempts:    intValue("CROSS_SERVICE_MAX_ATTEMPTS", 5),
		CrossServicePollInterval:   duration("CROSS_SERVICE_POLL_INTERVAL", 5*time.Second),
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

func intValue(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
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
