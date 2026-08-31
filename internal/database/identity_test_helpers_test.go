package database

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// Each case owns a new database. Never migrate or truncate the database named
// by TEST_DATABASE_URL: it is only the connection used to create our fixture.
func newIdentityCaseDatabase(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	connection := os.Getenv("TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	admin, err := Connect(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := "identity_case_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatal(err)
	}
	var db *pgxpool.Pool
	t.Cleanup(func() {
		if db != nil {
			db.Close()
		}
		if _, err := admin.Exec(context.Background(), `DROP DATABASE "`+name+`" WITH (FORCE)`); err != nil {
			t.Errorf("remove isolated migration database: %v", err)
		}
	})
	config, err := pgxpool.ParseConfig(connection)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = name
	db, err = pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	full, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		t.Fatal(err)
	}
	before := t.TempDir()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= "049_unify_human_identity.sql" {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(full, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(before, entry.Name()), contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MIGRATIONS_DIR", before)
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("prepare pre-cutover schema: %v", err)
	}
	t.Setenv("MIGRATIONS_DIR", full)
	return db, full
}

func identityCaseID(t *testing.T, db *pgxpool.Pool, query string, args ...any) string {
	t.Helper()
	var id string
	if err := db.QueryRow(context.Background(), query, args...).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func identityCaseExec(t *testing.T, db *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func identityCaseUser(t *testing.T, db *pgxpool.Pool, email, hash string, verified bool) string {
	t.Helper()
	return identityCaseID(t, db, `INSERT INTO users(email,password_hash,email_verified,email_verified_at,signup_pending_verification)
		VALUES($1,$2,$3,CASE WHEN $3 THEN now() ELSE NULL END,NOT $3) RETURNING id::text`, email, hash, verified)
}

func identityCaseBrand(t *testing.T, db *pgxpool.Pool, name, owner string) string {
	t.Helper()
	id := identityCaseID(t, db, `INSERT INTO organizations(name,organization_kind,status,tenant_slug)
		VALUES($1,'brand_cloud','active',$1) RETURNING id::text`, name)
	if owner != "" {
		identityCaseExec(t, db, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'owner')`, id, owner)
	}
	return id
}

func identityCaseLegacy(t *testing.T, db *pgxpool.Pool, brand, email, hash, role string, verified bool) string {
	t.Helper()
	id := identityCaseID(t, db, `INSERT INTO brand_cloud_users(brand_cloud_id,email,password_hash,email_verified,email_verified_at,signup_pending_verification)
		VALUES($1,$2,$3,$4,CASE WHEN $4 THEN now() ELSE NULL END,NOT $4) RETURNING id::text`, brand, email, hash, verified)
	identityCaseExec(t, db, `INSERT INTO brand_cloud_memberships(brand_cloud_id,brand_cloud_user_id,role) VALUES($1,$2,$3)`, brand, id, role)
	return id
}

func identityCaseHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(hash)
}
