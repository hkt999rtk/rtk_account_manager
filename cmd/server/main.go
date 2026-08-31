package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"rtk_account_manager/internal/api"
	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/billingbootstrap"
	"rtk_account_manager/internal/billinghandoff"
	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/emaildelivery"
	"rtk_account_manager/internal/logging"
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
	accountStore := store.New(db)
	if cfg.BillingCloudCreationBaseURL != "" {
		client, err := billingbootstrap.New(billingbootstrap.Config{BaseURL: cfg.BillingCloudCreationBaseURL, Token: cfg.BillingCloudCreationToken})
		if err != nil {
			fatal(logger, "configure Billing cloud creation failed", err)
		}
		worker, err := billingbootstrap.NewWorker(accountStore, client, logger)
		if err != nil {
			fatal(logger, "configure Billing cloud creation worker failed", err)
		}
		go worker.Run(ctx)
	}
	if cfg.BillingHandoffBaseURL != "" {
		client, err := billinghandoff.New(billinghandoff.Config{BaseURL: cfg.BillingHandoffBaseURL, Token: cfg.BillingHandoffToken})
		if err != nil {
			fatal(logger, "configure Billing handoff transport failed", err)
		}
		if err := accountStore.ConfigureHandoffBilling(client); err != nil {
			fatal(logger, "configure Billing handoff observations failed", err)
		}
		// Resource observers are installed separately once their reviewed service
		// inventory is implemented. Until then, deletion preflight fails closed.
		if err := accountStore.ConfigureCloudDeletionPreflight(store.CloudDeletionPreflightOptions{Billing: client}); err != nil {
			fatal(logger, "configure Billing deletion preflight failed", err)
		}
	}
	accountStore.ConfigureAuthTokenRateLimit(cfg.AuthTokenRateLimitMax, cfg.AuthTokenRateLimitWindow)
	cipher, err := emaildelivery.NewCipher(cfg.EmailOutboxEncryptionKey)
	if err != nil {
		fatal(logger, "configure email outbox encryption failed", err)
	}
	accountStore.ConfigureEmailOutboxCipher(cipher)
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
	server := api.New(apiStore, authService)
	server.ConfigureAuthTokenTTLs(cfg.EmailVerificationTTL, cfg.PasswordResetTTL)
	server.ConfigureEmailOutbox(accountStore)
	server.SetLogger(logger)
	server.ConfigureInternalAuthToken(cfg.InternalAuthToken)
	server.ConfigureImmediateBrandAccountProvisioning(cfg.AllowImmediateBrandAccounts)
	server.ConfigureProductionJWT(cfg.FactoryProductionJWTSecret, cfg.FactoryProductionJWTAudience)
	server.ConfigureFactoryEnrollmentToken(cfg.FactoryEnrollmentToken)
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

func fatal(logger *zap.Logger, message string, err error, fields ...zap.Field) {
	if err != nil {
		fields = append(fields, zap.Error(err))
	}
	logger.Error(message, fields...)
	logging.Sync(logger)
	os.Exit(1)
}
