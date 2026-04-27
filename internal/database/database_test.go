package database

import "testing"

func TestFindMigrationDirMissing(t *testing.T) {
	t.Chdir(t.TempDir())

	if _, err := findMigrationDir(); err == nil {
		t.Fatal("expected missing migrations directory error")
	}
}
