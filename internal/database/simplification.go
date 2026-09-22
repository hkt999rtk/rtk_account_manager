package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const SimplificationVersion = "084_retire_tenant_identity.sql"

var simplificationMigrations = []string{"083_consolidate_audit_storage.sql", SimplificationVersion}
var retiredIdentityTables = []string{"brand_cloud_users", "brand_cloud_memberships", "brand_cloud_refresh_tokens", "acl_audit_events"}

type SimplificationReport struct {
	SourceVersion string           `json:"source_version"`
	TargetVersion string           `json:"target_version"`
	Rows          map[string]int64 `json:"rows"`
	Ready         bool             `json:"ready"`
	Reason        string           `json:"reason,omitempty"`
}

// CheckSimplification runs the exact cleanup SQL in a transaction that is
// always rolled back. Run against a restored or write-frozen database.
func CheckSimplification(ctx context.Context, db *pgxpool.Pool) (report SimplificationReport, err error) {
	report = SimplificationReport{TargetVersion: SimplificationVersion, Rows: map[string]int64{}}
	tx, err := db.Begin(ctx)
	if err != nil {
		return report, err
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if e := tx.Rollback(rollbackCtx); e != nil && err == nil {
			err = e
			report.Ready = false
		}
	}()
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout='5s'; SELECT pg_advisory_xact_lock(hashtextextended('account-manager-schema',0))`); err != nil {
		return report, err
	}
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(version),'') FROM schema_migrations`).Scan(&report.SourceVersion); err != nil {
		return report, err
	}
	if report.SourceVersion > SimplificationVersion {
		report.Reason = "database schema is newer than this binary"
		return report, nil
	}
	if report.SourceVersion < "082_optional_service_default_suspended.sql" {
		report.Reason = "apply historical migrations through 082 before cleanup"
		return report, nil
	}
	for _, table := range retiredIdentityTables {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			return report, err
		}
		if exists {
			var count int64
			if err = tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
				return report, err
			}
			report.Rows[table] = count
		}
	}
	dir, err := findMigrationDir()
	if err != nil {
		return report, err
	}
	for _, name := range simplificationMigrations {
		var applied bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, name).Scan(&applied); err != nil {
			return report, err
		}
		if applied {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			return report, readErr
		}
		if _, execErr := tx.Exec(ctx, string(data)); execErr != nil {
			report.Reason = fmt.Sprintf("%s: %v", name, execErr)
			return report, nil
		}
	}
	if _, execErr := tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); execErr != nil {
		report.Reason = execErr.Error()
		return report, nil
	}
	report.Ready = true
	return report, nil
}

func VerifySimplification(ctx context.Context, db *pgxpool.Pool) error {
	var version string
	if err := db.QueryRow(ctx, `SELECT COALESCE(max(version),'') FROM schema_migrations`).Scan(&version); err != nil {
		return err
	}
	if version != SimplificationVersion {
		return fmt.Errorf("schema incompatible: source=%s required=%s; run offline migrations", version, SimplificationVersion)
	}
	for _, table := range retiredIdentityTables {
		var exists bool
		if err := db.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("retired table still exists: %s", table)
		}
	}
	var valid bool
	if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='audit_events' AND column_name='audit_domain')
 AND to_regclass('identity_providers_provider_id_idx') IS NULL AND to_regclass('device_tag_catalog_org_tag_idx') IS NULL`).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("audit/index schema verification failed")
	}
	return nil
}
