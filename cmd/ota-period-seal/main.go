// ota-period-seal generates Account Manager's immutable Platform-grant half
// of an OTA Billing month checkpoint. A single Cloud previews unless --submit
// is set; --all-brand-clouds is the retryable scheduled submission path.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"rtk_account_manager/internal/billingotaseal"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/store"
)

type sealSource interface {
	ListOTAPeriodSealBrandCloudIDs(context.Context, time.Time) ([]string, error)
	BuildOTAPeriodGrantSeal(context.Context, string, time.Time, time.Time) (store.OTAPeriodGrantSeal, error)
}

type sealSink interface {
	Submit(context.Context, store.OTAPeriodGrantSeal) (bool, error)
}

func main() {
	organizationID := flag.String("organization-id", "", "Brand Cloud UUID")
	allBrandClouds := flag.Bool("all-brand-clouds", false, "submit the closed month for every existing Brand Cloud")
	month := flag.String("month", "", "closed UTC month, YYYY-MM or previous")
	submit := flag.Bool("submit", false, "submit the seal to Billing after preview")
	flag.Parse()
	if flag.NArg() != 0 || strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" ||
		(*allBrandClouds == (strings.TrimSpace(*organizationID) != "")) ||
		(*allBrandClouds && !*submit) {
		fatal("DATABASE_URL, a month, and exactly one of --organization-id or --all-brand-clouds are required; batch mode also requires --submit")
	}
	start, end, err := parseOTAMonth(*month, time.Now().UTC())
	if err != nil {
		fatal("month must be YYYY-MM or previous")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	db, err := database.Connect(connectCtx, os.Getenv("DATABASE_URL"))
	cancel()
	if err != nil {
		fatal("database connection failed")
	}
	defer db.Close()
	source := store.New(db)
	if *allBrandClouds {
		client, err := newSealClient()
		if err != nil {
			fatal("Billing OTA period seal endpoint is not configured")
		}
		if err := submitAllBrandClouds(ctx, source, client, start, end, os.Stdout); err != nil {
			fatal(err.Error())
		}
		return
	}
	oneCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	seal, err := source.BuildOTAPeriodGrantSeal(oneCtx, *organizationID, start, end)
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
	client, err := newSealClient()
	if err != nil {
		fatal("Billing OTA period seal endpoint is not configured")
	}
	duplicate, err := client.Submit(oneCtx, seal)
	if err != nil {
		fatal("Billing rejected or could not acknowledge the Platform OTA period seal")
	}
	if duplicate {
		fmt.Printf("Platform OTA period seal already recorded: %s\n", seal.SealID)
		return
	}
	fmt.Printf("Platform OTA period seal recorded: %s\n", seal.SealID)
}

func parseOTAMonth(value string, now time.Time) (time.Time, time.Time, error) {
	if value == "previous" {
		firstOfCurrent := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
		start := firstOfCurrent.AddDate(0, -1, 0)
		return start, firstOfCurrent, nil
	}
	start, err := time.Parse("2006-01", value)
	if err != nil || start.Format("2006-01") != value {
		return time.Time{}, time.Time{}, errors.New("invalid UTC month")
	}
	return start.UTC(), start.UTC().AddDate(0, 1, 0), nil
}

func newSealClient() (*billingotaseal.Client, error) {
	return billingotaseal.New(billingotaseal.Config{
		BaseURL: os.Getenv("BILLING_OTA_PERIOD_SEAL_BASE_URL"),
		Token:   os.Getenv("BILLING_OTA_PLATFORM_SEAL_TOKEN"),
	})
}

func submitAllBrandClouds(ctx context.Context, source sealSource, sink sealSink, start, end time.Time, output io.Writer) error {
	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	ids, err := source.ListOTAPeriodSealBrandCloudIDs(listCtx, end)
	cancel()
	if err != nil {
		return errors.New("could not enumerate Brand Clouds for OTA Platform seal")
	}
	created, duplicate, failed := 0, 0, 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		seal, err := source.BuildOTAPeriodGrantSeal(attemptCtx, id, start, end)
		var alreadyRecorded bool
		if err == nil {
			alreadyRecorded, err = sink.Submit(attemptCtx, seal)
		}
		cancel()
		if err != nil {
			failed++
			ref := sha256.Sum256([]byte(id))
			fmt.Fprintf(output, "OTA Platform seal failed for Brand Cloud SHA-256 %x\n", ref)
			continue
		}
		if alreadyRecorded {
			duplicate++
		} else {
			created++
		}
	}
	fmt.Fprintf(output, "OTA Platform seal month=%s brand_clouds=%d created=%d duplicate=%d failed=%d\n",
		start.Format("2006-01"), len(ids), created, duplicate, failed)
	if failed > 0 {
		return fmt.Errorf("%d OTA Platform seals were not acknowledged by Billing", failed)
	}
	return nil
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
