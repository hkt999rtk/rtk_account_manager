package main

import (
	"context"
	"log"

	"github.com/kevinhuang/rtk_account_manager/internal/api"
	"github.com/kevinhuang/rtk_account_manager/internal/auth"
	"github.com/kevinhuang/rtk_account_manager/internal/config"
	"github.com/kevinhuang/rtk_account_manager/internal/database"
	"github.com/kevinhuang/rtk_account_manager/internal/store"
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
	server := api.New(store.New(db), authService)

	log.Printf("listening on :%s", cfg.Port)
	if err := server.Router().Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
