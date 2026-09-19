// device-pki-admin is deliberately separate from server startup.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Device PKI maintenance failed; verify the selected environment and inventory")
		os.Exit(1)
	}
}

func run() error {
	cloud := flag.String("cloud-id", "", "one reviewed Cloud UUID")
	root := flag.String("replacement-root-id", "", "verified replacement Device Root UUID")
	confirm := flag.String("confirm-environment", "", "required environment for apply")
	apply := flag.Bool("apply", false, "requeue reviewed ready/failed jobs; default is inventory only")
	flag.Parse()
	environment := os.Getenv("PKI_ENVIRONMENT")
	if (environment != "dev" && environment != "staging") || (*apply && *confirm != environment) || flag.NArg() != 0 || os.Getenv("DATABASE_URL") == "" {
		return fmt.Errorf("explicit non-production environment and scoped database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := database.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()
	targets, err := store.New(db).RequeueDevicePKI(ctx, environment, *cloud, *root, *apply)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Environment     string                         `json:"environment"`
		CloudID         string                         `json:"cloud_id"`
		ReplacementRoot string                         `json:"replacement_root_id"`
		Applied         bool                           `json:"applied"`
		Targets         []store.DevicePKIRequeueTarget `json:"targets"`
	}{environment, *cloud, *root, *apply, targets})
}
