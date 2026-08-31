package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const multiCloudMigration = "052_multicloud_owner_invariant.sql"

// Each case owns a newly created database, never the caller's configured DB.
func newMultiCloudDatabase(t *testing.T, beforeFoundation bool) *pgxpool.Pool {
	t.Helper()
	before := ""
	if beforeFoundation {
		before = multiCloudMigration
	}
	return newMultiCloudDatabaseBefore(t, before)
}

func newMultiCloudDatabaseBefore(t *testing.T, before string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	admin, err := Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("multicloud_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
	})
	dir, err := filepath.Abs("../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if before != "" {
		staged := t.TempDir()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= before {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(staged, entry.Name()), b, 0600); err != nil {
				t.Fatal(err)
			}
		}
		dir = staged
	}
	t.Setenv("MIGRATIONS_DIR", dir)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

func multiCloudUser(t *testing.T, db *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := db.QueryRow(context.Background(), `INSERT INTO users(email,password_hash,email_verified)
        VALUES (gen_random_uuid()::text || '@test.invalid','test-hash',true) RETURNING id::text`).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func multiCloudCreate(t *testing.T, db *pgxpool.Pool, owner string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO organizations(name,organization_kind,tenant_slug) VALUES ('test cloud','brand_cloud',gen_random_uuid()::text) RETURNING id::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES ($1,$2,'owner')`, id, owner); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return id
}

func requirePGState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("error = %v, want SQLSTATE %s", err, code)
	}
}

func TestMultiCloudSoleOwnerIntegration(t *testing.T) {
	db := newMultiCloudDatabase(t, false)
	ctx := context.Background()
	owner, next := multiCloudUser(t, db), multiCloudUser(t, db)
	cloud := multiCloudCreate(t, db, owner)
	// Multiple owned clouds are valid and ownership is membership-derived.
	other := multiCloudCreate(t, db, owner)
	if cloud == other {
		t.Fatal("cloud UUID reused")
	}
	_, err := db.Exec(ctx, `INSERT INTO organizations(name,organization_kind,tenant_slug) VALUES ('orphan','brand_cloud',gen_random_uuid()::text)`)
	requirePGState(t, err, "23514")
	_, err = db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES ($1,$2,'owner')`, cloud, next)
	requirePGState(t, err, "23514")
	_, err = db.Exec(ctx, `DELETE FROM organization_members WHERE organization_id=$1 AND user_id=$2`, cloud, owner)
	requirePGState(t, err, "23514")
	// Disabled or unverified owners remain designated; eligibility is a separate gate.
	if _, err := db.Exec(ctx, `UPDATE users SET disabled_at=now(),email_verified=false,signup_pending_verification=true WHERE id=$1`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE organization_members SET disabled_at=now() WHERE organization_id=$1`, cloud); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM organization_members WHERE organization_id=$1 AND role='owner'`, cloud).Scan(&count); err != nil || count != 1 {
		t.Fatalf("designated owners=%d err=%v", count, err)
	}
	// Zero-owner intermediate state is permitted only within an atomic replacement.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM organization_members WHERE organization_id=$1`, cloud); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES ($1,$2,'owner')`, cloud, next); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Moving the only owner must validate both the old and new cloud.
	_, err = db.Exec(ctx, `UPDATE organization_members SET organization_id=$1 WHERE organization_id=$2 AND user_id=$3`, other, cloud, next)
	requirePGState(t, err, "23514")
	// Tombstoning now additionally requires the 061 closure protocol. A direct
	// flag update cannot evade sole-owner preservation or Billing clearance.
	_, err = db.Exec(ctx, `UPDATE organizations SET deleted_at=now() WHERE id=$1`, cloud)
	requirePGState(t, err, "23514")
	_, err = db.Exec(ctx, `DELETE FROM organization_members WHERE organization_id=$1`, cloud)
	requirePGState(t, err, "23514")
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migration marker replay: %v", err)
	}
}

func TestMultiCloudLegacyCustomerOwnerIntegration(t *testing.T) {
	db := newMultiCloudDatabase(t, false)
	ctx := context.Background()
	u1, u2 := multiCloudUser(t, db), multiCloudUser(t, db)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var org string
	if err := tx.QueryRow(ctx, `INSERT INTO organizations(name) VALUES ('legacy customer') RETURNING id::text`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES ($1,$2,'owner'),($1,$3,'owner')`, org, u1, u2); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `UPDATE organizations SET organization_kind='brand_cloud',tenant_slug=gen_random_uuid()::text WHERE id=$1`, org)
	requirePGState(t, err, "23514")
	if _, err := db.Exec(ctx, `DELETE FROM organization_members WHERE organization_id=$1 AND user_id=$2`, org, u2); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `DELETE FROM organization_members WHERE organization_id=$1 AND user_id=$2`, org, u1)
	requirePGState(t, err, "23514")
}

func TestMultiCloudOwnerWriteSerializationIntegration(t *testing.T) {
	db := newMultiCloudDatabase(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner, next, competitor := multiCloudUser(t, db), multiCloudUser(t, db), multiCloudUser(t, db)
	cloud := multiCloudCreate(t, db, owner)
	stale, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Rollback(context.Background())
	if _, err := stale.Exec(ctx, `SELECT * FROM organization_members WHERE organization_id=$1`, cloud); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'owner')`, cloud, next); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM organization_members WHERE organization_id=$1 AND user_id=$2`, cloud, owner); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	pid := conn.Conn().PgConn().PID()
	result := make(chan error, 1)
	go func() {
		_, err := conn.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'owner')`, cloud, competitor)
		result <- err
	}()
	// Observe the actual waiting backend, rather than relying on a sleep/race.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := db.QueryRow(ctx, `SELECT COALESCE(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("competing writer did not wait: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	requirePGState(t, <-result, "23514")
	_, err = stale.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'owner')`, cloud, competitor)
	requirePGState(t, err, "40001")
}

func TestMultiCloudPreflightRejectsAmbiguousOwnersIntegration(t *testing.T) {
	for _, owners := range []int{0, 2} {
		t.Run(fmt.Sprint(owners), func(t *testing.T) {
			db := newMultiCloudDatabase(t, true)
			ctx := context.Background()
			var cloud string
			if err := db.QueryRow(ctx, `INSERT INTO organizations(name,organization_kind,tenant_slug) VALUES ('unresolved','brand_cloud',gen_random_uuid()::text) RETURNING id::text`).Scan(&cloud); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < owners; i++ {
				user := multiCloudUser(t, db)
				if _, err := db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'owner')`, cloud, user); err != nil {
					t.Fatal(err)
				}
			}
			dir, err := filepath.Abs("../../migrations")
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("MIGRATIONS_DIR", dir)
			err = Migrate(ctx, db)
			requirePGState(t, err, "23514")
			var applied bool
			if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, multiCloudMigration).Scan(&applied); err != nil || applied {
				t.Fatalf("failed migration marked applied=%v err=%v", applied, err)
			}
		})
	}
}
