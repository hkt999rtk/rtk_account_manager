package database

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
