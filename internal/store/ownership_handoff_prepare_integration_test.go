package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func preparedAckFixture(t *testing.T, env storeIntegrationEnv) (HandoffPrepareAck, BrandCloudOwnerTransferQuery) {
	t.Helper()
	configureTestHandoff(t, env)
	source := handoffDeveloper(t, env, "prepare-source")
	target := handoffDeveloper(t, env, "prepare-target")
	op := handoffRequest(t, env, source, target, "prepare-token")
	if _, err := env.store.AcceptBrandCloudOwnerTransfer(context.Background(), target.User.ID, "prepare-token", time.Now()); err != nil {
		t.Fatal(err)
	}
	query := BrandCloudOwnerTransferQuery{BrandCloudID: source.BrandCloud.ID, TransferID: op.ID, RequesterID: target.User.ID}
	state, err := env.store.GetCloudHandoffPreparation(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	return HandoffPrepareAck{OperationID: op.ID, CloudID: source.BrandCloud.ID,
		SourceUserID: source.User.ID, TargetUserID: target.User.ID, OwnershipVersion: state.OwnershipVersion, Cutoff: state.Cutoff,
		Participant: "billing", HoldReceiptSHA256: strings.Repeat("a", 64), DrainCheckpointSHA256: strings.Repeat("b", 64)}, query
}

func TestHandoffPreparationRequiresEveryBoundDurableCheckpoint(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	ack, query := preparedAckFixture(t, env)
	state, err := env.store.GetCloudHandoffPreparation(ctx, query)
	if err != nil || state.AllParticipantsPrepared || !reflect.DeepEqual(state.MissingParticipants, []string{"billing", "test_resources"}) {
		t.Fatalf("missing inventory: %+v %v", state, err)
	}
	for _, change := range []func(*HandoffPrepareAck){
		func(a *HandoffPrepareAck) { a.OwnershipVersion++ },
		func(a *HandoffPrepareAck) { a.Cutoff = a.Cutoff.Add(time.Second) },
		func(a *HandoffPrepareAck) { a.CloudID = a.OperationID },
		func(a *HandoffPrepareAck) { a.SourceUserID = a.TargetUserID },
		func(a *HandoffPrepareAck) { a.TargetUserID = a.SourceUserID },
		func(a *HandoffPrepareAck) { a.Participant = "unregistered" },
	} {
		bad := ack
		change(&bad)
		if _, err := env.store.RecordCloudHandoffPrepareAck(ctx, bad); !errors.Is(err, ErrNotFound) {
			t.Fatalf("mismatched receipt accepted: %+v %v", bad, err)
		}
	}
	bad := ack
	bad.DrainCheckpointSHA256 = ""
	if _, err := env.store.RecordCloudHandoffPrepareAck(ctx, bad); !errors.Is(err, ErrConflict) {
		t.Fatalf("HTTP delivery without drained evidence accepted: %v", err)
	}
	state, err = env.store.RecordCloudHandoffPrepareAck(ctx, ack)
	if err != nil || state.AllParticipantsPrepared || !reflect.DeepEqual(state.MissingParticipants, []string{"test_resources"}) {
		t.Fatalf("one participant: %+v %v", state, err)
	}
	// Reconstruct the store to prove readiness is not an in-memory flag. The
	// persisted inventory, not the process's current configuration, is authoritative.
	restarted := New(env.db)
	if err := restarted.ConfigureOwnershipHandoff(OwnershipHandoffOptions{Eligibility: env.store.ownershipHandoff.Eligibility, Producers: []string{"different_inventory"}}); err != nil {
		t.Fatal(err)
	}
	if replay, err := restarted.RecordCloudHandoffPrepareAck(ctx, ack); err != nil || replay.AllParticipantsPrepared {
		t.Fatalf("durable replay: %+v %v", replay, err)
	}
	bad = ack
	bad.HoldReceiptSHA256 = strings.Repeat("c", 64)
	if _, err := restarted.RecordCloudHandoffPrepareAck(ctx, bad); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed receipt replay: %v", err)
	}
	ack.Participant = "test_resources"
	state, err = restarted.RecordCloudHandoffPrepareAck(ctx, ack)
	if err != nil || !state.AllParticipantsPrepared || len(state.MissingParticipants) != 0 || state.Phase != "preparing" {
		t.Fatalf("all prepared: %+v %v", state, err)
	}
	var owner string
	if err := env.db.QueryRow(ctx, `SELECT user_id::text FROM organization_members WHERE organization_id=$1 AND role='owner'`, ack.CloudID).Scan(&owner); err != nil || owner != ack.SourceUserID {
		t.Fatalf("prepared receipts changed owner: %s %v", owner, err)
	}
	for _, sql := range []string{
		`UPDATE cloud_handoff_prepare_acknowledgments SET hold_receipt_sha256=repeat('d',64) WHERE operation_id=$1`,
		`DELETE FROM cloud_handoff_prepare_acknowledgments WHERE operation_id=$1`,
	} {
		if _, err := env.db.Exec(ctx, sql, ack.OperationID); err == nil {
			t.Fatal("durable preparation evidence was mutable")
		}
	}
	outsider := handoffDeveloper(t, env, "prepare-outsider")
	query.RequesterID = outsider.User.ID
	if _, err := restarted.GetCloudHandoffPreparation(ctx, query); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider read receipt progress: %v", err)
	}
	query.RequesterID = ack.TargetUserID
	if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, ack.TargetUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.GetCloudHandoffPreparation(ctx, query); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled participant read receipt progress: %v", err)
	}
}

func TestHandoffPreparedReceiptsCannotReleaseCancellationFence(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	ack, query := preparedAckFixture(t, env)
	query.RequesterID = ack.SourceUserID
	if _, err := env.store.CancelBrandCloudOwnerTransfer(ctx, query, time.Now()); err != nil {
		t.Fatal(err)
	}
	if state, err := env.store.RecordCloudHandoffPrepareAck(ctx, ack); err != nil || state.AllParticipantsPrepared || state.Phase != "canceling" {
		t.Fatalf("late receipt escaped cancellation: %+v %v", state, err)
	}
	assertFence := func() {
		t.Helper()
		if _, err := env.db.Exec(ctx, `UPDATE cloud_ownership_handoffs SET phase='canceled',version=version+1 WHERE id=$1`, ack.OperationID); err == nil {
			t.Fatal("database released cancellation without all participants' abort receipts")
		}
		var allowed bool
		if err := env.db.QueryRow(ctx, `SELECT user_can_access_brand_cloud($1,$2)`, ack.SourceUserID, ack.CloudID).Scan(&allowed); err != nil || allowed {
			t.Fatalf("cloud fence lost: %v %v", allowed, err)
		}
	}
	assertFence()
	release := HandoffAbortAck{OperationID: ack.OperationID, CloudID: ack.CloudID, OwnershipVersion: ack.OwnershipVersion, Participant: "billing", ReceiptSHA256: strings.Repeat("d", 64)}
	if _, err := env.store.RecordCloudHandoffAbortAck(ctx, release); err != nil {
		t.Fatal(err)
	}
	assertFence()
	release.Participant = "test_resources"
	if phase, err := env.store.RecordCloudHandoffAbortAck(ctx, release); err != nil || phase != "canceled" {
		t.Fatalf("complete release: %s %v", phase, err)
	}
	if state, err := env.store.RecordCloudHandoffPrepareAck(ctx, ack); err != nil || state.Phase != "canceled" || state.AllParticipantsPrepared {
		t.Fatalf("terminal exact replay: %+v %v", state, err)
	}
	ack.Participant = "test_resources"
	if _, err := env.store.RecordCloudHandoffPrepareAck(ctx, ack); !errors.Is(err, ErrConflict) {
		t.Fatalf("new hold accepted after cancellation: %v", err)
	}
}

func TestHandoffPrepareReceiptAuditRollbackAndConcurrentReplay(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	ack, query := preparedAckFixture(t, env)
	if _, err := env.db.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT reject_prepare_receipt_audit CHECK(event_type<>'brand_cloud_owner_transfer_prepared_acknowledged') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Exec(context.Background(), `ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS reject_prepare_receipt_audit`)
	})
	if _, err := env.store.RecordCloudHandoffPrepareAck(ctx, ack); err == nil {
		t.Fatal("audit failure was ignored")
	}
	state, err := env.store.GetCloudHandoffPreparation(ctx, query)
	if err != nil || len(state.MissingParticipants) != 2 {
		t.Fatalf("audit failure retained receipt: %+v %v", state, err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE audit_events DROP CONSTRAINT reject_prepare_receipt_audit`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		in := ack
		if i%2 != 0 {
			in.Participant = "test_resources"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := env.store.RecordCloudHandoffPrepareAck(ctx, in)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	state, err = env.store.GetCloudHandoffPreparation(ctx, query)
	if err != nil || !state.AllParticipantsPrepared {
		t.Fatalf("concurrent receipts: %+v %v", state, err)
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE event_type='brand_cloud_owner_transfer_prepared_acknowledged'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("duplicate prepare audit: %d %v", count, err)
	}
}

func TestHandoffAcceptanceRechecksExpiryAfterRemoteEligibility(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	configureTestHandoff(t, env)
	source := handoffDeveloper(t, env, "expiry-source")
	target := handoffDeveloper(t, env, "expiry-target")
	op := handoffRequest(t, env, source, target, "expiry-token")
	started := time.Now().UTC()
	if err := env.store.ConfigureOwnershipHandoff(OwnershipHandoffOptions{Eligibility: handoffEligibilityFunc(func(_ context.Context, in HandoffEligibilityRequest) (HandoffEligibility, error) {
		// The expiry is after request start but before the remote call finishes.
		// This simulates a slow dependency without relying on scheduler timing.
		if _, err := env.db.Exec(ctx, `UPDATE brand_cloud_owner_transfers SET expires_at=$2 WHERE id=$1`, op.ID, started.Add(time.Microsecond)); err != nil {
			t.Fatal(err)
		}
		return syntheticEligibility(in), nil
	}), Producers: []string{"test_resources"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, "expiry-token", started); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired during dependency accepted: %v", err)
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM cloud_ownership_handoffs WHERE id=$1`, op.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired acceptance acquired reservation: %d %v", count, err)
	}
}
