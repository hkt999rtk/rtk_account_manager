package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestBillingCreationMigrationDoesNotInferHistoricalResponsibility(t *testing.T) {
	db := newMultiCloudDatabaseBefore(t, "066_brand_cloud_billing_creation_outbox.sql")
	ctx := context.Background()
	owner, next := multiCloudUser(t, db), multiCloudUser(t, db)
	existing := multiCloudCreate(t, db, owner)
	dir, err := filepath.Abs("../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIGRATIONS_DIR", dir)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM brand_cloud_billing_creation_outbox`).Scan(&count); err != nil || count != 0 {
		t.Fatal("migration inferred historical cloud owner", count, err)
	}
	// A post-migration owner change must not masquerade as cloud creation.
	if _, err := db.Exec(ctx, `UPDATE organization_members SET user_id=$2 WHERE organization_id=$1 AND role='owner'`, existing, next); err != nil {
		t.Fatal(err)
	}
	created := multiCloudCreate(t, db, owner)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal("marker replay", err)
	}
	var cloud, initialOwner string
	if err := db.QueryRow(ctx, `SELECT cloud_id::text,owner_user_id::text FROM brand_cloud_billing_creation_outbox`).Scan(&cloud, &initialOwner); err != nil || cloud != created || initialOwner != owner {
		t.Fatal("new cloud did not retain its initial owner", cloud, initialOwner, err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM brand_cloud_billing_creation_outbox`).Scan(&count); err != nil || count != 1 {
		t.Fatal("owner mutation or migration replay created an extra event", count, err)
	}
}
