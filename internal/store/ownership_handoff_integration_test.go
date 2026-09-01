package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

type handoffEligibilityFunc func(context.Context, HandoffEligibilityRequest) (HandoffEligibility, error)

func (f handoffEligibilityFunc) CheckOwnershipEligibility(ctx context.Context, in HandoffEligibilityRequest) (HandoffEligibility, error) {
	return f(ctx, in)
}
func syntheticEligibility(in HandoffEligibilityRequest) HandoffEligibility {
	now := time.Now().UTC()
	return HandoffEligibility{Request: in, ReceiptID: "synthetic-test-receipt", EvidenceSHA256: strings.Repeat("a", 64), Currency: "TWD", BalanceMinor: 0, Complete: true, ObservedAt: now, ExpiresAt: now.Add(time.Minute)}
}
func configureTestHandoff(t *testing.T, env storeIntegrationEnv) {
	t.Helper()
	if err := env.store.ConfigureOwnershipHandoff(OwnershipHandoffOptions{Eligibility: handoffEligibilityFunc(func(_ context.Context, in HandoffEligibilityRequest) (HandoffEligibility, error) {
		return syntheticEligibility(in), nil
	}), Producers: []string{"test_resources"}}); err != nil {
		t.Fatal(err)
	}
}
func handoffDeveloper(t *testing.T, env storeIntegrationEnv, name string) DeveloperSignupResult {
	t.Helper()
	ctx := context.Background()
	u, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: name + "@example.test", PasswordHash: "test-hash"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true,signup_pending_verification=false WHERE id=$1`, u.User.ID); err != nil {
		t.Fatal(err)
	}
	return u
}
func handoffRequest(t *testing.T, env storeIntegrationEnv, source, target DeveloperSignupResult, key string) model.BrandCloudOwnerTransfer {
	t.Helper()
	op, err := env.store.CreateBrandCloudOwnerTransfer(context.Background(), BrandCloudOwnerTransferInput{BrandCloudID: source.BrandCloud.ID, RequestedByUserID: source.User.ID, TargetEmail: target.User.Email, TokenHash: key, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestHandoffAcceptanceReservesQuotaAndCancellationWaitsForEveryHoldRelease(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	configureTestHandoff(t, env)
	source := handoffDeveloper(t, env, "handoff-source")
	target := handoffDeveloper(t, env, "handoff-target")
	outsider := handoffDeveloper(t, env, "handoff-outsider")
	if err := env.store.SetDeveloperCloudLimit(ctx, target.User.ID, 2); err != nil {
		t.Fatal(err)
	}
	request := handoffRequest(t, env, source, target, "handoff-token")
	accepted, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, "handoff-token", time.Now())
	if err != nil || accepted.OperationPhase != "preparing" || accepted.OwnershipVersion != 1 {
		t.Fatalf("accept=%+v %v", accepted, err)
	}
	privateQuery := BrandCloudOwnerTransferQuery{BrandCloudID: source.BrandCloud.ID, TransferID: request.ID, RequesterID: outsider.User.ID}
	if _, err := env.store.GetBrandCloudOwnerTransfer(ctx, privateQuery, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unrelated cloud owner read operation: %v", err)
	}
	privateQuery.RequesterID = target.User.ID
	if _, err := env.store.CancelBrandCloudOwnerTransfer(ctx, privateQuery, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("target canceled source operation: %v", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, target.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.GetBrandCloudOwnerTransfer(ctx, privateQuery, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled participant read operation: %v", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at=NULL WHERE id=$1`, target.User.ID); err != nil {
		t.Fatal(err)
	}
	var owner string
	var allowed bool
	var commands int
	if err := env.db.QueryRow(ctx, `SELECT user_id::text FROM organization_members WHERE organization_id=$1 AND role='owner'`, source.BrandCloud.ID).Scan(&owner); err != nil || owner != source.User.ID {
		t.Fatalf("accept changed owner: %s %v", owner, err)
	}
	if err := env.db.QueryRow(ctx, `SELECT user_can_access_brand_cloud($1,$2)`, source.User.ID, source.BrandCloud.ID).Scan(&allowed); err != nil || allowed {
		t.Fatalf("preparation lacked cloud fence: %v %v", allowed, err)
	}
	page, err := env.store.ListManagedBrandClouds(ctx, target.User.ID, "all", 25, 0)
	if err != nil || page.OwnedCount != 1 || page.ReservedCount != 1 || page.Total != 1 {
		t.Fatalf("target became member/owner early: %+v %v", page, err)
	}
	if _, err := env.store.CreateDeveloperBrandCloud(ctx, target.User.ID, BrandCloudInput{Name: "over quota"}); !errors.Is(err, ErrDeveloperCloudLimitExceeded) {
		t.Fatalf("reservation bypass: %v", err)
	}
	if replay, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, "handoff-token", time.Now()); err != nil || replay.ID != accepted.ID {
		t.Fatalf("accept replay: %+v %v", replay, err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM cloud_handoff_outbox WHERE operation_id=$1 AND action='prepare'`, request.ID).Scan(&commands); err != nil || commands != 2 {
		t.Fatalf("prepare commands=%d %v", commands, err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE cloud_handoff_outbox SET payload='{}' WHERE operation_id=$1`, request.ID); err == nil {
		t.Fatal("outbox payload was mutable")
	}
	q := BrandCloudOwnerTransferQuery{BrandCloudID: source.BrandCloud.ID, TransferID: request.ID, RequesterID: source.User.ID}
	cancel, err := env.store.CancelBrandCloudOwnerTransfer(ctx, q, time.Now().Add(2*time.Hour))
	if err != nil || cancel.OperationPhase != "canceling" || cancel.Status != "accepted" {
		t.Fatalf("cancel released early: %+v %v", cancel, err)
	}
	if _, err := env.store.CreateDeveloperBrandCloud(ctx, target.User.ID, BrandCloudInput{Name: "still reserved"}); !errors.Is(err, ErrDeveloperCloudLimitExceeded) {
		t.Fatalf("cancel released quota early: %v", err)
	}
	ack := HandoffAbortAck{OperationID: request.ID, CloudID: source.BrandCloud.ID, OwnershipVersion: 1, Participant: "billing", ReceiptSHA256: strings.Repeat("b", 64)}
	if phase, err := env.store.RecordCloudHandoffAbortAck(ctx, ack); err != nil || phase != "canceling" {
		t.Fatalf("first ack=%s %v", phase, err)
	}
	if phase, err := env.store.RecordCloudHandoffAbortAck(ctx, ack); err != nil || phase != "canceling" {
		t.Fatalf("ack replay=%s %v", phase, err)
	}
	changed := ack
	changed.ReceiptSHA256 = strings.Repeat("c", 64)
	if _, err := env.store.RecordCloudHandoffAbortAck(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed ack accepted: %v", err)
	}
	ack.Participant = "unexpected"
	if _, err := env.store.RecordCloudHandoffAbortAck(ctx, ack); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown participant accepted: %v", err)
	}
	ack.Participant = "test_resources"
	if phase, err := env.store.RecordCloudHandoffAbortAck(ctx, ack); err != nil || phase != "canceled" {
		t.Fatalf("final ack=%s %v", phase, err)
	}
	if err := env.db.QueryRow(ctx, `SELECT user_can_access_brand_cloud($1,$2)`, source.User.ID, source.BrandCloud.ID).Scan(&allowed); err != nil || !allowed {
		t.Fatalf("hold never released: %v %v", allowed, err)
	}
	if _, err := env.store.CreateDeveloperBrandCloud(ctx, target.User.ID, BrandCloudInput{Name: "quota released"}); err != nil {
		t.Fatal(err)
	}
}

func TestHandoffFinancialEvidenceFailsClosedAtRequestAndAcceptance(t *testing.T) {
	for _, balance := range []int64{-1, 0, 1} {
		t.Run(fmt.Sprint(balance), func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			source := handoffDeveloper(t, env, "financial-source")
			target := handoffDeveloper(t, env, "financial-target")
			in := BrandCloudOwnerTransferInput{BrandCloudID: source.BrandCloud.ID, RequestedByUserID: source.User.ID, TargetEmail: target.User.Email, TokenHash: "financial-token", ExpiresAt: time.Now().Add(time.Hour)}
			if _, err := env.store.CreateBrandCloudOwnerTransfer(ctx, in); !errors.Is(err, ErrHandoffUnavailable) {
				t.Fatalf("missing client did not fail closed: %v", err)
			}
			mode := "normal"
			provider := handoffEligibilityFunc(func(_ context.Context, r HandoffEligibilityRequest) (HandoffEligibility, error) {
				e := syntheticEligibility(r)
				e.BalanceMinor = balance
				switch mode {
				case "blocked":
					e.Blockers = []string{"usage_unsettled"}
				case "changed":
					e.Request.TargetUserID = source.User.ID
				case "expired":
					e.ExpiresAt = e.ObservedAt.Add(-time.Second)
				case "negative_accept":
					if r.Action == "accept" {
						e.BalanceMinor = -1
					}
				}
				return e, nil
			})
			if err := env.store.ConfigureOwnershipHandoff(OwnershipHandoffOptions{Eligibility: provider, Producers: []string{"test_resources"}}); err != nil {
				t.Fatal(err)
			}
			for _, bad := range []string{"changed", "expired"} {
				mode = bad
				if _, err := env.store.CreateBrandCloudOwnerTransfer(ctx, in); !errors.Is(err, ErrHandoffUnavailable) {
					t.Fatalf("%s evidence=%v", bad, err)
				}
			}
			mode = "blocked"
			if _, err := env.store.CreateBrandCloudOwnerTransfer(ctx, in); !errors.Is(err, ErrHandoffFinancialBlocked) {
				t.Fatalf("positive credit masked blocker: %v", err)
			}
			mode = "normal"
			_, err := env.store.CreateBrandCloudOwnerTransfer(ctx, in)
			if balance < 0 {
				if !errors.Is(err, ErrHandoffFinancialBlocked) {
					t.Fatalf("negative balance=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			mode = "negative_accept"
			if _, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, in.TokenHash, time.Now()); !errors.Is(err, ErrHandoffFinancialBlocked) {
				t.Fatalf("accept used request-time balance: %v", err)
			}
			mode = "normal"
			if accepted, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, in.TokenHash, time.Now()); err != nil || accepted.OperationPhase != "preparing" {
				t.Fatalf("eligible accept=%+v %v", accepted, err)
			}
		})
	}
}

func TestConcurrentHandoffAcceptsCannotOverReserveTargetQuota(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	configureTestHandoff(t, env)
	ctx := context.Background()
	one := handoffDeveloper(t, env, "one-source")
	two := handoffDeveloper(t, env, "two-source")
	target := handoffDeveloper(t, env, "quota-target")
	if err := env.store.SetDeveloperCloudLimit(ctx, target.User.ID, 2); err != nil {
		t.Fatal(err)
	}
	handoffRequest(t, env, one, target, "one")
	handoffRequest(t, env, two, target, "two")
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, key := range []string{"one", "two"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			_, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, key, time.Now())
			errs <- err
		}(key)
	}
	wg.Wait()
	close(errs)
	success, denied := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrDeveloperCloudLimitExceeded) {
			denied++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || denied != 1 {
		t.Fatalf("accepts=%d quota denials=%d", success, denied)
	}
}

func TestHandoffConcurrentSameAcceptanceAndAuditRollback(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	configureTestHandoff(t, env)
	ctx := context.Background()
	source := handoffDeveloper(t, env, "retry-source")
	target := handoffDeveloper(t, env, "retry-target")
	request := handoffRequest(t, env, source, target, "retry-token")
	if _, err := env.db.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT test_handoff_audit_failure CHECK(event_type<>'brand_cloud_owner_transfer_preparation_requested') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Exec(context.Background(), `ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS test_handoff_audit_failure`)
	})
	if _, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, "retry-token", time.Now()); err == nil {
		t.Fatal("accepted without audit")
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE audit_events DROP CONSTRAINT test_handoff_audit_failure`); err != nil {
		t.Fatal(err)
	}
	var operations, commands int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM cloud_ownership_handoffs),(SELECT count(*) FROM cloud_handoff_outbox)`).Scan(&operations, &commands); err != nil || operations != 0 || commands != 0 {
		t.Fatalf("rollback left operation/outbox: %d/%d %v", operations, commands, err)
	}
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			accepted, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, "retry-token", time.Now())
			if err == nil && (accepted.ID != request.ID || accepted.OperationPhase != "preparing") {
				err = fmt.Errorf("wrong operation %+v", accepted)
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM cloud_ownership_handoffs),(SELECT count(*) FROM cloud_handoff_outbox)`).Scan(&operations, &commands); err != nil || operations != 1 || commands != 2 {
		t.Fatalf("duplicate operation/outbox: %d/%d %v", operations, commands, err)
	}
	// The pre-existing Product eligibility function must use the new lifecycle gate.
	var allowed bool
	if err := env.db.QueryRow(ctx, `SELECT user_can_access_brand_cloud_product($1,$2,NULL)`, source.User.ID, source.BrandCloud.ID).Scan(&allowed); err != nil || allowed {
		t.Fatalf("Product eligibility bypassed handoff: %v %v", allowed, err)
	}
}

func TestHandoffRechecksOwnerAndTargetAfterExternalPreflight(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	configureTestHandoff(t, env)
	ctx := context.Background()
	source := handoffDeveloper(t, env, "stale-source")
	target := handoffDeveloper(t, env, "stale-target")
	handoffRequest(t, env, source, target, "stale-token")
	provider := handoffEligibilityFunc(func(ctx context.Context, r HandoffEligibilityRequest) (HandoffEligibility, error) {
		// No AM row lock may be held across the Billing call. Simulate eligibility
		// changing while the remote response is in flight, before local commit.
		_, err := env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, target.User.ID)
		return syntheticEligibility(r), err
	})
	if err := env.store.ConfigureOwnershipHandoff(OwnershipHandoffOptions{Eligibility: provider, Producers: []string{"test_resources"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, "stale-token", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled target admitted: %v", err)
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM cloud_ownership_handoffs`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("disabled target got reservation: %d %v", count, err)
	}
}
