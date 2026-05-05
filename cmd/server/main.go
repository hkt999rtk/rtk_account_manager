package main

import (
	"context"
	"log"

	"rtk_account_manager/internal/api"
	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/mailer"
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
	mailerService := mailer.NewLogMailer(nil)
	server := api.NewWithMailer(store.New(db), authService, mailerService, api.AuthCodeOptions{
		EmailVerificationTTL: cfg.EmailVerificationTTL,
		PasswordResetTTL:     cfg.PasswordResetTTL,
		OTPResendInterval:    cfg.OTPResendInterval,
		OTPMaxAttempts:       cfg.OTPMaxAttempts,
	})

	log.Printf("listening on :%s", cfg.Port)
	if err := server.Router().Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
