package main

import (
	"context"
	"net/http"
	"net/smtp"
	"os"
	"strings"

	"rtk_account_manager/internal/api"
	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/logging"
	"rtk_account_manager/internal/store"

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

	authService := auth.NewService(cfg.AccessSecret, cfg.RefreshSecret, cfg.AccessTokenTTL, cfg.RefreshTokenTTL)
	var authTokenSink api.AuthTokenSink
	switch cfg.AuthTokenDelivery {
	case "log":
		authTokenSink = api.NewLogAuthTokenSink(logger)
	default:
		fatal(logger, "unsupported auth token delivery", nil, zap.String("delivery", cfg.AuthTokenDelivery))
	}
	notificationSink := quotaRaiseNotificationSink(cfg, logger)
	accountStore := store.New(db)
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
	server := api.NewWithAuthTokenAndNotificationSink(accountStore, authService, authTokenSink, notificationSink)
	server.SetLogger(logger)
	server.ConfigureInternalAuthToken(cfg.InternalAuthToken)
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

func quotaRaiseNotificationSink(cfg config.Config, logger *zap.Logger) api.QuotaRaiseNotificationSink {
	if cfg.SMTPHost != "" && cfg.SMTPFrom != "" {
		addr := cfg.SMTPHost
		if cfg.SMTPPort != "" && !strings.Contains(addr, ":") {
			addr = addr + ":" + cfg.SMTPPort
		}
		var auth smtp.Auth
		if cfg.SMTPUsername != "" || cfg.SMTPPassword != "" {
			host := cfg.SMTPHost
			if i := strings.Index(host, ":"); i >= 0 {
				host = host[:i]
			}
			auth = smtp.PlainAuth("", cfg.SMTPUsername, cfg.SMTPPassword, host)
		}
		return api.NewSMTPQuotaRaiseNotificationSink(addr, cfg.SMTPFrom, auth)
	}
	return api.NewLogQuotaRaiseNotificationSink(logger)
}

func fatal(logger *zap.Logger, message string, err error, fields ...zap.Field) {
	if err != nil {
		fields = append(fields, zap.Error(err))
	}
	logger.Error(message, fields...)
	logging.Sync(logger)
	os.Exit(1)
}
