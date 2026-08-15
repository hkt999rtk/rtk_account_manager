package config

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
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
	AuthTokenDelivery              string
	AuthTokenBaseURL               string
	SMTPHost                       string
	SMTPPort                       string
	SMTPUsername                   string
	SMTPPassword                   string
	SMTPFrom                       string
	SMTPFromName                   string
	SMTPEncryption                 string
	SendMailHTTPBaseURL            string
	SendMailHTTPBearerToken        string
	SendMailHTTPTimeout            time.Duration
	EmailOutboxEncryptionKey       string
	EmailOutboxPollInterval        time.Duration
	EmailOutboxBatchSize           int
	EmailOutboxMaxAttempts         int
	EmailOutboxRetryBase           time.Duration
	EmailOutboxRetryMax            time.Duration
	PaymentWorkerEnabled           bool
	PaymentReferenceEncryptionKey  string
	PaymentWorkerPollInterval      time.Duration
	PaymentWorkerBatchSize         int
	PaymentWorkerLeaseDuration     time.Duration
	PaymentReconciliationDelay     time.Duration
	NewebPayEnabled                bool
	NewebPayEnvironment            string
	NewebPayMerchantID             string
	NewebPayHashKey                string
	NewebPayHashIV                 string
	NewebPayRequestTimeout         time.Duration
	NewebPayAutoChargeEnabled      bool
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
	if err := validatePaymentBooleanEnv(); err != nil {
		return Config{}, err
	}
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
	if err := validateEmailConfig(cfg, false); err != nil {
		return Config{}, err
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

func LoadEmailWorker() (Config, error) {
	cfg, err := load()
	if err != nil {
		return Config{}, err
	}
	if err := validateEmailConfig(cfg, true); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func LoadPaymentWorker() (Config, error) {
	if err := validatePaymentBooleanEnv(); err != nil {
		return Config{}, err
	}
	cfg, err := load()
	if err != nil {
		return Config{}, err
	}
	if cfg.NewebPayAutoChargeEnabled {
		return Config{}, fmt.Errorf("NEWEBPAY_MERCHANT_INITIATED_CHARGE_ENABLED is unsupported until merchant capability approval and sandbox qualification")
	}
	if !cfg.PaymentWorkerEnabled {
		if cfg.NewebPayEnabled {
			return Config{}, fmt.Errorf("NEWEBPAY_ENABLED requires PAYMENT_WORKER_ENABLED")
		}
		return cfg, nil
	}
	if !cfg.NewebPayEnabled {
		return Config{}, fmt.Errorf("PAYMENT_WORKER_ENABLED requires an enabled payment provider")
	}
	decoded, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(cfg.PaymentReferenceEncryptionKey))
	if decodeErr != nil || len(decoded) != 32 {
		return Config{}, fmt.Errorf("PAYMENT_REFERENCE_ENCRYPTION_KEY must be base64 encoded 32 bytes")
	}
	environment := strings.ToLower(strings.TrimSpace(cfg.NewebPayEnvironment))
	if environment != "sandbox" && environment != "production" {
		return Config{}, fmt.Errorf("NEWEBPAY_ENVIRONMENT must be sandbox or production")
	}
	if strings.TrimSpace(cfg.NewebPayMerchantID) == "" || len(strings.TrimSpace(cfg.NewebPayMerchantID)) > 15 ||
		len(strings.TrimSpace(cfg.NewebPayHashKey)) != 32 || len(strings.TrimSpace(cfg.NewebPayHashIV)) != 16 {
		return Config{}, fmt.Errorf("enabled NewebPay requires MerchantID (max 15), 32-byte HashKey, and 16-byte HashIV")
	}
	if cfg.PaymentWorkerPollInterval <= 0 || cfg.PaymentWorkerLeaseDuration <= 0 ||
		cfg.PaymentReconciliationDelay <= 0 || cfg.PaymentWorkerBatchSize <= 0 ||
		cfg.NewebPayRequestTimeout <= 0 {
		return Config{}, fmt.Errorf("payment worker durations and batch size must be positive")
	}
	return cfg, nil
}

func validatePaymentBooleanEnv() error {
	for _, key := range []string{"PAYMENT_WORKER_ENABLED", "NEWEBPAY_ENABLED", "NEWEBPAY_MERCHANT_INITIATED_CHARGE_ENABLED"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			if _, err := strconv.ParseBool(value); err != nil {
				return fmt.Errorf("%s must be true or false", key)
			}
		}
	}
	return nil
}

func validateEmailConfig(cfg Config, worker bool) error {
	production := strings.EqualFold(strings.TrimSpace(cfg.LogEnv), "production")
	delivery := strings.ToLower(strings.TrimSpace(cfg.AuthTokenDelivery))
	emailDelivery := delivery == "smtp" || delivery == "sendmail_http"
	if production && !emailDelivery {
		return fmt.Errorf("AUTH_TOKEN_DELIVERY must be smtp or sendmail_http in production")
	}
	if !worker && !emailDelivery {
		return nil
	}
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
	if delivery == "sendmail_http" {
		return validateSendMailHTTPConfig(cfg, production)
	}
	if strings.TrimSpace(cfg.SMTPHost) == "" {
		return fmt.Errorf("SMTP_HOST is required")
	}
	if strings.TrimSpace(cfg.SMTPFrom) == "" {
		return fmt.Errorf("SMTP_FROM is required")
	}
	if _, err := mail.ParseAddress(strings.TrimSpace(cfg.SMTPFrom)); err != nil {
		return fmt.Errorf("SMTP_FROM must be a valid mailbox: %w", err)
	}
	encryption := strings.ToLower(strings.TrimSpace(cfg.SMTPEncryption))
	if encryption != "starttls" && encryption != "none" {
		return fmt.Errorf("SMTP_ENCRYPTION must be starttls or none")
	}
	if production && encryption != "starttls" {
		return fmt.Errorf("SMTP_ENCRYPTION must be starttls in production")
	}
	if production && (strings.TrimSpace(cfg.SMTPUsername) == "" || strings.TrimSpace(cfg.SMTPPassword) == "") {
		return fmt.Errorf("SMTP_USERNAME and SMTP_PASSWORD are required in production")
	}
	return nil
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
	if cfg.SendMailHTTPTimeout <= 0 {
		return fmt.Errorf("SENDMAIL_HTTP_TIMEOUT must be positive")
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
		AuthTokenDelivery:              getenv("AUTH_TOKEN_DELIVERY", "log"),
		AuthTokenBaseURL:               getenv("AUTH_TOKEN_BASE_URL", ""),
		SMTPHost:                       getenv("SMTP_HOST", ""),
		SMTPPort:                       getenv("SMTP_PORT", "587"),
		SMTPUsername:                   getenv("SMTP_USERNAME", ""),
		SMTPPassword:                   getenv("SMTP_PASSWORD", ""),
		SMTPFrom:                       getenv("SMTP_FROM", ""),
		SMTPFromName:                   getenv("SMTP_FROM_NAME", "Realtek Connect"),
		SMTPEncryption:                 getenv("SMTP_ENCRYPTION", "starttls"),
		SendMailHTTPBaseURL:            getenv("SENDMAIL_HTTP_BASE_URL", ""),
		SendMailHTTPBearerToken:        getenv("SENDMAIL_HTTP_BEARER_TOKEN", ""),
		SendMailHTTPTimeout:            duration("SENDMAIL_HTTP_TIMEOUT", 15*time.Second),
		EmailOutboxEncryptionKey:       getenv("EMAIL_OUTBOX_ENCRYPTION_KEY", ""),
		EmailOutboxPollInterval:        duration("EMAIL_OUTBOX_POLL_INTERVAL", 5*time.Second),
		EmailOutboxBatchSize:           intValue("EMAIL_OUTBOX_BATCH_SIZE", 20),
		EmailOutboxMaxAttempts:         intValue("EMAIL_OUTBOX_MAX_ATTEMPTS", 8),
		EmailOutboxRetryBase:           duration("EMAIL_OUTBOX_RETRY_BASE", 30*time.Second),
		EmailOutboxRetryMax:            duration("EMAIL_OUTBOX_RETRY_MAX", 30*time.Minute),
		PaymentWorkerEnabled:           boolValue("PAYMENT_WORKER_ENABLED", false),
		PaymentReferenceEncryptionKey:  os.Getenv("PAYMENT_REFERENCE_ENCRYPTION_KEY"),
		PaymentWorkerPollInterval:      duration("PAYMENT_WORKER_POLL_INTERVAL", 5*time.Second),
		PaymentWorkerBatchSize:         intValue("PAYMENT_WORKER_BATCH_SIZE", 20),
		PaymentWorkerLeaseDuration:     duration("PAYMENT_WORKER_LEASE_DURATION", 30*time.Second),
		PaymentReconciliationDelay:     duration("PAYMENT_RECONCILIATION_DELAY", time.Minute),
		NewebPayEnabled:                boolValue("NEWEBPAY_ENABLED", false),
		NewebPayEnvironment:            getenv("NEWEBPAY_ENVIRONMENT", "sandbox"),
		NewebPayMerchantID:             os.Getenv("NEWEBPAY_MERCHANT_ID"),
		NewebPayHashKey:                os.Getenv("NEWEBPAY_HASH_KEY"),
		NewebPayHashIV:                 os.Getenv("NEWEBPAY_HASH_IV"),
		NewebPayRequestTimeout:         duration("NEWEBPAY_REQUEST_TIMEOUT", 10*time.Second),
		NewebPayAutoChargeEnabled:      boolValue("NEWEBPAY_MERCHANT_INITIATED_CHARGE_ENABLED", false),
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
