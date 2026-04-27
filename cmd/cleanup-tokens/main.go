package main

import (
	"context"
	"log"
	"time"

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

	count, err := store.New(db).CleanupRefreshTokens(ctx, time.Now().UTC())
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("deleted %d expired or revoked refresh tokens", count)
}
