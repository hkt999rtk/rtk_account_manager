package main

import (
	"context"
	"log"

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
	notificationSink := api.NewLogQuotaRaiseNotificationSink(log.Default())
	server := api.NewWithAuthTokenAndNotificationSink(store.New(db), authService, authTokenSink, notificationSink)

	log.Printf("listening on :%s", cfg.Port)
	if err := server.Router().Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
