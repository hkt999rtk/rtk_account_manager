package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"strings"

	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/logging"
	"rtk_account_manager/internal/schemamaintenance"
	"rtk_account_manager/internal/store"

	"go.uber.org/zap"
)

func main() {
	schemaAction := flag.String("schema-maintenance", "", "check, apply or verify the database simplification on a write-frozen database")
	identityPreflight := flag.Bool("identity-preflight", false, "report forward identity correction on a restored write-frozen database; always roll back")
	serviceGrantReport := flag.Bool("service-grant-backfill-report", false, "report legacy Product service grants without writing")
	serviceGrantApply := flag.Bool("service-grant-backfill-apply", false, "backfill legacy Product service grants atomically after a reviewed report")
	serviceGrantExpected := flag.String("service-grant-backfill-expected-sha256", "", "SHA-256 snapshot from the reviewed service grant backfill report")
	flag.Parse()
	earlyLogger := logging.NewFromEnv(logging.ServiceMigrate)
	if boolCount(*identityPreflight, *serviceGrantReport, *serviceGrantApply, *schemaAction != "") > 1 {
		fatal(earlyLogger, "choose only one migration action", nil)
	}
	if *serviceGrantExpected != "" && !*serviceGrantApply {
		fatal(earlyLogger, "service grant expected SHA-256 requires apply mode", nil)
	}
	ctx := context.Background()
	if *schemaAction != "" {
		db, err := database.Connect(ctx, strings.TrimSpace(os.Getenv("DATABASE_URL")))
		if err != nil {
			fatal(earlyLogger, "database connection failed", err)
		}
		defer db.Close()
		switch *schemaAction {
		case "check", "apply":
			report, err := schemamaintenance.CheckSimplification(ctx, db)
			if err != nil {
				fatal(earlyLogger, "schema preflight failed", err)
			}
			if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
				fatal(earlyLogger, "schema report failed", err)
			}
			if !report.Ready {
				fatal(earlyLogger, "schema maintenance blocked", nil)
			}
			if *schemaAction == "apply" {
				if err := database.Migrate(ctx, db); err != nil {
					fatal(earlyLogger, "schema migration failed", err)
				}
				if err := schemamaintenance.VerifySimplification(ctx, db); err != nil {
					fatal(earlyLogger, "schema verification failed", err)
				}
			}
		case "verify":
			if err := schemamaintenance.VerifySimplification(ctx, db); err != nil {
				fatal(earlyLogger, "schema verification failed", err)
			}
		default:
			fatal(earlyLogger, "unknown schema-maintenance action", nil)
		}
		return
	}
	if *serviceGrantReport || *serviceGrantApply {
		// The backfill needs only database access, not API JWT or OAuth secrets.
		databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
		if databaseURL == "" {
			fatal(earlyLogger, "DATABASE_URL is required for service grant backfill", nil)
		}
		db, err := database.Connect(ctx, databaseURL)
		if err != nil {
			fatal(earlyLogger, "database connection failed", err)
		}
		defer db.Close()
		report, err := store.New(db).BackfillLegacyProductServiceGrants(ctx, *serviceGrantApply, *serviceGrantExpected)
		if err != nil {
			fatal(earlyLogger, "service grant backfill failed", err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			fatal(earlyLogger, "write service grant report failed", err)
		}
		if !report.Ready {
			logging.Sync(earlyLogger)
			os.Exit(1)
		}
		return
	}
	cfg, err := config.Load()
	if err != nil {
		fatal(earlyLogger, "load config failed", err)
	}
	logging.Sync(earlyLogger)
	logger, err := logging.New(logging.ServiceMigrate, cfg)
	if err != nil {
		fatal(earlyLogger, "create logger failed", err)
	}
	defer logging.Sync(logger)

	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		fatal(logger, "database connection failed", err)
	}
	defer db.Close()
	if *identityPreflight {
		report, err := database.PreflightIdentityCorrection(ctx, db)
		if err != nil {
			fatal(logger, "identity preflight failed", err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			fatal(logger, "write identity preflight report failed", err)
		}
		if !report.Ready {
			logging.Sync(logger)
			os.Exit(1)
		}
		return
	}

	if err := database.Migrate(ctx, db); err != nil {
		fatal(logger, "database migration failed", err)
	}
	logger.Info("migrations applied")
}

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

func fatal(logger *zap.Logger, message string, err error, fields ...zap.Field) {
	if err != nil {
		fields = append(fields, zap.Error(err))
	}
	logger.Error(message, fields...)
	logging.Sync(logger)
	os.Exit(1)
}
