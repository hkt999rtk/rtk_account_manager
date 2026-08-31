package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"rtk_account_manager/internal/billinghandoff"
)

// Synthetic durable Billing/producer recovery state. Real HTTP/store integration
// tests independently verify the cross-service serialization and transactions.
type deletionRecoveryFixture struct {
	*deletionCoordinatorFixture
	lock                                                          sync.Mutex
	retired                                                       map[string]billinghandoff.CloseCommandResolution
	rejectOnce, lostRetirement, billingReleased, producerReleased bool
	producerCanceled                                              map[string]bool
	receipt                                                       string
}

func newDeletionRecoveryFixture() *deletionRecoveryFixture {
	return &deletionRecoveryFixture{deletionCoordinatorFixture: newDeletionCoordinatorFixture(), retired: map[string]billinghandoff.CloseCommandResolution{}, producerCanceled: map[string]bool{}}
}
func (f *deletionRecoveryFixture) CloudClosureStatus(ctx context.Context, in billinghandoff.ClosureBinding) (billinghandoff.ClosureStatus, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	out, err := f.deletionCoordinatorFixture.CloudClosureStatus(ctx, in)
	if f.receipt != "" {
		out.ReceiptID = f.receipt
	}
	return out, err
}
func (f *deletionRecoveryFixture) CloseCloud(ctx context.Context, in billinghandoff.ClosureBinding, receipt, sha string) (billinghandoff.ClosureAcknowledgment, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	if _, ok := f.retired[sha]; ok {
		return billinghandoff.ClosureAcknowledgment{}, &billinghandoff.HTTPError{Status: 409, Code: "BILLING_CLOSURE_COMMAND_RETIRED"}
	}
	if f.rejectOnce {
		f.rejectOnce = false
		return billinghandoff.ClosureAcknowledgment{}, &billinghandoff.HTTPError{Status: 409, Code: "BILLING_CLOSURE_NOT_READY"}
	}
	return f.deletionCoordinatorFixture.CloseCloud(ctx, in, receipt, sha)
}
func (f *deletionRecoveryFixture) RetireCloudClose(_ context.Context, in billinghandoff.ClosureBinding, receipt, sha string) (billinghandoff.CloseCommandResolution, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	if out, ok := f.retired[sha]; ok {
		return out, nil
	}
	out := billinghandoff.CloseCommandResolution{OperationID: in.OperationID, SettlementID: receipt, AMReadinessSHA256: sha}
	f.mu.Lock()
	ack, closed := f.ack[in.OperationID]
	f.mu.Unlock()
	if closed {
		out.Outcome = "closed"
		out.Acknowledgment = &ack
		return out, nil
	}
	now := time.Now()
	out.Outcome = "retired"
	out.RetiredAt = &now
	out.ReceiptSHA256 = strings.Repeat("d", 64)
	f.retired[sha] = out
	if f.lostRetirement {
		f.lostRetirement = false
		return billinghandoff.CloseCommandResolution{}, errors.New("retirement reply lost")
	}
	return out, nil
}
func (f *deletionRecoveryFixture) CancelCloudClosure(_ context.Context, in billinghandoff.ClosureBinding, id, sha string) (billinghandoff.ClosureOperation, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	phase := "canceling"
	if f.billingReleased {
		phase = "canceled"
	}
	return billinghandoff.ClosureOperation{ID: in.OperationID, OwnerUserID: in.OwnerUserID, OwnershipVersion: in.OwnershipVersion, Phase: phase}, nil
}
func (f *deletionRecoveryFixture) CancelCloudDeletion(_ context.Context, in billinghandoff.ClosureBinding, version int64, id, sha string) (CloudDeletionRelease, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	if !f.producerReleased {
		return CloudDeletionRelease{}, ErrHandoffUnavailable
	}
	f.producerCanceled[in.OperationID] = true
	return CloudDeletionRelease{Binding: in, CancellationID: id, Participant: "test_resources", Released: true, ReceiptSHA256: strings.Repeat("e", 64)}, nil
}
func (f *deletionRecoveryFixture) PrepareCloudDeletion(ctx context.Context, in billinghandoff.ClosureBinding, version int64) (CloudDeletionHold, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	if f.producerCanceled[in.OperationID] {
		return CloudDeletionHold{}, ErrConflict
	}
	return f.deletionCoordinatorFixture.PrepareCloudDeletion(ctx, in, version)
}
func configureDeletionRecovery(t *testing.T, s *Store, f *deletionRecoveryFixture) {
	t.Helper()
	if err := s.ConfigureCloudDeletionPreflight(CloudDeletionPreflightOptions{Billing: deletionBillingFunc(syntheticDeletionBilling), Resources: map[string]CloudDeletionResourceObserver{"test_resources": deletionResourceFunc(syntheticDeletionResources)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureCloudDeletion(CloudDeletionOptions{Billing: f, Producers: map[string]CloudDeletionProducer{"test_resources": f}}); err != nil {
		t.Fatal(err)
	}
}

func TestCloudDeletionRecoversRetiredStaleCommand(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "delete-retired")
	f := newDeletionRecoveryFixture()
	f.rejectOnce = true
	f.lostRetirement = true
	configureDeletionRecovery(t, env.store, f)
	op, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID, "retire")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.store.AdvanceCloudDeletion(ctx, op.CloudID, op.ID); err == nil {
		t.Fatal("expected lost retirement reply")
	}
	var count int
	if err = env.db.QueryRow(ctx, `SELECT count(*) FROM cloud_deletion_command_retirements`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unproven retirement %d %v", count, err)
	}
	old, err := activeDeletionClose(ctx, env.db, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.db.Exec(ctx, `INSERT INTO cloud_deletion_close_attempts(operation_id,settlement_id,readiness_sha256) VALUES($1,gen_random_uuid(),$2)`, op.ID, strings.Repeat("f", 64)); err == nil {
		t.Fatal("replacement before retirement accepted")
	}
	restarted := New(env.db)
	configureDeletionRecovery(t, restarted, f)
	if _, err = restarted.AdvanceCloudDeletion(ctx, op.CloudID, op.ID); !errors.Is(err, ErrHandoffSnapshotNotReady) {
		t.Fatalf("retirement replay %v", err)
	}
	if err = env.db.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&f.receipt); err != nil {
		t.Fatal(err)
	}
	op, err = restarted.AdvanceCloudDeletion(ctx, op.CloudID, op.ID)
	if err != nil || op.State != "succeeded" {
		t.Fatalf("fresh evidence recovery %+v %v", op, err)
	}
	if err = env.db.QueryRow(ctx, `SELECT count(*) FROM cloud_deletion_close_attempts`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("attempt history %d %v", count, err)
	}
	if _, err = f.CloseCloud(ctx, op.binding(), old.Receipt, old.SHA); err == nil {
		t.Fatal("late old command accepted")
	}
}

func TestCloudDeletionCancellationWaitsForEveryRelease(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "delete-cancel")
	f := newDeletionRecoveryFixture()
	configureDeletionRecovery(t, env.store, f)
	op, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID, "cancel")
	if err != nil {
		t.Fatal(err)
	}
	op, err = env.store.RequestCloudDeletionCancellation(ctx, owner.User.ID, op.CloudID, op.ID)
	if err != nil || op.Phase != "canceling" {
		t.Fatalf("intent %+v %v", op, err)
	}
	if _, err = env.store.AdvanceCloudDeletion(ctx, op.CloudID, op.ID); err == nil {
		t.Fatal("no releases succeeded")
	}
	var allowed bool
	if err = env.db.QueryRow(ctx, `SELECT user_can_access_brand_cloud($1,$2)`, owner.User.ID, op.CloudID).Scan(&allowed); err != nil || allowed {
		t.Fatalf("premature access %v %v", allowed, err)
	}
	f.producerReleased = true
	if _, err = env.store.AdvanceCloudDeletion(ctx, op.CloudID, op.ID); !errors.Is(err, ErrHandoffSnapshotNotReady) {
		t.Fatalf("Billing release missing %v", err)
	}
	if _, err = env.db.Exec(ctx, `UPDATE cloud_deletion_operations SET phase='canceled' WHERE id=$1`, op.ID); err == nil {
		t.Fatal("SQL premature release")
	}
	f.billingReleased = true
	if _, err = env.db.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT test_cancel_audit CHECK(event_type<>'brand_cloud_deletion_canceled') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Exec(context.Background(), `ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS test_cancel_audit`)
	})
	if _, err = env.store.AdvanceCloudDeletion(ctx, op.CloudID, op.ID); err == nil {
		t.Fatal("audit failure released")
	}
	if _, err = env.db.Exec(ctx, `ALTER TABLE audit_events DROP CONSTRAINT test_cancel_audit`); err != nil {
		t.Fatal(err)
	}
	op, err = env.store.AdvanceCloudDeletion(ctx, op.CloudID, op.ID)
	if err != nil || op.State != "canceled" {
		t.Fatalf("cancel recovery %+v %v", op, err)
	}
	if err = env.db.QueryRow(ctx, `SELECT user_can_access_brand_cloud($1,$2)`, owner.User.ID, op.CloudID).Scan(&allowed); err != nil || !allowed {
		t.Fatalf("access not restored %v %v", allowed, err)
	}
	if _, err = f.PrepareCloudDeletion(ctx, op.binding(), op.AuthorizationVersion+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("late producer prepare reacquired hold: %v", err)
	}
	replay, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, op.CloudID, "cancel")
	if err != nil || replay.ID != op.ID || replay.State != "canceled" {
		t.Fatalf("canceled replay %+v %v", replay, err)
	}
	next, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, op.CloudID, "new-deletion")
	if err != nil || next.ID == op.ID {
		t.Fatalf("fresh attempt %+v %v", next, err)
	}
	jobs, err := env.store.ClaimCloudDeletionJobs(ctx, 10, time.Minute)
	if err != nil || len(jobs) != 1 || jobs[0].OperationID != next.ID {
		t.Fatalf("canceled job reclaimed %+v %v", jobs, err)
	}
}

func TestCloudDeletionCancellationKeepsIdempotencyActorScoped(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "delete-former-owner")
	nextOwner := handoffDeveloper(t, env, "delete-new-owner")
	f := newDeletionRecoveryFixture()
	f.billingReleased, f.producerReleased = true, true
	configureDeletionRecovery(t, env.store, f)
	op, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID, "same-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.store.RequestCloudDeletionCancellation(ctx, owner.User.ID, op.CloudID, op.ID); err != nil {
		t.Fatal(err)
	}
	if canceled, err := env.store.AdvanceCloudDeletion(ctx, op.CloudID, op.ID); err != nil || canceled.State != "canceled" {
		t.Fatalf("cancel %+v %v", canceled, err)
	}
	// Synthetic committed ownership state, not proof of a Billing handoff.
	// Actor-scoped request keys must survive retained predecessor history.
	tx, err := env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DELETE FROM organization_members WHERE organization_id=$1 AND user_id=$2`, op.CloudID, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'owner')`, op.CloudID, nextOwner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE organizations SET ownership_version=ownership_version+1 WHERE id=$1`, op.CloudID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	next, err := env.store.RequestDeveloperCloudDeletion(ctx, nextOwner.User.ID, op.CloudID, "same-key")
	if err != nil || next.ID == op.ID || next.OwnerUserID != nextOwner.User.ID || next.OwnershipVersion != op.OwnershipVersion+1 {
		t.Fatalf("new owner's same key %+v %v", next, err)
	}
	replayed, err := env.store.RequestDeveloperCloudDeletion(ctx, nextOwner.User.ID, op.CloudID, "same-key")
	if err != nil || replayed.ID != next.ID {
		t.Fatalf("new owner's replay %+v %v", replayed, err)
	}
}

func TestCloudDeletionCloseWinsCancellation(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "delete-close-wins")
	f := newDeletionRecoveryFixture()
	f.failReply = true
	configureDeletionRecovery(t, env.store, f)
	op, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID, "close-wins")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.store.AdvanceCloudDeletion(ctx, op.CloudID, op.ID); err == nil {
		t.Fatal("expected lost close reply")
	}
	if _, err = env.store.RequestCloudDeletionCancellation(ctx, owner.User.ID, op.CloudID, op.ID); err != nil {
		t.Fatal(err)
	}
	op, err = env.store.AdvanceCloudDeletion(ctx, op.CloudID, op.ID)
	if err != nil || op.State != "succeeded" {
		t.Fatalf("known close rolled back %+v %v", op, err)
	}
	var releases int
	if err = env.db.QueryRow(ctx, `SELECT count(*) FROM cloud_deletion_release_receipts`).Scan(&releases); err != nil || releases != 0 {
		t.Fatalf("closed holds released %d %v", releases, err)
	}
	if _, err = env.store.RequestCloudDeletionCancellation(ctx, owner.User.ID, op.CloudID, op.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("completed cancel %v", err)
	}
}

func TestCloudDeletionCancellationInvalidatesStaleWriter(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "delete-stale-writer")
	f := newDeletionRecoveryFixture()
	configureDeletionRecovery(t, env.store, f)
	op, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID, "rr")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := env.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM cloud_deletion_cancellations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if _, err = env.store.RequestCloudDeletionCancellation(ctx, owner.User.ID, op.CloudID, op.ID); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO cloud_deletion_close_commands(operation_id,settlement_id,readiness_sha256) VALUES($1,gen_random_uuid(),$2)`, op.ID, strings.Repeat("a", 64))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "40001" {
		t.Fatalf("stale RR command writer was not invalidated: %v", err)
	}
}
