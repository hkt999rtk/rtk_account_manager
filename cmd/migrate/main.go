package main

import (
	"context"
	"log"

	"github.com/hkt999rtk/rtk_account_manager/internal/config"
	"github.com/hkt999rtk/rtk_account_manager/internal/database"
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

	if err := database.Migrate(ctx, db); err != nil {
		log.Fatal(err)
	}
	log.Println("migrations applied")
}
