package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rtk_account_manager/internal/billinghandoff"
	"rtk_account_manager/internal/model"
)

type balanceTestBilling struct {
	mu              sync.Mutex
	version, amount int64
	confirmed       map[string]bool
	loseReply       bool
	reads           int
}

func (b *balanceTestBilling) snapshot(in billinghandoff.Binding) billinghandoff.Settlement {
	return billinghandoff.Settlement{OperationID: in.OperationID, Phase: "prepared", Blockers: []string{}, Snapshot: &billinghandoff.Snapshot{
		Version: b.version, BalanceMinor: b.amount, Currency: "TWD", Cutoff: in.Cutoff, SourceConfirmed: b.confirmed[in.SourceUserID], TargetConfirmed: b.confirmed[in.TargetUserID]}}
}
func (b *balanceTestBilling) Settlement(_ context.Context, in billinghandoff.Binding) (billinghandoff.Settlement, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads++
	return b.snapshot(in), nil
}
func (b *balanceTestBilling) Confirm(_ context.Context, in billinghandoff.Binding, confirm billinghandoff.Confirmation) (billinghandoff.Settlement, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if confirm.SnapshotVersion != b.version || confirm.BalanceMinor != b.amount || confirm.Currency != "TWD" {
		return billinghandoff.Settlement{}, billinghandoff.ErrUnavailable
	}
	b.confirmed[confirm.UserID] = true
	if b.loseReply {
		b.loseReply = false
		return billinghandoff.Settlement{}, billinghandoff.ErrUnavailable
	}
	return b.snapshot(in), nil
}

type balanceIntercept struct {
	HandoffBilling
	read func(context.Context, billinghandoff.Binding) (billinghandoff.Settlement, error)
}

func (b balanceIntercept) Settlement(ctx context.Context, in billinghandoff.Binding) (billinghandoff.Settlement, error) {
	return b.read(ctx, in)
}

func readyBalanceFixture(t *testing.T, env storeIntegrationEnv, amount int64) (*balanceTestBilling, HandoffPrepareAck, BrandCloudOwnerTransferQuery) {
	t.Helper()
	ack, query := preparedAckFixture(t, env)
	for _, participant := range []string{"billing", "test_resources"} {
		ack.Participant = participant
		if _, err := env.store.RecordCloudHandoffPrepareAck(context.Background(), ack); err != nil {
			t.Fatal(err)
		}
	}
	remote := &balanceTestBilling{version: 2, amount: amount, confirmed: map[string]bool{}}
	if err := env.store.ConfigureHandoffBilling(remote); err != nil {
		t.Fatal(err)
	}
	return remote, ack, query
}
func confirmBalance(t *testing.T, env storeIntegrationEnv, query BrandCloudOwnerTransferQuery, snapshot model.CloudBalanceSnapshot, key string) model.BrandCloudOwnerTransfer {
	t.Helper()
	view, err := env.store.ConfirmOwnerHandoff(context.Background(), HandoffConfirmationInput{Query: query, Snapshot: snapshot, IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func TestHandoffBalancePreviewRequiresAllPrepareEvidenceAndPreservesSnapshot(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	ack, query := preparedAckFixture(t, env)
	remote := &balanceTestBilling{version: 2, confirmed: map[string]bool{}}
	if err := env.store.ConfigureHandoffBilling(remote); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.PreviewOwnerHandoff(ctx, query); !errors.Is(err, ErrHandoffSnapshotNotReady) {
		t.Fatalf("missing producer proof allowed preview: %v", err)
	}
	if remote.reads != 0 {
		t.Fatal("unprepared operation reached Billing")
	}
	for _, participant := range []string{"billing", "test_resources"} {
		ack.Participant = participant
		if _, err := env.store.RecordCloudHandoffPrepareAck(ctx, ack); err != nil {
			t.Fatal(err)
		}
	}
	view, err := env.store.PreviewOwnerHandoff(ctx, query)
	if err != nil || view.Phase != "awaiting_balance_confirmation" || !view.HasSettledSnapshot || view.BalanceSnapshot.BalanceMinor != 0 || view.SourceConfirmed == nil || *view.SourceConfirmed || view.Operation == nil {
		t.Fatalf("zero preview: %+v %v", view, err)
	}
	query.RequesterID = ack.SourceUserID
	view = confirmBalance(t, env, query, *view.BalanceSnapshot, "source-consent")
	if !*view.SourceConfirmed || *view.TargetConfirmed {
		t.Fatalf("source consent: %+v", view)
	}
	query.RequesterID = ack.TargetUserID
	view = confirmBalance(t, env, query, *view.BalanceSnapshot, "target-consent")
	if !*view.SourceConfirmed || !*view.TargetConfirmed {
		t.Fatalf("two consents: %+v", view)
	}
	var owner string
	if err := env.db.QueryRow(ctx, `SELECT user_id::text FROM organization_members WHERE organization_id=$1 AND role='owner'`, ack.CloudID).Scan(&owner); err != nil || owner != ack.SourceUserID {
		t.Fatalf("confirmation changed owner: %s %v", owner, err)
	}
	// Simulate API process restart with a temporarily unavailable Billing client.
	restarted := New(env.db)
	view, err = restarted.GetOwnerHandoffStatus(ctx, query)
	if err != nil || !view.HasSettledSnapshot || view.BalanceSnapshot.BalanceMinor != 0 || view.SourceConfirmed == nil || *view.SourceConfirmed || *view.TargetConfirmed || view.Phase != "blocked" {
		t.Fatalf("outage lost preview or retained stale consent: %+v %v", view, err)
	}
	if err := restarted.ConfigureHandoffBilling(remote); err != nil {
		t.Fatal(err)
	}
	view, err = restarted.GetOwnerHandoffStatus(ctx, query)
	if err != nil || !*view.SourceConfirmed || !*view.TargetConfirmed {
		t.Fatalf("reload lost remote durable progress: %+v %v", view, err)
	}
	query.RequesterID = ack.SourceUserID
	view, err = restarted.CancelBrandCloudOwnerTransfer(ctx, query, time.Now())
	if err != nil || view.OperationPhase != "canceling" || !view.HasSettledSnapshot || *view.SourceConfirmed || *view.TargetConfirmed {
		t.Fatalf("cancel hid prior amount or kept confirmable consent: %+v %v", view, err)
	}
	for _, participant := range []string{"billing", "test_resources"} {
		if _, err := restarted.RecordCloudHandoffAbortAck(ctx, HandoffAbortAck{CloudID: ack.CloudID, OperationID: ack.OperationID, OwnershipVersion: 1, Participant: participant, ReceiptSHA256: strings.Repeat("e", 64)}); err != nil {
			t.Fatal(err)
		}
	}
	view, err = restarted.GetOwnerHandoffStatus(ctx, query)
	if err != nil || view.Phase != "canceled" || !view.HasSettledSnapshot || view.BalanceSnapshot.BalanceMinor != 0 || view.SourceConfirmed == nil {
		t.Fatalf("terminal reload lost snapshot: %+v %v", view, err)
	}
}

func TestHandoffBalanceChangeToZeroRequiresFreshConsentAndNewKey(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	remote, ack, query := readyBalanceFixture(t, env, 1)
	view, err := env.store.PreviewOwnerHandoff(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	old := *view.BalanceSnapshot
	query.RequesterID = ack.SourceUserID
	confirmBalance(t, env, query, old, "source-original")
	remote.mu.Lock()
	remote.version = 3
	remote.amount = 0
	remote.confirmed = map[string]bool{}
	remote.mu.Unlock()
	view, err = env.store.PreviewOwnerHandoff(ctx, query)
	if err != nil || view.BalanceSnapshot.BalanceMinor != 0 || view.BalanceSnapshot.BillingSnapshotVersion != 3 || *view.SourceConfirmed || *view.TargetConfirmed {
		t.Fatalf("new zero amount: %+v %v", view, err)
	}
	if _, err := env.store.ConfirmOwnerHandoff(ctx, HandoffConfirmationInput{Query: query, Snapshot: old, IdempotencyKey: "source-original"}); !errors.Is(err, ErrHandoffSnapshotNotReady) {
		t.Fatalf("stale consent accepted: %v", err)
	}
	if _, err := env.store.ConfirmOwnerHandoff(ctx, HandoffConfirmationInput{Query: query, Snapshot: *view.BalanceSnapshot, IdempotencyKey: "source-original"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("key changed amount/version: %v", err)
	}
	view = confirmBalance(t, env, query, *view.BalanceSnapshot, "source-zero")
	if !*view.SourceConfirmed {
		t.Fatal("new zero consent not recorded")
	}
	for _, sql := range []string{`UPDATE cloud_handoff_billing_snapshots SET balance_minor=99 WHERE operation_id=$1`, `DELETE FROM cloud_handoff_billing_snapshots WHERE operation_id=$1`, `UPDATE cloud_handoff_confirmation_requests SET idempotency_key='changed' WHERE operation_id=$1`} {
		if _, err := env.db.Exec(ctx, sql, ack.OperationID); err == nil {
			t.Fatal("immutable preview/consent modified")
		}
	}
}

func TestHandoffLostConfirmationReplyReplaysDurableIntent(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	remote, _, query := readyBalanceFixture(t, env, 0)
	view, err := env.store.PreviewOwnerHandoff(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	input := HandoffConfirmationInput{Query: query, Snapshot: *view.BalanceSnapshot, IdempotencyKey: "one-intent"}
	remote.loseReply = true
	if _, err := env.store.ConfirmOwnerHandoff(ctx, input); !errors.Is(err, ErrHandoffSnapshotNotReady) {
		t.Fatalf("lost reply treated as confirmed: %v", err)
	}
	var requests, acks int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM cloud_handoff_confirmation_requests),(SELECT count(*) FROM cloud_handoff_confirmation_acknowledgments)`).Scan(&requests, &acks); err != nil || requests != 1 || acks != 0 {
		t.Fatalf("missing durable intent: %d %d %v", requests, acks, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := env.store.ConfirmOwnerHandoff(ctx, input); err != nil {
			t.Fatal(err)
		}
	}
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM cloud_handoff_confirmation_requests),(SELECT count(*) FROM cloud_handoff_confirmation_acknowledgments)`).Scan(&requests, &acks); err != nil || requests != 1 || acks != 1 {
		t.Fatalf("retry duplicated consent: %d %d %v", requests, acks, err)
	}
}

func TestHandoffBalanceRechecksParticipantAfterRemoteRead(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	remote, ack, query := readyBalanceFixture(t, env, 0)
	if err := env.store.ConfigureHandoffBilling(balanceIntercept{HandoffBilling: remote, read: func(ctx context.Context, in billinghandoff.Binding) (billinghandoff.Settlement, error) {
		out, err := remote.Settlement(ctx, in)
		if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, ack.TargetUserID); err != nil {
			t.Fatal(err)
		}
		return out, err
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.PreviewOwnerHandoff(ctx, query); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled during read exposed snapshot: %v", err)
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM cloud_handoff_billing_snapshots`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("ineligible preview persisted: %d %v", count, err)
	}
}

func TestHandoffOlderRemoteSnapshotCannotReplaceNewerPreview(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	remote, _, query := readyBalanceFixture(t, env, 1)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	if err := env.store.ConfigureHandoffBilling(balanceIntercept{HandoffBilling: remote, read: func(ctx context.Context, in billinghandoff.Binding) (billinghandoff.Settlement, error) {
		out, err := remote.Settlement(ctx, in)
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return out, err
	}}); err != nil {
		t.Fatal(err)
	}
	results := make(chan model.BrandCloudOwnerTransfer, 1)
	errs := make(chan error, 1)
	go func() { view, err := env.store.GetOwnerHandoffStatus(ctx, query); results <- view; errs <- err }()
	<-started
	remote.mu.Lock()
	remote.version = 3
	remote.amount = 0
	remote.confirmed = map[string]bool{}
	remote.mu.Unlock()
	newer, err := env.store.PreviewOwnerHandoff(ctx, query)
	close(release)
	if err != nil || newer.BalanceSnapshot.BillingSnapshotVersion != 3 {
		t.Fatalf("newer preview: %+v %v", newer, err)
	}
	older := <-results
	if err := <-errs; err != nil || older.BalanceSnapshot.BillingSnapshotVersion != 3 || older.Phase != "blocked" || *older.SourceConfirmed {
		t.Fatalf("out of order read reverted preview: %+v %v", older, err)
	}
}

func TestHandoffConfirmationAuditFailureCannotEraseRemoteConsentIntent(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	_, _, query := readyBalanceFixture(t, env, 0)
	view, err := env.store.PreviewOwnerHandoff(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT reject_balance_ack CHECK(event_type<>'brand_cloud_owner_transfer_balance_confirmed') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Exec(context.Background(), `ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS reject_balance_ack`)
	})
	input := HandoffConfirmationInput{Query: query, Snapshot: *view.BalanceSnapshot, IdempotencyKey: "audit-retry"}
	if _, err := env.store.ConfirmOwnerHandoff(ctx, input); err == nil {
		t.Fatal("missing confirmation audit accepted")
	}
	var requests, acks int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM cloud_handoff_confirmation_requests),(SELECT count(*) FROM cloud_handoff_confirmation_acknowledgments)`).Scan(&requests, &acks); err != nil || requests != 1 || acks != 0 {
		t.Fatalf("audit failure destroyed intent: %d %d %v", requests, acks, err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE audit_events DROP CONSTRAINT reject_balance_ack`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ConfirmOwnerHandoff(ctx, input); err != nil {
		t.Fatal(err)
	}
}

func TestHandoffConfirmationConcurrentSameIntent(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	_, _, query := readyBalanceFixture(t, env, 0)
	view, err := env.store.PreviewOwnerHandoff(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	input := HandoffConfirmationInput{Query: query, Snapshot: *view.BalanceSnapshot, IdempotencyKey: "concurrent"}
	results := make(chan error, 6)
	for i := 0; i < cap(results); i++ {
		go func() { _, err := env.store.ConfirmOwnerHandoff(ctx, input); results <- err }()
	}
	for i := 0; i < cap(results); i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var requests, acks, events int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM cloud_handoff_confirmation_requests),
		(SELECT count(*) FROM cloud_handoff_confirmation_acknowledgments),
		(SELECT count(*) FROM audit_events WHERE event_type='brand_cloud_owner_transfer_balance_confirmed')`).Scan(&requests, &acks, &events); err != nil || requests != 1 || acks != 1 || events != 1 {
		t.Fatalf("concurrent confirmation duplicated evidence: %d %d %d %v", requests, acks, events, err)
	}
}

func TestHandoffLatePreviewCannotUndoCancellationOrExpiry(t *testing.T) {
	for _, mode := range []string{"cancel", "expire"} {
		t.Run(mode, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			remote, ack, query := readyBalanceFixture(t, env, 0)
			if _, err := env.store.PreviewOwnerHandoff(ctx, query); err != nil {
				t.Fatal(err)
			}
			if err := env.store.ConfigureHandoffBilling(balanceIntercept{HandoffBilling: remote, read: func(ctx context.Context, in billinghandoff.Binding) (billinghandoff.Settlement, error) {
				out, err := remote.Settlement(ctx, in)
				if mode == "expire" {
					if _, err := env.db.Exec(ctx, `UPDATE brand_cloud_owner_transfers SET expires_at=now()-interval '1 second' WHERE id=$1`, ack.OperationID); err != nil {
						t.Fatal(err)
					}
				} else {
					source := query
					source.RequesterID = ack.SourceUserID
					if _, err := env.store.CancelBrandCloudOwnerTransfer(ctx, source, time.Now()); err != nil {
						t.Fatal(err)
					}
					for _, participant := range []string{"billing", "test_resources"} {
						if _, err := env.store.RecordCloudHandoffAbortAck(ctx, HandoffAbortAck{CloudID: ack.CloudID, OperationID: ack.OperationID, OwnershipVersion: 1, Participant: participant, ReceiptSHA256: strings.Repeat("e", 64)}); err != nil {
							t.Fatal(err)
						}
					}
				}
				return out, err
			}}); err != nil {
				t.Fatal(err)
			}
			view, err := env.store.GetOwnerHandoffStatus(ctx, query)
			if err != nil || !view.HasSettledSnapshot || *view.SourceConfirmed || *view.TargetConfirmed {
				t.Fatalf("late snapshot exposed consent: %+v %v", view, err)
			}
			if mode == "cancel" && view.Phase != "canceled" || mode == "expire" && view.Phase != "blocked" {
				t.Fatalf("late snapshot changed lifecycle: %+v", view)
			}
		})
	}
}

func TestHandoffLiveFinancialBlockerInvalidatesConsentNotHistory(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	remote, _, query := readyBalanceFixture(t, env, 0)
	view, err := env.store.PreviewOwnerHandoff(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	confirmBalance(t, env, query, *view.BalanceSnapshot, "before-blocker")
	for _, tc := range []struct{ remote, public string }{
		{"balance_negative", "balance_negative"}, {"usage_unsettled", "usage_unsettled"},
		{"payments_pending", "payment_pending"}, {"disputes_open", "dispute_pending"},
		{"settlement_evidence_stale", "confirmation_stale"}, {"unknown", "evidence_unavailable"},
	} {
		if err := env.store.ConfigureHandoffBilling(balanceIntercept{HandoffBilling: remote, read: func(ctx context.Context, in billinghandoff.Binding) (billinghandoff.Settlement, error) {
			out, err := remote.Settlement(ctx, in)
			out.Blockers = []string{tc.remote}
			return out, err
		}}); err != nil {
			t.Fatal(err)
		}
		view, err := env.store.GetOwnerHandoffStatus(ctx, query)
		if err != nil || view.Phase != "blocked" || !view.HasSettledSnapshot || *view.SourceConfirmed || *view.TargetConfirmed || len(view.Blockers) != 1 || view.Blockers[0].Code != tc.public {
			t.Fatalf("blocker %s lost history or allowed consent: %+v %v", tc.remote, view, err)
		}
	}
}

func TestHandoffSnapshotBindingComparesCutoffInstant(t *testing.T) {
	instant := time.Date(2026, 8, 31, 2, 0, 0, 0, time.UTC)
	a := billinghandoff.Binding{CloudID: "cloud", OperationID: "op", SourceUserID: "source", TargetUserID: "target", OwnershipVersion: 1, Cutoff: instant}
	b := a
	b.Cutoff = instant.In(time.FixedZone("offset", 8*60*60))
	if !sameHandoffBinding(a, b) {
		t.Fatal("same instant rejected due to location representation")
	}
	b.Cutoff = b.Cutoff.Add(time.Microsecond)
	if sameHandoffBinding(a, b) {
		t.Fatal("different cutoff accepted")
	}
}
