package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"rtk_account_manager/internal/readiness"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	opts := readiness.OptionsFromEnv()

	flags := flag.NewFlagSet("readiness-smoke", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.BaseURL, "base-url", opts.BaseURL, "account-manager API base URL")
	flags.StringVar(&opts.ServiceVersion, "service-version", opts.ServiceVersion, "service version or deployment build identifier to record")
	flags.StringVar(&opts.Email, "email", opts.Email, "existing smoke user email")
	flags.StringVar(&opts.Password, "password", opts.Password, "existing smoke user password")
	flags.StringVar(&opts.OrganizationID, "org-id", opts.OrganizationID, "existing organization id to verify")
	flags.StringVar(&opts.DeviceID, "device-id", opts.DeviceID, "existing device id to verify")
	flags.StringVar(&opts.DatabaseURL, "database-url", opts.DatabaseURL, "Postgres URL for read-only migration evidence")
	flags.StringVar(&opts.MigrationsDir, "migrations-dir", opts.MigrationsDir, "local migrations directory")
	flags.StringVar(&opts.Broker, "cross-service-broker", opts.Broker, "cross-service broker adapter to record as optional evidence")
	flags.StringVar(&opts.CommandStream, "command-stream", opts.CommandStream, "account-to-video command stream name")
	flags.StringVar(&opts.EventStream, "event-stream", opts.EventStream, "video-to-account event stream name")
	outputPath := flags.String("output", "", "write JSON report artifact to this path instead of stdout")
	timeout := flags.Duration("timeout", 30*time.Second, "overall smoke timeout")
	flags.BoolVar(&opts.DryRun, "dry-run", opts.DryRun, "validate configuration and emit a report without network or database calls")
	if err := flags.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	report, runErr := readiness.Run(ctx, opts)
	if *outputPath != "" {
		if dir := filepath.Dir(*outputPath); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		file, err := os.Create(*outputPath)
		if err != nil {
			return err
		}
		if err := readiness.WriteReport(file, report); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	} else if err := readiness.WriteReport(stdout, report); err != nil {
		return err
	}
	return runErr
}
