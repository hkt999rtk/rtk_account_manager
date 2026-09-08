package database

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Test Lab migrations shipped with earlier numbers before unrelated migrations
// occupied those numbers. Only these exact historical filenames are aliases;
// matching a numeric prefix or finding an existing table is never sufficient.
// Keep the original markers and timestamp, and do not repeat session revocations.
func adoptRenamedTestLabMigration(ctx context.Context, tx pgx.Tx, name string, contents []byte) (bool, error) {
	var aliases []string
	var digest string
	switch name {
	case "071_test_lab_sessions.sql":
		aliases = []string{"068_test_lab_sessions.sql", "070_test_lab_sessions.sql"}
		digest = "c6e8e4250017cbb18d375aa665bc2c5a257280af29d6da567e252544a018731a"
	case "072_test_lab_bindings.sql":
		aliases = []string{"069_test_lab_bindings.sql", "071_test_lab_bindings.sql"}
		digest = "b38157b4bcf97a425ccaa0c82963bca5e368bb7baa14d772c057cb00752f6b45"
	case "073_test_lab_console_identity.sql":
		aliases = []string{"070_test_lab_console_identity.sql", "072_test_lab_console_identity.sql"}
		digest = "c66985c480dafb1bb64a51a685104a6e1bd261d7816f95ec316649ff371601af"
	default:
		return false, nil
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=ANY($1))`, aliases).Scan(&exists); err != nil || !exists {
		return false, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(contents)) != digest {
		return false, fmt.Errorf("renamed migration %s content changed; explicit reconciliation required", name)
	}
	_, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version,applied_at)
		SELECT $1,min(applied_at) FROM schema_migrations WHERE version=ANY($2)`, name, aliases)
	return err == nil, err
}
