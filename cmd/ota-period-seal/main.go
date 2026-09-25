// ota-period-seal generates Account Manager's immutable Platform-grant half
// of an OTA Billing month checkpoint. It previews JSON unless --submit is set.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"rtk_account_manager/internal/billingotaseal"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/store"
)

func main() {
	organizationID := flag.String("organization-id", "", "Brand Cloud UUID")
	month := flag.String("month", "", "closed UTC month, YYYY-MM")
	submit := flag.Bool("submit", false, "submit the seal to Billing after preview")
	flag.Parse()
	if flag.NArg() != 0 || strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" ||
		strings.TrimSpace(*organizationID) == "" {
		fatal("organization ID and DATABASE_URL are required")
	}
	start, err := time.Parse("2006-01", *month)
	if err != nil || start.Format("2006-01") != *month {
		fatal("month must be YYYY-MM")
	}
	start = start.UTC()
	end := start.AddDate(0, 1, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := database.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		fatal("database connection failed")
	}
	defer db.Close()
	seal, err := store.New(db).BuildOTAPeriodGrantSeal(ctx, *organizationID, start, end)
	if err != nil {
		fatal("Platform OTA grant history is not ready to seal")
	}
	preview, err := json.MarshalIndent(seal, "", "  ")
	if err != nil {
		fatal("could not encode seal")
	}
	if !*submit {
		fmt.Println(string(preview))
		return
	}
	client, err := billingotaseal.New(billingotaseal.Config{
		BaseURL: os.Getenv("BILLING_OTA_PERIOD_SEAL_BASE_URL"),
		Token:   os.Getenv("BILLING_OTA_PLATFORM_SEAL_TOKEN"),
	})
	if err != nil {
		fatal("Billing OTA period seal endpoint is not configured")
	}
	duplicate, err := client.Submit(ctx, seal)
	if err != nil {
		fatal("Billing rejected or could not acknowledge the Platform OTA period seal")
	}
	if duplicate {
		fmt.Printf("Platform OTA period seal already recorded: %s\n", seal.SealID)
		return
	}
	fmt.Printf("Platform OTA period seal recorded: %s\n", seal.SealID)
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
