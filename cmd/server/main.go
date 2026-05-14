package main

import (
	"context"
	"log"
	"net/smtp"
	"strings"

	"rtk_account_manager/internal/api"
	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/store"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	authService := auth.NewService(cfg.AccessSecret, cfg.RefreshSecret, cfg.AccessTokenTTL, cfg.RefreshTokenTTL)
	var authTokenSink api.AuthTokenSink
	switch cfg.AuthTokenDelivery {
	case "log":
		authTokenSink = api.NewLogAuthTokenSink(log.Default())
	default:
		log.Fatalf("unsupported AUTH_TOKEN_DELIVERY %q", cfg.AuthTokenDelivery)
	}
	notificationSink := quotaRaiseNotificationSink(cfg)
	accountStore := store.New(db)
	server := api.NewWithAuthTokenAndNotificationSink(accountStore, authService, authTokenSink, notificationSink)
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

	log.Printf("listening on :%s", cfg.Port)
	if err := server.Router().Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}

func quotaRaiseNotificationSink(cfg config.Config) api.QuotaRaiseNotificationSink {
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
	return api.NewLogQuotaRaiseNotificationSink(log.Default())
}
