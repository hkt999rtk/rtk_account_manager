package schemamaintenance

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrationDirectoryPrefersExplicitExistingDirectory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.Mkdir("migrations", 0700); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(root, "reviewed")
	if err := os.Mkdir(explicit, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIGRATIONS_DIR", explicit)
	if got, err := findMigrationDir(); err != nil || got != explicit {
		t.Fatalf("migration directory = %q, %v; want %q", got, err, explicit)
	}
	t.Setenv("MIGRATIONS_DIR", filepath.Join(root, "absent"))
	if got, err := findMigrationDir(); err != nil || got != "migrations" {
		t.Fatalf("fallback directory = %q, %v; want migrations", got, err)
	}
}
