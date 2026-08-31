package database

import (
	"context"
	"testing"
)

func TestHandoffDatabaseTransitionCannotSkipPreparationOrReleaseEvidence(t *testing.T) {
	db := newMultiCloudDatabase(t, false)
	ctx := context.Background()
	source, target := multiCloudUser(t, db), multiCloudUser(t, db)
	cloud := multiCloudCreate(t, db, source)
	var transfer string
	if err := db.QueryRow(ctx, `INSERT INTO brand_cloud_owner_transfers
		(brand_cloud_id,requested_by_user_id,target_user_id,token_hash,status,expires_at,ownership_version)
		VALUES($1,$2,$3,'migration-test-token','pending',now()+interval '1 hour',1) RETURNING id::text`, cloud, source, target).Scan(&transfer); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO cloud_ownership_handoffs
		(id,brand_cloud_id,source_user_id,target_user_id,ownership_version,cutoff,acceptance_eligibility,phase,version)
		VALUES($1,$2,$3,$4,1,now(),'{}',$5,$6)`
	for _, tc := range []struct {
		phase   string
		version int64
	}{{"canceling", 1}, {"canceled", 1}, {"preparing", 2}} {
		_, err := db.Exec(ctx, insert, transfer, cloud, source, target, tc.phase, tc.version)
		requirePGState(t, err, "23514")
	}
	if _, err := db.Exec(ctx, insert, transfer, cloud, source, target, "preparing", 1); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(ctx, `UPDATE cloud_ownership_handoffs SET phase='canceled',version=version+1 WHERE id=$1`, transfer)
	requirePGState(t, err, "23514")
	if _, err := db.Exec(ctx, `UPDATE cloud_ownership_handoffs SET phase='canceling',version=version+1 WHERE id=$1`, transfer); err != nil {
		t.Fatal(err)
	}
	// Empty inventory is not "all participants released". Nor may a missing
	// producer be replaced with Billing alone, even with its release receipt.
	assertCannotRelease := func() {
		t.Helper()
		_, err := db.Exec(ctx, `UPDATE cloud_ownership_handoffs SET phase='canceled',version=version+1 WHERE id=$1`, transfer)
		requirePGState(t, err, "23514")
	}
	assertCannotRelease()
	if _, err := db.Exec(ctx, `INSERT INTO cloud_handoff_participants(operation_id,participant) VALUES($1,'billing')`, transfer); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO cloud_handoff_abort_acknowledgments(operation_id,participant,receipt_sha256) VALUES($1,'billing',repeat('a',64))`, transfer); err != nil {
		t.Fatal(err)
	}
	assertCannotRelease()
	if _, err := db.Exec(ctx, `INSERT INTO cloud_handoff_participants(operation_id,participant) VALUES($1,'test_resources')`, transfer); err != nil {
		t.Fatal(err)
	}
	assertCannotRelease()
	if _, err := db.Exec(ctx, `INSERT INTO cloud_handoff_abort_acknowledgments(operation_id,participant,receipt_sha256) VALUES($1,'test_resources',repeat('b',64))`, transfer); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE cloud_ownership_handoffs SET phase='canceled',version=version+1 WHERE id=$1`, transfer); err != nil {
		t.Fatal(err)
	}
}
