package testutil

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const integrationDBLockKey int64 = 104729

// LockIntegrationDatabase serializes tests that share TEST_DATABASE_URL so
// package-level go test parallelism does not make them clobber each other's
// fixtures and truncation.
func LockIntegrationDatabase(t *testing.T, db *pgxpool.Pool) {
	t.Helper()

	ctx := context.Background()
	conn, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, integrationDBLockKey); err != nil {
		conn.Release()
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, integrationDBLockKey); err != nil {
			t.Errorf("unlock integration database: %v", err)
		}
		conn.Release()
	})
}
