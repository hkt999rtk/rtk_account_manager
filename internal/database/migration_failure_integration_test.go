package database

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIdentityPreflightRejectsUnavailableInputsWithoutClaimingReady(t *testing.T) {
	for _, scenario := range []string{"missing directory", "missing SQL", "canceled connection", "missing marker table", "missing token table"} {
		t.Run(scenario, func(t *testing.T) {
			db, full := newIdentityCaseDatabase(t)
			ctx := context.Background()
			wantRollback := false
			switch scenario {
			case "missing directory":
				t.Chdir(t.TempDir())
				t.Setenv("MIGRATIONS_DIR", filepath.Join(t.TempDir(), "absent"))
			case "missing SQL":
				t.Setenv("MIGRATIONS_DIR", t.TempDir())
			case "canceled connection":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			case "missing marker table":
				identityCaseExec(t, db, `ALTER TABLE schema_migrations RENAME TO preflight_hidden_markers`)
				wantRollback = true
			case "missing token table":
				applyPublishedIdentityCutover(t, db, full)
				identityCaseExec(t, db, `ALTER TABLE refresh_tokens RENAME TO preflight_hidden_tokens`)
				wantRollback = true
			}
			report, err := PreflightIdentityCorrection(ctx, db)
			if err == nil || report.Ready || report.RolledBack != wantRollback {
				t.Fatalf("unsafe preflight result: %+v, %v", report, err)
			}
			if scenario == "canceled connection" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation was lost: %v", err)
			}
			var unchanged bool
			if err := db.QueryRow(context.Background(), `SELECT to_regclass('organization_member_activation_holds') IS NULL`).Scan(&unchanged); err != nil || !unchanged {
				t.Fatalf("failed preflight created correction state: %t, %v", unchanged, err)
			}
		})
	}
}

func TestMigrationMarkerAndCommitFailuresRollBackDDL(t *testing.T) {
	for _, scenario := range []string{"marker rejection", "deferred commit rejection"} {
		t.Run(scenario, func(t *testing.T) {
			db, _ := newIdentityCaseDatabase(t)
			ctx := context.Background()
			fixture := t.TempDir()
			t.Setenv("MIGRATIONS_DIR", fixture)
			const migration = "900_synthetic_failure.sql"
			sql := `CREATE TABLE migration_failure_probe(id INT); INSERT INTO migration_failure_probe VALUES(1);`
			if scenario == "marker rejection" {
				identityCaseExec(t, db, `ALTER TABLE schema_migrations ADD CONSTRAINT reject_probe_marker CHECK(version<>'900_synthetic_failure.sql') NOT VALID`)
			} else {
				sql = `CREATE TABLE migration_failure_probe(id INT UNIQUE DEFERRABLE INITIALLY DEFERRED); INSERT INTO migration_failure_probe VALUES(1),(1);`
			}
			if err := os.WriteFile(filepath.Join(fixture, migration), []byte(sql), 0600); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(ctx, db); err == nil {
				t.Fatal("failed migration was accepted")
			}
			var rolledBack bool
			if err := db.QueryRow(ctx, `SELECT to_regclass('migration_failure_probe') IS NULL AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, migration).Scan(&rolledBack); err != nil || !rolledBack {
				t.Fatalf("migration left DDL or marker: %t, %v", rolledBack, err)
			}
			// Repair only this synthetic fixture and retry; published migration
			// files and preexisting applied markers are never changed.
			if scenario == "marker rejection" {
				identityCaseExec(t, db, `ALTER TABLE schema_migrations DROP CONSTRAINT reject_probe_marker`)
			} else if err := os.WriteFile(filepath.Join(fixture, migration), []byte(strings.Replace(sql, "VALUES(1),(1)", "VALUES(1),(2)", 1)), 0600); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(ctx, db); err != nil {
				t.Fatalf("safe retry failed: %v", err)
			}
			var applied bool
			if err := db.QueryRow(ctx, `SELECT to_regclass('migration_failure_probe') IS NOT NULL AND EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, migration).Scan(&applied); err != nil || !applied {
				t.Fatalf("successful retry missing DDL/marker: %t, %v", applied, err)
			}
		})
	}
}

func TestMigrationRejectsCanceledDatabaseAndMissingDirectory(t *testing.T) {
	db, _ := newIdentityCaseDatabase(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Migrate(ctx, db); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled migration: %v", err)
	}
	t.Chdir(t.TempDir())
	t.Setenv("MIGRATIONS_DIR", "")
	if err := Migrate(context.Background(), db); err == nil || !strings.Contains(err.Error(), "migrations directory not found") {
		t.Fatalf("missing migration directory: %v", err)
	}
}
