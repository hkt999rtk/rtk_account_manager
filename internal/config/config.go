package config

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"rtk_account_manager/internal/billinghandoff"
)

type Config struct {
	DatabaseURL                    string
	AccessSecret                   string
	RefreshSecret                  string
	JWTSignerProvider              string
	JWTAccessPrivateKeyPath        string
	JWTAccessPublicKeyPath         string
	JWTRefreshPrivateKeyPath       string
	JWTRefreshPublicKeyPath        string
	JWTAccessPKCS11ModulePath      string
	JWTAccessPKCS11TokenLabel      string
	JWTAccessPKCS11SlotID          string
	JWTAccessPKCS11PIN             string
	JWTAccessPKCS11KeyLabel        string
	JWTRefreshPKCS11ModulePath     string
	JWTRefreshPKCS11TokenLabel     string
	JWTRefreshPKCS11SlotID         string
	JWTRefreshPKCS11PIN            string
	JWTRefreshPKCS11KeyLabel       string
	AccessTokenTTL                 time.Duration
	RefreshTokenTTL                time.Duration
	Port                           string
	AuthTokenBaseURL               string
	SendMailHTTPBaseURL            string
	SendMailHTTPBearerToken        string
	SendMailHTTPTimeout            time.Duration
	EmailOutboxEncryptionKey       string
	EmailOutboxPollInterval        time.Duration
	EmailOutboxBatchSize           int
	EmailOutboxMaxAttempts         int
	EmailOutboxRetryBase           time.Duration
	EmailOutboxRetryMax            time.Duration
	CrossServiceBroker             string
	AzureEventHubConnectionString  string
	AzureEventHubCheckpointFile    string
	AccountVideoCommandsStream     string
	VideoAccountEventsStream       string
	CrossServiceConsumerGroup      string
	CrossServiceMaxAttempts        int
	CrossServicePollInterval       time.Duration
	VideoCloudLifecycleBaseURL     string
	VideoCloudLifecycleToken       string
	VideoCloudLifecycleTimeout     time.Duration
	EmailVerificationTTL           time.Duration
	PasswordResetTTL               time.Duration
	OTPResendInterval              time.Duration
	OTPMaxAttempts                 int
	AuthTokenRateLimitMax          int
	AuthTokenRateLimitWindow       time.Duration
	OIDCEnabled                    bool
	OIDCProviderID                 string
	OIDCProviderName               string
	OIDCIssuerURL                  string
	OIDCClientID                   string
	OIDCClientSecret               string
	OIDCRedirectURL                string
	OIDCScopes                     []string
	OIDCAutoLinkEmail              bool
	UserCacheEnabled               bool
	UserCacheAddr                  string
	UserCachePrefix                string
	AppCertIssuerBaseURL           string
	AppCertIssuerClientCert        string
	AppCertIssuerClientKey         string
	AppCertIssuerCAFile            string
	AppCertIssuerTimeout           time.Duration
	InternalAuthToken              string
	BillingHandoffBaseURL          string
	BillingHandoffToken            string
	HandoffPollInterval            time.Duration
	HandoffLeaseDuration           time.Duration
	HandoffStepTimeout             time.Duration
	HandoffBatchSize               int
	CloudDeletionPollInterval      time.Duration
	CloudDeletionLeaseDuration     time.Duration
	CloudDeletionStepTimeout       time.Duration
	CloudDeletionBatchSize         int
	AllowImmediateBrandAccounts    bool
	FactoryProductionJWTSecret     string
	FactoryProductionJWTAudience   string
	BootstrapPlatformAdminEmail    string
	BootstrapPlatformAdminPassword string
	ChipsetProviderAllowedHosts    []string
	ChipsetProviderRefreshInterval time.Duration
	LogEnv                         string
	LogVersion                     string
	LogLevel                       string
	LogDevelopment                 bool
}

func Load() (Config, error) {
	cfg, err := load()
	if err != nil {
		return Config{}, err
	}
	switch strings.ToLower(strings.TrimSpace(cfg.JWTSignerProvider)) {
	case "", "hs256":
		if cfg.AccessSecret == "" {
			return Config{}, fmt.Errorf("JWT_ACCESS_SECRET is required")
		}
		if cfg.RefreshSecret == "" {
			return Config{}, fmt.Errorf("JWT_REFRESH_SECRET is required")
		}
	case "pem":
		if strings.TrimSpace(cfg.JWTAccessPrivateKeyPath) == "" || strings.TrimSpace(cfg.JWTAccessPublicKeyPath) == "" {
			return Config{}, fmt.Errorf("JWT access PEM signer paths are required")
		}
		if strings.TrimSpace(cfg.JWTRefreshPrivateKeyPath) == "" || strings.TrimSpace(cfg.JWTRefreshPublicKeyPath) == "" {
			return Config{}, fmt.Errorf("JWT refresh PEM signer paths are required")
		}
	case "pkcs11":
		if err := validatePKCS11JWTSigner("JWT_ACCESS", cfg.JWTAccessPKCS11ModulePath, cfg.JWTAccessPKCS11TokenLabel, cfg.JWTAccessPKCS11SlotID, cfg.JWTAccessPKCS11PIN, cfg.JWTAccessPKCS11KeyLabel); err != nil {
			return Config{}, err
		}
		if err := validatePKCS11JWTSigner("JWT_REFRESH", cfg.JWTRefreshPKCS11ModulePath, cfg.JWTRefreshPKCS11TokenLabel, cfg.JWTRefreshPKCS11SlotID, cfg.JWTRefreshPKCS11PIN, cfg.JWTRefreshPKCS11KeyLabel); err != nil {
			return Config{}, err
		}
	default:
		return Config{}, fmt.Errorf("JWT_SIGNER_PROVIDER must be hs256, pem, or pkcs11")
	}
	if err := validateEmailConfig(cfg); err != nil {
		return Config{}, err
	}
	if err := validateHandoffBillingConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateHandoffBillingConfig(cfg Config) error {
	if cfg.BillingHandoffBaseURL == "" && cfg.BillingHandoffToken == "" {
		return nil
	}
	if _, err := billinghandoff.New(billinghandoff.Config{BaseURL: cfg.BillingHandoffBaseURL, Token: cfg.BillingHandoffToken}); err != nil {
		return fmt.Errorf("BILLING_HANDOFF_BASE_URL and a dedicated BILLING_HANDOFF_TOKEN must be configured together with a trusted origin")
	}
	for _, secret := range []string{cfg.AccessSecret, cfg.RefreshSecret, cfg.InternalAuthToken, cfg.SendMailHTTPBearerToken, cfg.VideoCloudLifecycleToken, cfg.EmailOutboxEncryptionKey, cfg.FactoryProductionJWTSecret, cfg.OIDCClientSecret, cfg.JWTAccessPKCS11PIN, cfg.JWTRefreshPKCS11PIN} {
		if secret != "" && secret == cfg.BillingHandoffToken {
			return fmt.Errorf("BILLING_HANDOFF_TOKEN must not reuse other service credentials")
		}
	}
	return nil
}

func LoadHandoffWorker() (Config, error) {
	cfg, err := load()
	if err != nil {
		return Config{}, err
	}
	if cfg.BillingHandoffBaseURL == "" || cfg.BillingHandoffToken == "" {
		return Config{}, fmt.Errorf("handoff worker requires dedicated Billing transport configuration")
	}
	if err := validateHandoffBillingConfig(cfg); err != nil {
		return Config{}, err
	}
	if cfg.HandoffPollInterval <= 0 || cfg.HandoffPollInterval > time.Minute || cfg.HandoffLeaseDuration < 30*time.Second || cfg.HandoffLeaseDuration > 5*time.Minute || cfg.HandoffStepTimeout <= 0 || cfg.HandoffStepTimeout+5*time.Second >= cfg.HandoffLeaseDuration || cfg.HandoffBatchSize < 1 || cfg.HandoffBatchSize > 128 {
		return Config{}, fmt.Errorf("invalid handoff worker timing or batch size")
	}
	return cfg, nil
}

func LoadWorker() (Config, error) {
	cfg, err := load()
	if err != nil {
		return Config{}, err
	}
	if strings.EqualFold(strings.TrimSpace(cfg.CrossServiceBroker), "direct_http") {
		endpoint, err := url.Parse(strings.TrimSpace(cfg.VideoCloudLifecycleBaseURL))
		if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
			return Config{}, fmt.Errorf("VIDEO_CLOUD_LIFECYCLE_BASE_URL must be an absolute URL when CROSS_SERVICE_BROKER=direct_http")
		}
		if endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
			return Config{}, fmt.Errorf("VIDEO_CLOUD_LIFECYCLE_BASE_URL must be a credential-free origin")
		}
		if strings.TrimSpace(cfg.VideoCloudLifecycleToken) == "" {
			return Config{}, fmt.Errorf("VIDEO_CLOUD_LIFECYCLE_TOKEN is required when CROSS_SERVICE_BROKER=direct_http")
		}
		if cfg.VideoCloudLifecycleTimeout <= 0 {
			return Config{}, fmt.Errorf("VIDEO_CLOUD_LIFECYCLE_TIMEOUT must be positive")
		}
	}
	return cfg, nil
}

// Recovery-only process: it has no public routes or resource-observer setup.
func LoadCloudDeletionWorker() (Config, error) {
	cfg, err := load()
	if err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" {
		return Config{}, fmt.Errorf("cloud deletion worker requires explicit DATABASE_URL")
	}
	if cfg.BillingHandoffBaseURL == "" || cfg.BillingHandoffToken == "" {
		return Config{}, fmt.Errorf("cloud deletion worker requires dedicated Billing transport configuration")
	}
	if err := validateHandoffBillingConfig(cfg); err != nil {
		return Config{}, err
	}
	// The generic loaders use defaults for malformed values. Recovery startup must
	// reject typos rather than silently running with a different resource budget.
	for key, target := range map[string]*time.Duration{
		"CLOUD_DELETION_WORKER_POLL_INTERVAL":  &cfg.CloudDeletionPollInterval,
		"CLOUD_DELETION_WORKER_LEASE_DURATION": &cfg.CloudDeletionLeaseDuration,
		"CLOUD_DELETION_WORKER_STEP_TIMEOUT":   &cfg.CloudDeletionStepTimeout,
	} {
		if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
			value, err := time.ParseDuration(raw)
			if err != nil {
				return Config{}, fmt.Errorf("invalid %s", key)
			}
			*target = value
		}
	}
	if raw := strings.TrimSpace(os.Getenv("CLOUD_DELETION_WORKER_BATCH_SIZE")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CLOUD_DELETION_WORKER_BATCH_SIZE")
		}
		cfg.CloudDeletionBatchSize = value
	}
	if cfg.CloudDeletionPollInterval <= 0 || cfg.CloudDeletionPollInterval > time.Minute || cfg.CloudDeletionLeaseDuration < 30*time.Second || cfg.CloudDeletionLeaseDuration > 5*time.Minute || cfg.CloudDeletionStepTimeout <= 0 || cfg.CloudDeletionStepTimeout >= cfg.CloudDeletionLeaseDuration-5*time.Second || cfg.CloudDeletionBatchSize < 1 || cfg.CloudDeletionBatchSize > 128 {
		return Config{}, fmt.Errorf("invalid cloud deletion worker timing or batch size")
	}
	return cfg, nil
}

func LoadEmailWorker() (Config, error) {
	cfg, err := load()
	if err != nil {
		return Config{}, err
	}
	if err := validateEmailConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateEmailConfig(cfg Config) error {
	production := strings.EqualFold(strings.TrimSpace(cfg.LogEnv), "production")
	if strings.TrimSpace(cfg.AuthTokenBaseURL) == "" {
		return fmt.Errorf("AUTH_TOKEN_BASE_URL is required")
	}
	baseURL, err := url.Parse(strings.TrimSpace(cfg.AuthTokenBaseURL))
	if err != nil || baseURL.Host == "" {
		return fmt.Errorf("AUTH_TOKEN_BASE_URL must be an absolute URL")
	}
	if production && baseURL.Scheme != "https" {
		return fmt.Errorf("AUTH_TOKEN_BASE_URL must use https in production")
	}
	if _, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cfg.EmailOutboxEncryptionKey)); err != nil {
		return fmt.Errorf("EMAIL_OUTBOX_ENCRYPTION_KEY must be base64 encoded")
	}
	decoded, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(cfg.EmailOutboxEncryptionKey))
	if len(decoded) != 32 {
		return fmt.Errorf("EMAIL_OUTBOX_ENCRYPTION_KEY must decode to exactly 32 bytes")
	}
	for _, setting := range []struct {
		key   string
		value time.Duration
	}{
		{"EMAIL_OUTBOX_POLL_INTERVAL", cfg.EmailOutboxPollInterval},
		{"EMAIL_OUTBOX_RETRY_BASE", cfg.EmailOutboxRetryBase},
		{"EMAIL_OUTBOX_RETRY_MAX", cfg.EmailOutboxRetryMax},
	} {
		if err := validatePositiveDuration(setting.key, setting.value); err != nil {
			return err
		}
	}
	for _, setting := range []struct {
		key   string
		value int
	}{
		{"EMAIL_OUTBOX_BATCH_SIZE", cfg.EmailOutboxBatchSize},
		{"EMAIL_OUTBOX_MAX_ATTEMPTS", cfg.EmailOutboxMaxAttempts},
	} {
		if err := validatePositiveInt(setting.key, setting.value); err != nil {
			return err
		}
	}
	if cfg.EmailOutboxRetryMax < cfg.EmailOutboxRetryBase {
		return fmt.Errorf("EMAIL_OUTBOX_RETRY_MAX must be greater than or equal to EMAIL_OUTBOX_RETRY_BASE")
	}
	return validateSendMailHTTPConfig(cfg, production)
}

func validateSendMailHTTPConfig(cfg Config, production bool) error {
	raw := strings.TrimSpace(cfg.SendMailHTTPBaseURL)
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return fmt.Errorf("SENDMAIL_HTTP_BASE_URL must be an absolute URL")
	}
	if endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return fmt.Errorf("SENDMAIL_HTTP_BASE_URL must be a credential-free origin")
	}
	if endpoint.Path != "" && endpoint.Path != "/" {
		return fmt.Errorf("SENDMAIL_HTTP_BASE_URL must not contain a path")
	}
	if production && endpoint.Scheme != "https" {
		return fmt.Errorf("SENDMAIL_HTTP_BASE_URL must use https in production")
	}
	if strings.TrimSpace(cfg.SendMailHTTPBearerToken) == "" {
		return fmt.Errorf("SENDMAIL_HTTP_BEARER_TOKEN is required")
	}
	return validatePositiveDuration("SENDMAIL_HTTP_TIMEOUT", cfg.SendMailHTTPTimeout)
}

func validatePositiveDuration(key string, value time.Duration) error {
	if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("%s must be a positive duration", key)
		}
	}
	if value <= 0 {
		return fmt.Errorf("%s must be a positive duration", key)
	}
	return nil
}

func validatePositiveInt(key string, value int) error {
	if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("%s must be a positive integer", key)
		}
	}
	if value <= 0 {
		return fmt.Errorf("%s must be a positive integer", key)
	}
	return nil
}

func validatePKCS11JWTSigner(prefix, modulePath, tokenLabel, slotID, pin, keyLabel string) error {
	if strings.TrimSpace(modulePath) == "" {
		return fmt.Errorf("%s_PKCS11_MODULE_PATH is required", prefix)
	}
	if strings.TrimSpace(tokenLabel) == "" && strings.TrimSpace(slotID) == "" {
		return fmt.Errorf("%s_PKCS11_TOKEN_LABEL or %s_PKCS11_SLOT_ID is required", prefix, prefix)
	}
	if strings.TrimSpace(pin) == "" {
		return fmt.Errorf("%s_PKCS11_PIN is required", prefix)
	}
	if strings.TrimSpace(keyLabel) == "" {
		return fmt.Errorf("%s_PKCS11_KEY_LABEL is required", prefix)
	}
	return nil
}

func load() (Config, error) {
	if err := LoadDotEnv(".env"); err != nil {
		return Config{}, err
	}
	cfg := Config{
		DatabaseURL:                    getenv("DATABASE_URL", "postgres://rtk:rtk_password@localhost:5432/rtk_account_manager?sslmode=disable"),
		AccessSecret:                   os.Getenv("JWT_ACCESS_SECRET"),
		RefreshSecret:                  os.Getenv("JWT_REFRESH_SECRET"),
		JWTSignerProvider:              getenv("JWT_SIGNER_PROVIDER", "hs256"),
		JWTAccessPrivateKeyPath:        os.Getenv("JWT_ACCESS_PRIVATE_KEY_PATH"),
		JWTAccessPublicKeyPath:         os.Getenv("JWT_ACCESS_PUBLIC_KEY_PATH"),
		JWTRefreshPrivateKeyPath:       os.Getenv("JWT_REFRESH_PRIVATE_KEY_PATH"),
		JWTRefreshPublicKeyPath:        os.Getenv("JWT_REFRESH_PUBLIC_KEY_PATH"),
		JWTAccessPKCS11ModulePath:      os.Getenv("JWT_ACCESS_PKCS11_MODULE_PATH"),
		JWTAccessPKCS11TokenLabel:      os.Getenv("JWT_ACCESS_PKCS11_TOKEN_LABEL"),
		JWTAccessPKCS11SlotID:          os.Getenv("JWT_ACCESS_PKCS11_SLOT_ID"),
		JWTAccessPKCS11PIN:             os.Getenv("JWT_ACCESS_PKCS11_PIN"),
		JWTAccessPKCS11KeyLabel:        os.Getenv("JWT_ACCESS_PKCS11_KEY_LABEL"),
		JWTRefreshPKCS11ModulePath:     os.Getenv("JWT_REFRESH_PKCS11_MODULE_PATH"),
		JWTRefreshPKCS11TokenLabel:     os.Getenv("JWT_REFRESH_PKCS11_TOKEN_LABEL"),
		JWTRefreshPKCS11SlotID:         os.Getenv("JWT_REFRESH_PKCS11_SLOT_ID"),
		JWTRefreshPKCS11PIN:            os.Getenv("JWT_REFRESH_PKCS11_PIN"),
		JWTRefreshPKCS11KeyLabel:       os.Getenv("JWT_REFRESH_PKCS11_KEY_LABEL"),
		AccessTokenTTL:                 duration("ACCESS_TOKEN_TTL", 15*time.Minute),
		RefreshTokenTTL:                duration("REFRESH_TOKEN_TTL", 30*24*time.Hour),
		Port:                           getenv("PORT", "8080"),
		AuthTokenBaseURL:               getenv("AUTH_TOKEN_BASE_URL", ""),
		SendMailHTTPBaseURL:            getenv("SENDMAIL_HTTP_BASE_URL", ""),
		SendMailHTTPBearerToken:        getenv("SENDMAIL_HTTP_BEARER_TOKEN", ""),
		SendMailHTTPTimeout:            duration("SENDMAIL_HTTP_TIMEOUT", 15*time.Second),
		EmailOutboxEncryptionKey:       getenv("EMAIL_OUTBOX_ENCRYPTION_KEY", ""),
		EmailOutboxPollInterval:        duration("EMAIL_OUTBOX_POLL_INTERVAL", 5*time.Second),
		EmailOutboxBatchSize:           intValue("EMAIL_OUTBOX_BATCH_SIZE", 20),
		EmailOutboxMaxAttempts:         intValue("EMAIL_OUTBOX_MAX_ATTEMPTS", 8),
		EmailOutboxRetryBase:           duration("EMAIL_OUTBOX_RETRY_BASE", 30*time.Second),
		EmailOutboxRetryMax:            duration("EMAIL_OUTBOX_RETRY_MAX", 30*time.Minute),
		CrossServiceBroker:             getenv("CROSS_SERVICE_BROKER", "log"),
		AzureEventHubConnectionString:  getenv("AZURE_EVENTHUB_CONNECTION_STRING", ""),
		AzureEventHubCheckpointFile:    getenv("AZURE_EVENTHUB_CHECKPOINT_FILE", ""),
		AccountVideoCommandsStream:     getenv("ACCOUNT_VIDEO_COMMANDS_STREAM", "account.video.commands"),
		VideoAccountEventsStream:       getenv("VIDEO_ACCOUNT_EVENTS_STREAM", "video.account.events"),
		CrossServiceConsumerGroup:      getenv("CROSS_SERVICE_CONSUMER_GROUP", "rtk_account_manager"),
		CrossServiceMaxAttempts:        intValue("CROSS_SERVICE_MAX_ATTEMPTS", 5),
		CrossServicePollInterval:       duration("CROSS_SERVICE_POLL_INTERVAL", 5*time.Second),
		VideoCloudLifecycleBaseURL:     getenv("VIDEO_CLOUD_LIFECYCLE_BASE_URL", ""),
		VideoCloudLifecycleToken:       os.Getenv("VIDEO_CLOUD_LIFECYCLE_TOKEN"),
		VideoCloudLifecycleTimeout:     duration("VIDEO_CLOUD_LIFECYCLE_TIMEOUT", 10*time.Second),
		EmailVerificationTTL:           duration("EMAIL_VERIFICATION_TTL", 30*time.Minute),
		PasswordResetTTL:               duration("PASSWORD_RESET_TTL", 30*time.Minute),
		OTPResendInterval:              duration("OTP_RESEND_INTERVAL", 60*time.Second),
		OTPMaxAttempts:                 intValue("OTP_MAX_ATTEMPTS", 5),
		AuthTokenRateLimitMax:          intValue("AUTH_TOKEN_RATE_LIMIT_MAX", 5),
		AuthTokenRateLimitWindow:       duration("AUTH_TOKEN_RATE_LIMIT_WINDOW", time.Hour),
		OIDCEnabled:                    boolValue("OIDC_ENABLED", false),
		OIDCProviderID:                 getenv("OIDC_PROVIDER_ID", "keycloak"),
		OIDCProviderName:               getenv("OIDC_PROVIDER_NAME", "Keycloak"),
		OIDCIssuerURL:                  os.Getenv("OIDC_ISSUER_URL"),
		OIDCClientID:                   os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret:               os.Getenv("OIDC_CLIENT_SECRET"),
		OIDCRedirectURL:                os.Getenv("OIDC_REDIRECT_URL"),
		OIDCScopes:                     stringList("OIDC_SCOPES", []string{"openid", "email", "profile"}),
		OIDCAutoLinkEmail:              boolValue("OIDC_AUTO_LINK_EMAIL", false),
		UserCacheEnabled:               boolValue("ACCOUNT_MANAGER_USER_CACHE_ENABLED", false),
		UserCacheAddr:                  getenv("ACCOUNT_MANAGER_USER_CACHE_ADDR", "127.0.0.1:6379"),
		UserCachePrefix:                getenv("ACCOUNT_MANAGER_USER_CACHE_PREFIX", "account_manager:user"),
		AppCertIssuerBaseURL:           os.Getenv("APP_CERT_ISSUER_BASE_URL"),
		AppCertIssuerClientCert:        os.Getenv("APP_CERT_ISSUER_CLIENT_CERT"),
		AppCertIssuerClientKey:         os.Getenv("APP_CERT_ISSUER_CLIENT_KEY"),
		AppCertIssuerCAFile:            os.Getenv("APP_CERT_ISSUER_CA_FILE"),
		AppCertIssuerTimeout:           duration("APP_CERT_ISSUER_TIMEOUT", 10*time.Second),
		InternalAuthToken:              os.Getenv("ACCOUNT_MANAGER_INTERNAL_AUTH_TOKEN"),
		BillingHandoffBaseURL:          strings.TrimSpace(os.Getenv("BILLING_HANDOFF_BASE_URL")),
		BillingHandoffToken:            strings.TrimSpace(os.Getenv("BILLING_HANDOFF_TOKEN")),
		HandoffPollInterval:            duration("HANDOFF_WORKER_POLL_INTERVAL", 5*time.Second),
		HandoffLeaseDuration:           duration("HANDOFF_WORKER_LEASE_DURATION", 2*time.Minute),
		HandoffStepTimeout:             duration("HANDOFF_WORKER_STEP_TIMEOUT", 45*time.Second),
		HandoffBatchSize:               intValue("HANDOFF_WORKER_BATCH_SIZE", 10),
		CloudDeletionPollInterval:      duration("CLOUD_DELETION_WORKER_POLL_INTERVAL", 5*time.Second),
		CloudDeletionLeaseDuration:     duration("CLOUD_DELETION_WORKER_LEASE_DURATION", 2*time.Minute),
		CloudDeletionStepTimeout:       duration("CLOUD_DELETION_WORKER_STEP_TIMEOUT", 45*time.Second),
		CloudDeletionBatchSize:         intValue("CLOUD_DELETION_WORKER_BATCH_SIZE", 10),
		AllowImmediateBrandAccounts:    strings.EqualFold(strings.TrimSpace(os.Getenv("ACCOUNT_MANAGER_ENV")), "staging") && boolValue("ACCOUNT_MANAGER_ALLOW_IMMEDIATE_BRAND_ACCOUNTS", false),
		FactoryProductionJWTSecret:     os.Getenv("FACTORY_PRODUCTION_JWT_SECRET"),
		FactoryProductionJWTAudience:   getenv("FACTORY_PRODUCTION_JWT_AUDIENCE", "factory-enroll"),
		BootstrapPlatformAdminEmail:    os.Getenv("ACCOUNT_MANAGER_BOOTSTRAP_PLATFORM_ADMIN_EMAIL"),
		BootstrapPlatformAdminPassword: os.Getenv("ACCOUNT_MANAGER_BOOTSTRAP_PLATFORM_ADMIN_PASSWORD"),
		ChipsetProviderAllowedHosts:    stringList("CHIPSET_PROVIDER_ALLOWED_HOSTS", nil),
		ChipsetProviderRefreshInterval: duration("CHIPSET_PROVIDER_REFRESH_INTERVAL", time.Hour),
		LogEnv:                         getenv("ACCOUNT_MANAGER_ENV", "local"),
		LogVersion:                     getenv("ACCOUNT_MANAGER_VERSION", "dev"),
		LogLevel:                       getenv("ACCOUNT_MANAGER_LOG_LEVEL", "info"),
		LogDevelopment:                 boolValue("ACCOUNT_MANAGER_LOG_DEVELOPMENT", false),
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

func boolValue(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func stringList(key string, fallback []string) []string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return append([]string(nil), fallback...)
	}
	parts := strings.Fields(value)
	if len(parts) == 0 {
		return append([]string(nil), fallback...)
	}
	return parts
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
