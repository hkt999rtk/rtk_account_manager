package main

import (
	"context"
	"fmt"
	"net/http"
	"net/smtp"
	"os"
	"strings"

	"rtk_account_manager/internal/api"
	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/billingservice"
	"rtk_account_manager/internal/billingstore"
	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/emaildelivery"
	"rtk_account_manager/internal/logging"
	"rtk_account_manager/internal/payment"
	"rtk_account_manager/internal/paymentcrypto"
	"rtk_account_manager/internal/paymentprovider/newebpay"
	paymentSimulator "rtk_account_manager/internal/paymentprovider/simulator"
	"rtk_account_manager/internal/paymentstore"
	"rtk_account_manager/internal/store"
	"rtk_account_manager/internal/usercache"

	"go.uber.org/zap"
)

func main() {
	earlyLogger := logging.NewFromEnv(logging.ServiceAPI)
	cfg, err := config.Load()
	if err != nil {
		fatal(earlyLogger, "load config failed", err)
	}
	logging.Sync(earlyLogger)
	logger, err := logging.New(logging.ServiceAPI, cfg)
	if err != nil {
		fatal(earlyLogger, "create logger failed", err)
	}
	defer logging.Sync(logger)

	ctx := context.Background()
	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		fatal(logger, "database connection failed", err)
	}
	defer db.Close()

	authService, err := newAuthService(cfg)
	if err != nil {
		fatal(logger, "configure auth token signer failed", err)
	}
	var authTokenSink api.AuthTokenSink
	switch cfg.AuthTokenDelivery {
	case "log":
		authTokenSink = api.NewLogAuthTokenSink(logger)
	case "smtp", "sendmail_http":
		// Email delivery is performed by the durable email worker.
		authTokenSink = nil
	default:
		fatal(logger, "unsupported auth token delivery", nil, zap.String("delivery", cfg.AuthTokenDelivery))
	}
	var notificationSink api.QuotaRaiseNotificationSink
	if cfg.AuthTokenDelivery == "log" {
		notificationSink = api.NewLogQuotaRaiseNotificationSink(logger)
	}
	accountStore := store.New(db)
	accountStore.ConfigureAuthTokenRateLimit(cfg.AuthTokenRateLimitMax, cfg.AuthTokenRateLimitWindow)
	if emailOutboxDelivery(cfg.AuthTokenDelivery) {
		cipher, err := emaildelivery.NewCipher(cfg.EmailOutboxEncryptionKey)
		if err != nil {
			fatal(logger, "configure email outbox encryption failed", err)
		}
		accountStore.ConfigureEmailOutboxCipher(cipher)
	}
	if cfg.BootstrapPlatformAdminEmail != "" || cfg.BootstrapPlatformAdminPassword != "" {
		if cfg.BootstrapPlatformAdminEmail == "" || cfg.BootstrapPlatformAdminPassword == "" {
			fatal(logger, "bootstrap platform admin config incomplete", nil)
		}
		hash, err := auth.HashPassword(cfg.BootstrapPlatformAdminPassword)
		if err != nil {
			fatal(logger, "hash bootstrap platform admin password failed", err)
		}
		displayName := "Realtek Platform Admin"
		admin, err := accountStore.EnsurePlatformAdmin(ctx, cfg.BootstrapPlatformAdminEmail, hash, &displayName)
		if err != nil {
			fatal(logger, "ensure platform admin failed", err)
		}
		logger.Info("platform admin ensured", zap.String("email", admin.Email), zap.String("user_id", admin.ID))
	}
	var apiStore api.Store = accountStore
	if cfg.UserCacheEnabled {
		cache := usercache.NewRedisCache(usercache.Config{
			Addr:   cfg.UserCacheAddr,
			Prefix: cfg.UserCachePrefix,
		})
		apiStore = usercache.NewStore(apiStore, cache, logger)
		logger.Info("user cache enabled", zap.String("addr", cfg.UserCacheAddr), zap.String("prefix", cfg.UserCachePrefix))
	}
	server := api.NewWithAuthTokenAndNotificationSink(apiStore, authService, authTokenSink, notificationSink)
	paymentProviders := make([]payment.PaymentProvider, 0, 1)
	if cfg.PaymentSimulatorEnabled {
		provider, providerErr := paymentSimulator.New(paymentSimulator.Config{
			BaseURL: cfg.PaymentSimulatorBaseURL, SharedSecret: cfg.PaymentSimulatorSharedSecret,
			RunID: cfg.PaymentSimulatorRunID, Scenario: cfg.PaymentSimulatorScenario, Timeout: cfg.NewebPayRequestTimeout,
		})
		if providerErr != nil {
			fatal(logger, "configure payment simulator client failed", providerErr)
		}
		paymentProviders = append(paymentProviders, provider)
	}
	if cfg.NewebPayEnabled {
		provider, providerErr := newebpay.New(newebpay.Config{
			Enabled: true, Environment: cfg.NewebPayEnvironment, MerchantID: cfg.NewebPayMerchantID,
			HashKey: cfg.NewebPayHashKey, HashIV: cfg.NewebPayHashIV, Timeout: cfg.NewebPayRequestTimeout,
		})
		if providerErr != nil {
			// Payment routes fail closed, while identity and registry APIs remain
			// available. The error contains no credential values.
			logger.Error("payment webhook verifier disabled", zap.Error(providerErr))
		} else {
			paymentProviders = append(paymentProviders, provider)
		}
	}
	var paymentReferenceProtector api.PaymentReferenceProtector
	if strings.TrimSpace(cfg.PaymentReferenceEncryptionKey) != "" {
		paymentReferenceProtector, err = paymentcrypto.New(cfg.PaymentReferenceEncryptionKey)
		if err != nil {
			fatal(logger, "configure payment reference protection failed", err)
		}
	}
	paymentStore := paymentstore.New(db)
	if err := server.ConfigurePayments(api.PaymentAPIOptions{
		Store: paymentStore, Providers: paymentProviders,
		ReferenceProtector: paymentReferenceProtector,
		BillingDebitToken:  cfg.BillingDebitToken, BillingDebitSource: cfg.BillingDebitSource,
		SimulatorCallbackSecret: cfg.PaymentSimulatorCallbackSecret,
	}); err != nil {
		fatal(logger, "configure payment API failed", err)
	}
	billingStore := billingstore.New(db)
	billingService, err := billingservice.New(billingservice.Options{Store: billingStore, PaymentStore: paymentStore})
	if err != nil {
		fatal(logger, "configure billing service failed", err)
	}
	if err := server.ConfigureBilling(api.BillingAPIOptions{Store: billingStore, Service: billingService}); err != nil {
		fatal(logger, "configure billing API failed", err)
	}
	if emailOutboxDelivery(cfg.AuthTokenDelivery) {
		server.ConfigureEmailOutbox(accountStore)
	}
	server.SetLogger(logger)
	server.ConfigureInternalAuthToken(cfg.InternalAuthToken)
	server.ConfigureProductionJWT(cfg.FactoryProductionJWTSecret, cfg.FactoryProductionJWTAudience)
	server.ConfigureChipsetManifestFetcher(api.NewChipsetManifestFetcher(api.ChipsetManifestFetcherConfig{AllowedHosts: cfg.ChipsetProviderAllowedHosts}))
	if cfg.ChipsetProviderRefreshInterval > 0 {
		go server.RunChipsetProviderRefresh(ctx, cfg.ChipsetProviderRefreshInterval)
	}
	server.ConfigureOIDC(api.OIDCOptions{
		Env: auth.OIDCEnvConfig{
			Enabled:       cfg.OIDCEnabled,
			ProviderID:    cfg.OIDCProviderID,
			ProviderName:  cfg.OIDCProviderName,
			IssuerURL:     cfg.OIDCIssuerURL,
			ClientID:      cfg.OIDCClientID,
			ClientSecret:  cfg.OIDCClientSecret,
			RedirectURL:   cfg.OIDCRedirectURL,
			Scopes:        cfg.OIDCScopes,
			AutoLinkEmail: cfg.OIDCAutoLinkEmail,
		},
	})
	if strings.TrimSpace(cfg.AppCertIssuerBaseURL) != "" {
		issuer, err := api.NewHTTPAppCertificateIssuer(api.HTTPAppCertificateIssuerConfig{
			BaseURL:    cfg.AppCertIssuerBaseURL,
			ClientCert: cfg.AppCertIssuerClientCert,
			ClientKey:  cfg.AppCertIssuerClientKey,
			CAFile:     cfg.AppCertIssuerCAFile,
			Timeout:    cfg.AppCertIssuerTimeout,
		})
		if err != nil {
			fatal(logger, "configure app certificate issuer failed", err)
		}
		server.ConfigureAppCertificateIssuer(issuer)
	}

	addr := ":" + cfg.Port
	logger.Info("server listening", zap.String("addr", addr))
	httpServer := &http.Server{
		Addr:    addr,
		Handler: server.Router(),
	}
	if err := httpServer.ListenAndServe(); err != nil {
		fatal(logger, "server stopped", err)
	}
}

func emailOutboxDelivery(delivery string) bool {
	return delivery == "smtp" || delivery == "sendmail_http"
}

func newAuthService(cfg config.Config) (*auth.Service, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.JWTSignerProvider)) {
	case "", "hs256":
		return auth.NewService(cfg.AccessSecret, cfg.RefreshSecret, cfg.AccessTokenTTL, cfg.RefreshTokenTTL), nil
	case "pem":
		accessSigner, err := auth.LoadPEMTokenSigner(cfg.JWTAccessPrivateKeyPath, cfg.JWTAccessPublicKeyPath)
		if err != nil {
			return nil, err
		}
		refreshSigner, err := auth.LoadPEMTokenSigner(cfg.JWTRefreshPrivateKeyPath, cfg.JWTRefreshPublicKeyPath)
		if err != nil {
			return nil, err
		}
		return auth.NewServiceWithSigners(accessSigner, refreshSigner, cfg.AccessTokenTTL, cfg.RefreshTokenTTL), nil
	case "pkcs11":
		accessSigner, err := auth.LoadPKCS11TokenSigner(auth.PKCS11TokenSignerConfig{
			ModulePath: cfg.JWTAccessPKCS11ModulePath,
			TokenLabel: cfg.JWTAccessPKCS11TokenLabel,
			SlotID:     cfg.JWTAccessPKCS11SlotID,
			PIN:        cfg.JWTAccessPKCS11PIN,
			KeyLabel:   cfg.JWTAccessPKCS11KeyLabel,
		})
		if err != nil {
			return nil, err
		}
		refreshSigner, err := auth.LoadPKCS11TokenSigner(auth.PKCS11TokenSignerConfig{
			ModulePath: cfg.JWTRefreshPKCS11ModulePath,
			TokenLabel: cfg.JWTRefreshPKCS11TokenLabel,
			SlotID:     cfg.JWTRefreshPKCS11SlotID,
			PIN:        cfg.JWTRefreshPKCS11PIN,
			KeyLabel:   cfg.JWTRefreshPKCS11KeyLabel,
		})
		if err != nil {
			return nil, err
		}
		return auth.NewServiceWithSigners(accessSigner, refreshSigner, cfg.AccessTokenTTL, cfg.RefreshTokenTTL), nil
	default:
		return nil, fmt.Errorf("unsupported JWT signer provider %q", cfg.JWTSignerProvider)
	}
}

func quotaRaiseNotificationSink(cfg config.Config, logger *zap.Logger) api.QuotaRaiseNotificationSink {
	if cfg.SMTPHost != "" && cfg.SMTPFrom != "" {
		addr, auth, err := smtpConfig(cfg)
		if err != nil {
			logger.Warn("SMTP quota raise notification sink unavailable", zap.Error(err))
		} else {
			return api.NewSMTPQuotaRaiseNotificationSink(addr, cfg.SMTPFrom, auth)
		}
	}
	return api.NewLogQuotaRaiseNotificationSink(logger)
}

func smtpConfig(cfg config.Config) (string, smtp.Auth, error) {
	if strings.TrimSpace(cfg.SMTPHost) == "" {
		return "", nil, fmt.Errorf("SMTP_HOST is required")
	}
	if strings.TrimSpace(cfg.SMTPFrom) == "" {
		return "", nil, fmt.Errorf("SMTP_FROM is required")
	}
	addr := strings.TrimSpace(cfg.SMTPHost)
	if cfg.SMTPPort != "" && !strings.Contains(addr, ":") {
		addr = addr + ":" + strings.TrimSpace(cfg.SMTPPort)
	}
	var auth smtp.Auth
	if cfg.SMTPUsername != "" || cfg.SMTPPassword != "" {
		host := strings.TrimSpace(cfg.SMTPHost)
		if i := strings.Index(host, ":"); i >= 0 {
			host = host[:i]
		}
		auth = smtp.PlainAuth("", cfg.SMTPUsername, cfg.SMTPPassword, host)
	}
	return addr, auth, nil
}

func fatal(logger *zap.Logger, message string, err error, fields ...zap.Field) {
	if err != nil {
		fields = append(fields, zap.Error(err))
	}
	logger.Error(message, fields...)
	logging.Sync(logger)
	os.Exit(1)
}
