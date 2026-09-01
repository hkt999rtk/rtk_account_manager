package database

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPreflightIdentityCorrectionRejectsNilPool(t *testing.T) {
	report, err := PreflightIdentityCorrection(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "database pool") {
		t.Fatalf("nil pool: report=%+v err=%v", report, err)
	}
	if report.Migration != identityCorrectionMigration || report.Ready || report.RolledBack {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestPreflightIdentityCorrectionRequiresMigrationDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MIGRATIONS_DIR", filepath.Join(t.TempDir(), "missing"))
	report, err := PreflightIdentityCorrection(context.Background(), &pgxpool.Pool{})
	if err == nil || !strings.Contains(err.Error(), "migrations directory not found") {
		t.Fatalf("missing migrations: report=%+v err=%v", report, err)
	}
	if report.Migration != identityCorrectionMigration || report.Ready || report.RolledBack {
		t.Fatalf("unexpected report: %+v", report)
	}
}
