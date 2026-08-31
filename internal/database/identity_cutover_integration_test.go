package database

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestIdentityCutoverMigratesExistingPlatformAuthTokensIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}

	ctx := context.Background()
	admin, err := Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)

	databaseName := "identity_cutover_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+databaseName+`"`); err != nil {
		t.Fatal(err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = databaseName
	db, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		_, _ = admin.Exec(context.Background(), `DROP DATABASE "`+databaseName+`" WITH (FORCE)`)
	})
	if err := db.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	migrationsDir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	preCutoverDir := t.TempDir()
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= "049_unify_human_identity.sql" {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(migrationsDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(preCutoverDir, entry.Name()), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("MIGRATIONS_DIR", preCutoverDir)
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("apply pre-cutover migrations: %v", err)
	}

	var platformUserID, brandCloudID, legacyUserID string
	legacyHash := identityCaseHash(t, "fixture-legacy-password")
	if err := db.QueryRow(ctx, `
		INSERT INTO users (email,password_hash,email_verified,email_verified_at,signup_pending_verification)
		VALUES ('platform-owner@example.com','platform-hash',true,now(),false)
		RETURNING id::text
	`).Scan(&platformUserID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `
		INSERT INTO organizations (name,organization_kind,status,tenant_slug)
		VALUES ('Cutover Brand','brand_cloud','active','cutover-brand')
		RETURNING id::text
	`).Scan(&brandCloudID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `
		INSERT INTO brand_cloud_users (brand_cloud_id,email,password_hash,email_verified,email_verified_at,signup_pending_verification)
		VALUES ($1,'legacy-owner@example.com',$2,true,now(),false)
		RETURNING id::text
	`, brandCloudID, legacyHash).Scan(&legacyUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO brand_cloud_memberships (brand_cloud_id,brand_cloud_user_id,role) VALUES ($1,$2,'owner')`, brandCloudID, legacyUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO auth_tokens (user_id,subject_type,subject_id,purpose,scope,token_hash,expires_at)
		VALUES ($1,'platform_user',$1,'login_activation','','platform-token',now()+interval '1 hour')
	`, platformUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO brand_cloud_refresh_tokens (brand_cloud_id,brand_cloud_user_id,token_hash,expires_at)
		VALUES ($1,$2,'legacy-refresh',now()+interval '1 hour')
	`, brandCloudID, legacyUserID); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MIGRATIONS_DIR", migrationsDir)
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("apply identity cutover migration: %v", err)
	}

	var subjectType string
	if err := db.QueryRow(ctx, `SELECT subject_type FROM auth_tokens WHERE token_hash='platform-token'`).Scan(&subjectType); err != nil {
		t.Fatal(err)
	}
	if subjectType != "user" {
		t.Fatalf("platform auth token subject type = %q, want user", subjectType)
	}

	var mappingCount, ownerCount, activeLegacyRefresh int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM brand_cloud_user_migrations WHERE brand_cloud_user_id=$1`, legacyUserID).Scan(&mappingCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM organization_members WHERE organization_id=$1 AND role='owner' AND disabled_at IS NULL`, brandCloudID).Scan(&ownerCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM brand_cloud_refresh_tokens WHERE revoked_at IS NULL`).Scan(&activeLegacyRefresh); err != nil {
		t.Fatal(err)
	}
	if mappingCount != 1 || ownerCount != 1 || activeLegacyRefresh != 0 {
		t.Fatalf("cutover evidence mapping=%d owners=%d active_legacy_refresh=%d", mappingCount, ownerCount, activeLegacyRefresh)
	}

	var appliedAt time.Time
	if err := db.QueryRow(ctx, `SELECT applied_at FROM schema_migrations WHERE version='049_unify_human_identity.sql'`).Scan(&appliedAt); err != nil {
		t.Fatal(err)
	}
	// A subsequent startup skips published SQL, never deletes/recreates its marker.
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var sameMarker bool
	if err := db.QueryRow(ctx, `SELECT applied_at=$1 FROM schema_migrations WHERE version='049_unify_human_identity.sql'`, appliedAt).Scan(&sameMarker); err != nil || !sameMarker {
		t.Fatalf("published migration marker changed: same=%t err=%v", sameMarker, err)
	}

	var migrationResult string
	if err := db.QueryRow(ctx, `SELECT result FROM brand_cloud_user_migrations WHERE brand_cloud_user_id=$1`, legacyUserID).Scan(&migrationResult); err != nil {
		t.Fatal(err)
	}
	if migrationResult != "created_user" {
		t.Fatalf("idempotent rerun changed migration result to %q", migrationResult)
	}
}
