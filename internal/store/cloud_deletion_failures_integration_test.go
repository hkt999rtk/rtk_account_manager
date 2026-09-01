package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/billinghandoff"
)

func TestCloudDeletionFinancialBlockersAreConservative(t *testing.T) {
	for _, tc := range []struct {
		input, want []string
	}{
		{nil, []string{"evidence_unavailable"}},
		{[]string{"balance_negative", "balance_positive", "usage_unsettled", "outstanding_debt", "unpaid_invoices", "payments_pending", "payment_setups_pending", "provider_revocations_pending", "refunds_pending", "disputes_open", "account_not_active", "unknown-provider-code"},
			[]string{"balance_nonzero", "usage_unsettled", "debt_outstanding", "payment_pending", "refund_pending", "dispute_pending", "lifecycle_conflict", "evidence_unavailable"}},
	} {
		out := closureFinancialBlockers(tc.input)
		codes := []string{}
		for _, blocker := range out {
			codes = append(codes, blocker.Code)
			if !blocker.Retryable {
				t.Fatalf("recoverable blocker is terminal: %+v", blocker)
			}
		}
		if !reflect.DeepEqual(codes, tc.want) {
			t.Fatalf("blockers %v, want %v", codes, tc.want)
		}
		err := &cloudDeletionBlockers{blockers: out}
		if !errors.Is(err, ErrCloudDeletionBlocked) || err.Error() != ErrCloudDeletionBlocked.Error() {
			t.Fatalf("lost safe error classification: %v", err)
		}
	}
}

func TestCloudDeletionAuditFailuresRollbackEachDurableDecision(t *testing.T) {
	for _, action := range []string{"requested", "producer_held", "close_requested", "command_retired", "cancellation_requested", "hold_released"} {
		t.Run(action, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			owner := handoffDeveloper(t, env, "delete-audit-"+action)
			f := newDeletionRecoveryFixture()
			configureDeletionRecovery(t, env.store, f)
			var op CloudDeletionOperation
			var err error
			if action != "requested" {
				op, err = env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID, "atomic")
				if err != nil {
					t.Fatal(err)
				}
			}
			hold, _ := f.PrepareCloudDeletion(ctx, op.binding(), op.AuthorizationVersion+1)
			if action == "close_requested" || action == "command_retired" {
				if err = env.store.recordDeletionHold(ctx, op, hold); err != nil {
					t.Fatal(err)
				}
			}
			var command deletionCloseAttempt
			if action == "command_retired" {
				if _, err = env.store.prepareDeletionClose(ctx, op, op.ID); err != nil {
					t.Fatal(err)
				}
				command, err = activeDeletionClose(ctx, env.db, op.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if action == "hold_released" {
				op, err = env.store.RequestCloudDeletionCancellation(ctx, owner.User.ID, op.CloudID, op.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			// Only this owned integration DB is altered; the guard is removed
			// before a real retry and also on assertion failure.
			if _, err = env.db.Exec(ctx, fmt.Sprintf(`ALTER TABLE audit_events ADD CONSTRAINT deletion_audit_failure CHECK(event_type<>'brand_cloud_deletion_%s') NOT VALID`, action)); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = env.db.Exec(context.Background(), `ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS deletion_audit_failure`)
			})
			var table string
			attempt := func() error {
				switch action {
				case "requested":
					table = "cloud_deletion_operations"
					_, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID, "atomic")
					return err
				case "producer_held":
					table = "cloud_deletion_resource_receipts"
					return env.store.recordDeletionHold(ctx, op, hold)
				case "close_requested":
					table = "cloud_deletion_close_attempts"
					_, err := env.store.prepareDeletionClose(ctx, op, op.ID)
					return err
				case "command_retired":
					table = "cloud_deletion_command_retirements"
					_, err := env.store.resolveDeletionClose(ctx, op, command)
					return err
				case "cancellation_requested":
					table = "cloud_deletion_cancellations"
					_, err := env.store.RequestCloudDeletionCancellation(ctx, owner.User.ID, op.CloudID, op.ID)
					return err
				default:
					table = "cloud_deletion_release_receipts"
					return env.store.recordDeletionRelease(ctx, op, "test_resources", strings.Repeat("e", 64))
				}
			}
			if err = attempt(); err == nil {
				t.Fatal("audit failure accepted")
			}
			var count int
			if err = env.db.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial decision survived audit failure: %s=%d, %v", table, count, err)
			}
			if _, err = env.db.Exec(ctx, `ALTER TABLE audit_events DROP CONSTRAINT deletion_audit_failure`); err != nil {
				t.Fatal(err)
			}
			if err = attempt(); err != nil {
				t.Fatalf("safe retry: %v", err)
			}
			if err = env.db.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 1 {
				t.Fatalf("missing durable retry: %s=%d, %v", table, count, err)
			}
		})
	}
}

type deletionResolutionFixture struct {
	*deletionRecoveryFixture
	resolution billinghandoff.CloseCommandResolution
}

func (f *deletionResolutionFixture) RetireCloudClose(context.Context, billinghandoff.ClosureBinding, string, string) (billinghandoff.CloseCommandResolution, error) {
	return f.resolution, nil
}

func TestCloudDeletionRecoveryRejectsMalformedRetirementProof(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "delete-proof")
	f := newDeletionRecoveryFixture()
	configureDeletionRecovery(t, env.store, f)
	op, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID, "proof")
	if err != nil {
		t.Fatal(err)
	}
	hold, _ := f.PrepareCloudDeletion(ctx, op.binding(), op.AuthorizationVersion+1)
	if err = env.store.recordDeletionHold(ctx, op, hold); err != nil {
		t.Fatal(err)
	}
	if _, err = env.store.prepareDeletionClose(ctx, op, op.ID); err != nil {
		t.Fatal(err)
	}
	command, err := activeDeletionClose(ctx, env.db, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	valid := billinghandoff.CloseCommandResolution{OperationID: op.ID, SettlementID: command.Receipt, AMReadinessSHA256: command.SHA, Outcome: "retired", RetiredAt: &now, ReceiptSHA256: strings.Repeat("a", 64)}
	remote := &deletionResolutionFixture{deletionRecoveryFixture: f}
	env.store.deletion.Billing = remote
	for _, tc := range []struct {
		name   string
		mutate func(*billinghandoff.CloseCommandResolution)
	}{
		{"wrong operation", func(r *billinghandoff.CloseCommandResolution) { r.OperationID = "another" }},
		{"wrong settlement", func(r *billinghandoff.CloseCommandResolution) { r.SettlementID = "another" }},
		{"wrong command", func(r *billinghandoff.CloseCommandResolution) { r.AMReadinessSHA256 = strings.Repeat("b", 64) }},
		{"unknown outcome", func(r *billinghandoff.CloseCommandResolution) { r.Outcome = "pending" }},
		{"missing timestamp", func(r *billinghandoff.CloseCommandResolution) { r.RetiredAt = nil }},
		{"empty timestamp", func(r *billinghandoff.CloseCommandResolution) { r.RetiredAt = &time.Time{} }},
		{"bad digest", func(r *billinghandoff.CloseCommandResolution) { r.ReceiptSHA256 = "untrusted" }},
		{"mixed outcomes", func(r *billinghandoff.CloseCommandResolution) {
			r.Acknowledgment = &billinghandoff.ClosureAcknowledgment{}
		}},
		{"closed without acknowledgment", func(r *billinghandoff.CloseCommandResolution) {
			r.Outcome = "closed"
			r.RetiredAt = nil
			r.ReceiptSHA256 = ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote.resolution = valid
			tc.mutate(&remote.resolution)
			if closed, err := env.store.resolveDeletionClose(ctx, op, command); closed || !errors.Is(err, ErrHandoffUnavailable) {
				t.Fatalf("invalid proof accepted: %t %v", closed, err)
			}
		})
	}
	var count int
	if err = env.db.QueryRow(ctx, `SELECT count(*) FROM cloud_deletion_command_retirements`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid proof persisted: %d %v", count, err)
	}
	remote.resolution = valid
	for i := 0; i < 2; i++ {
		if closed, err := env.store.resolveDeletionClose(ctx, op, command); err != nil || closed {
			t.Fatalf("valid replay: %t %v", closed, err)
		}
	}
	remote.resolution.ReceiptSHA256 = strings.Repeat("b", 64)
	if _, err = env.store.resolveDeletionClose(ctx, op, command); !errors.Is(err, ErrConflict) {
		t.Fatalf("contradictory replay: %v", err)
	}
	current, err := env.store.loadDeletion(ctx, op.CloudID, op.ID)
	if err != nil || current.Phase != "closing" {
		t.Fatalf("retirement changed deletion phase: %+v %v", current, err)
	}
}

func TestCloudDeletionReleaseRejectsWrongPhaseAndConflictingReplay(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "delete-release-proof")
	f := newDeletionRecoveryFixture()
	configureDeletionRecovery(t, env.store, f)
	op, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID, "release")
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 64)
	if err = env.store.recordDeletionRelease(ctx, op, "test_resources", "bad"); !errors.Is(err, ErrHandoffUnavailable) {
		t.Fatalf("bad receipt: %v", err)
	}
	if err = env.store.recordDeletionRelease(ctx, op, "test_resources", sha); !errors.Is(err, ErrConflict) {
		t.Fatalf("release before cancellation: %v", err)
	}
	op, err = env.store.RequestCloudDeletionCancellation(ctx, owner.User.ID, op.CloudID, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = env.store.recordDeletionRelease(ctx, op, "unknown_producer", sha); !errors.Is(err, ErrConflict) {
		t.Fatalf("unregistered producer: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err = env.store.recordDeletionRelease(ctx, op, "test_resources", sha); err != nil {
			t.Fatal(err)
		}
	}
	if err = env.store.recordDeletionRelease(ctx, op, "test_resources", strings.Repeat("b", 64)); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting release: %v", err)
	}
	current, err := env.store.loadDeletion(ctx, op.CloudID, op.ID)
	if err != nil || current.Phase != "canceling" {
		t.Fatalf("partial release reopened cloud: %+v %v", current, err)
	}
}

func TestCloudDeletionDelayedHoldCannotSucceedAfterCancellation(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "delete-late-hold")
	f := newDeletionRecoveryFixture()
	configureDeletionRecovery(t, env.store, f)
	op, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID, "late-hold")
	if err != nil {
		t.Fatal(err)
	}
	hold, err := f.PrepareCloudDeletion(ctx, op.binding(), op.AuthorizationVersion+1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.store.RequestCloudDeletionCancellation(ctx, owner.User.ID, op.CloudID, op.ID); err != nil {
		t.Fatal(err)
	}
	// Simulate delivery of the already-issued preparation response after
	// cancellation. It is not a release and cannot be recorded as a new hold.
	if err = env.store.recordDeletionHold(ctx, op, hold); !errors.Is(err, ErrConflict) {
		t.Fatalf("late hold falsely acknowledged: %v", err)
	}
	var evidence int
	if err = env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM cloud_deletion_resource_receipts WHERE operation_id=$1::uuid) + (SELECT count(*) FROM audit_events WHERE subject_id=$1::text AND event_type='brand_cloud_deletion_producer_held')`, op.ID).Scan(&evidence); err != nil || evidence != 0 {
		t.Fatalf("late hold evidence/audit: %d %v", evidence, err)
	}
	f.billingReleased, f.producerReleased = true, true
	finished, err := env.store.AdvanceCloudDeletion(ctx, op.CloudID, op.ID)
	if err != nil || finished.Phase != "canceled" {
		t.Fatalf("cancellation did not recover: %+v %v", finished, err)
	}
}
