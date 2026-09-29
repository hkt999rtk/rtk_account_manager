package main

import (
	"context"
	"log"
	"os"

	"rtk_account_manager/internal/database"
)

// schema-init applies production migrations to an isolated database without
// requiring service authentication or launching background workers.
func main() {
	ctx := context.Background()
	pool, err := database.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		log.Fatal(err)
	}
}
