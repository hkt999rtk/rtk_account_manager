package database

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenamedTestLabMigrationsPreserveHistory(t *testing.T) {
	canonical := []string{"071_test_lab_sessions.sql", "072_test_lab_bindings.sql", "073_test_lab_console_identity.sql"}
	for _, aliases := range [][]string{
		{"068_test_lab_sessions.sql", "069_test_lab_bindings.sql", "070_test_lab_console_identity.sql"},
		{"070_test_lab_sessions.sql", "071_test_lab_bindings.sql", "072_test_lab_console_identity.sql"},
	} {
		t.Run(aliases[0], func(t *testing.T) {
			db, full := newIdentityCaseDatabase(t)
			ctx := context.Background()
			old := t.TempDir()
			entries, err := os.ReadDir(full)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= "074" {
					continue
				}
				contents, err := os.ReadFile(filepath.Join(full, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				name := entry.Name()
				for i, current := range canonical {
					if name == current {
						name = aliases[i]
						break
					}
				}
				if err := os.WriteFile(filepath.Join(old, name), contents, 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("MIGRATIONS_DIR", old)
			if err := Migrate(ctx, db); err != nil {
				t.Fatal(err)
			}
			identityCaseExec(t, db, `UPDATE schema_migrations SET applied_at='2026-09-01T00:00:00Z' WHERE version=ANY($1)`, aliases)
			t.Setenv("MIGRATIONS_DIR", full)
			for repeat := 0; repeat < 2; repeat++ {
				if err := Migrate(ctx, db); err != nil {
					t.Fatal(err)
				}
			}
			for i, current := range canonical {
				var preserved bool
				if err := db.QueryRow(ctx, `SELECT a.applied_at=b.applied_at AND a.applied_at='2026-09-01T00:00:00Z'
					FROM schema_migrations a JOIN schema_migrations b ON b.version=$2 WHERE a.version=$1`, aliases[i], current).Scan(&preserved); err != nil || !preserved {
					t.Fatalf("history changed for %s: %t %v", current, preserved, err)
				}
			}
			var ready bool
			if err := db.QueryRow(ctx, `SELECT to_regclass('platform_bootstrap') IS NOT NULL AND EXISTS(SELECT 1 FROM schema_migrations WHERE version='077_admin_recovery.sql')
				AND (SELECT count(*) FROM roles WHERE name IN ('pki_admin','security_custodian','pki_auditor'))=3`).Scan(&ready); err != nil || !ready {
				t.Fatalf("PKI migration not reached: %t %v", ready, err)
			}
		})
	}
}

func TestRenamedMigrationRejectsUnprovenAliases(t *testing.T) {
	for _, scenario := range []string{"unrelated-marker", "edited-content"} {
		t.Run(scenario, func(t *testing.T) {
			db, full := newIdentityCaseDatabase(t)
			ctx := context.Background()
			if err := Migrate(ctx, db); err != nil {
				t.Fatal(err)
			}
			alias := "068_unrelated.sql"
			if scenario == "edited-content" {
				alias = "068_test_lab_sessions.sql"
				contents, err := os.ReadFile(filepath.Join(full, "071_test_lab_sessions.sql"))
				if err != nil {
					t.Fatal(err)
				}
				changed := t.TempDir()
				if err := os.WriteFile(filepath.Join(changed, "071_test_lab_sessions.sql"), append(contents, []byte("\n-- changed\n")...), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("MIGRATIONS_DIR", changed)
			}
			identityCaseExec(t, db, `UPDATE schema_migrations SET version=$1 WHERE version='071_test_lab_sessions.sql'`, alias)
			err := Migrate(ctx, db)
			if err == nil {
				t.Fatal("unproven alias accepted")
			}
			if scenario == "edited-content" && !strings.Contains(err.Error(), "content changed") {
				t.Fatal(err)
			}
			var unchanged bool
			if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)
				AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version='071_test_lab_sessions.sql')`, alias).Scan(&unchanged); err != nil || !unchanged {
				t.Fatalf("failed adoption modified history: %t %v", unchanged, err)
			}
		})
	}
}
