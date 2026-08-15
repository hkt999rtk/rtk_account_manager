package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestConnectRejectsInvalidConfig(t *testing.T) {
	if _, err := Connect(context.Background(), "://invalid"); err == nil {
		t.Fatal("expected invalid database URL to fail")
	}
}

func TestConnectRejectsUnreachableDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if pool, err := Connect(ctx, "postgres://coverage:coverage@127.0.0.1:1/coverage?sslmode=disable&connect_timeout=1"); err == nil {
		pool.Close()
		t.Fatal("expected unreachable database to fail ping")
	}
}

func TestConnectAppliesPoolTuningIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	t.Setenv("DATABASE_MAX_CONNS", "7")
	t.Setenv("DATABASE_MIN_CONNS", "2")
	t.Setenv("DATABASE_MAX_CONN_LIFETIME", "11m")
	t.Setenv("DATABASE_MAX_CONN_IDLE_TIME", "90s")

	pool, err := Connect(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer pool.Close()

	config := pool.Config()
	if config.MaxConns != 7 || config.MinConns != 2 {
		t.Fatalf("pool connection bounds = %d/%d, want 7/2", config.MaxConns, config.MinConns)
	}
	if config.MaxConnLifetime != 11*time.Minute {
		t.Fatalf("MaxConnLifetime = %v, want 11m", config.MaxConnLifetime)
	}
	if config.MaxConnIdleTime != 90*time.Second {
		t.Fatalf("MaxConnIdleTime = %v, want 90s", config.MaxConnIdleTime)
	}
}

func TestFindMigrationDirMissing(t *testing.T) {
	t.Chdir(t.TempDir())

	if _, err := findMigrationDir(); err == nil {
		t.Fatal("expected missing migrations directory error")
	}
}

func TestFindMigrationDirHonorsEnvOverride(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	t.Setenv("MIGRATIONS_DIR", dir)

	got, err := findMigrationDir()
	if err != nil {
		t.Fatalf("expected env override to succeed, got %v", err)
	}
	if got != dir {
		t.Fatalf("expected %s, got %s", dir, got)
	}
}

func TestEnvIntFallback(t *testing.T) {
	if v := envInt("DATABASE_TEST_MISSING_INT", 7); v != 7 {
		t.Fatalf("expected fallback 7, got %d", v)
	}
	t.Setenv("DATABASE_TEST_INT", "invalid")
	if v := envInt("DATABASE_TEST_INT", 9); v != 9 {
		t.Fatalf("expected fallback for invalid value, got %d", v)
	}
	t.Setenv("DATABASE_TEST_INT", "42")
	if v := envInt("DATABASE_TEST_INT", 9); v != 42 {
		t.Fatalf("expected 42, got %d", v)
	}
}

func TestEnvDurationFallback(t *testing.T) {
	if v := envDuration("DATABASE_TEST_MISSING_DUR", 3*time.Second); v != 3*time.Second {
		t.Fatalf("expected fallback, got %v", v)
	}
	t.Setenv("DATABASE_TEST_DUR", "not-a-duration")
	if v := envDuration("DATABASE_TEST_DUR", 5*time.Second); v != 5*time.Second {
		t.Fatalf("expected fallback for invalid value, got %v", v)
	}
	t.Setenv("DATABASE_TEST_DUR", "10s")
	if v := envDuration("DATABASE_TEST_DUR", 5*time.Second); v != 10*time.Second {
		t.Fatalf("expected 10s, got %v", v)
	}
}

func TestFindMigrationDirCandidates(t *testing.T) {
	tmp := t.TempDir()
	migrationsDir := filepath.Join(tmp, "migrations")
	if err := os.Mkdir(migrationsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(tmp)

	got, err := findMigrationDir()
	if err != nil {
		t.Fatalf("expected candidate lookup to succeed, got %v", err)
	}
	if got != "migrations" {
		t.Fatalf("expected migrations, got %s", got)
	}
}

func TestMigrateAppliesAndThenSkipsCommittedFiles(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	admin, err := Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()

	schema := fmt.Sprintf("migration_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	migrations := t.TempDir()
	if err := os.WriteFile(filepath.Join(migrations, "001_create.sql"), []byte("CREATE TABLE migration_probe (id BIGINT PRIMARY KEY);"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migrations, "002_insert.sql"), []byte("INSERT INTO migration_probe (id) VALUES (1);"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migrations, "README.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIGRATIONS_DIR", migrations)

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("first migration: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("idempotent migration: %v", err)
	}
	var versions, rows int
	if err := pool.QueryRow(ctx, "SELECT count(*)::int FROM schema_migrations").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*)::int FROM migration_probe").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if versions != 2 || rows != 1 {
		t.Fatalf("versions/rows = %d/%d, want 2/1", versions, rows)
	}
}
