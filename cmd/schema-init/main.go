package main

import (
	"context"
	"log"
	"os"
	"strings"

	"rtk_account_manager/internal/database"
)

// schema-init applies production migrations to an isolated database without
// requiring service authentication or launching background workers.
func main() {
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		log.Fatal(err)
	}
}
