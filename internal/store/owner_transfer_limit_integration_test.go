package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestOwnerTransferLimitAppliesToRequestsAcceptanceAndLegacyHistory(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	eligibilityCalls := 0
	if err := env.store.ConfigureOwnershipHandoff(OwnershipHandoffOptions{Eligibility: handoffEligibilityFunc(func(_ context.Context, in HandoffEligibilityRequest) (HandoffEligibility, error) {
		eligibilityCalls++
		return syntheticEligibility(in), nil
	}), Producers: RequiredHandoffProducers()}); err != nil {
		t.Fatal(err)
	}
	source := handoffDeveloper(t, env, "limit-source")
	target := handoffDeveloper(t, env, "limit-target")
	other := handoffDeveloper(t, env, "limit-other")
	quota, err := env.store.GetOwnerTransferQuota(ctx, source.BrandCloud.ID)
	if err != nil || quota != (OwnerTransferQuota{Limit: 3, Used: 0, Remaining: 3}) {
		t.Fatalf("default quota=%+v err=%v", quota, err)
	}
	if _, err := env.store.SetOwnerTransferLimit(ctx, source.User.ID, source.BrandCloud.ID, 0); err != nil {
		t.Fatal(err)
	}
	request := BrandCloudOwnerTransferInput{BrandCloudID: source.BrandCloud.ID, RequestedByUserID: source.User.ID,
		TargetEmail: target.User.Email, TokenHash: "limit-token-one", ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := env.store.CreateBrandCloudOwnerTransfer(ctx, request); !errors.Is(err, ErrOwnerTransferLimitReached) {
		t.Fatalf("request at zero limit: %v", err)
	}
	if eligibilityCalls != 0 {
		t.Fatalf("exhausted request called remote eligibility %d times", eligibilityCalls)
	}
	if _, err := env.store.SetOwnerTransferLimit(ctx, source.User.ID, source.BrandCloud.ID, 1); err != nil {
		t.Fatal(err)
	}
	transfer, err := env.store.CreateBrandCloudOwnerTransfer(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.SetOwnerTransferLimit(ctx, source.User.ID, source.BrandCloud.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, request.TokenHash, time.Now()); !errors.Is(err, ErrOwnerTransferLimitReached) {
		t.Fatalf("pending invitation accepted after limit lowered: %v", err)
	}
	if eligibilityCalls != 1 {
		t.Fatalf("exhausted acceptance called remote eligibility: %d calls", eligibilityCalls)
	}
	if _, err := env.store.CancelBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferQuery{BrandCloudID: source.BrandCloud.ID, TransferID: transfer.ID, RequesterID: source.User.ID}, time.Now()); err != nil {
		t.Fatal(err)
	}
	quota, err = env.store.GetOwnerTransferQuota(ctx, source.BrandCloud.ID)
	if err != nil || quota.Used != 0 || quota.Remaining != 0 {
		t.Fatalf("canceled invitation consumed quota: %+v %v", quota, err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO brand_cloud_owner_transfers(brand_cloud_id,requested_by_user_id,target_user_id,token_hash,status,expires_at,accepted_at)
		VALUES($1,$2,$3,'legacy-limit-token','accepted',now()+interval '1 hour',now())`, other.BrandCloud.ID, other.User.ID, target.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO brand_cloud_owner_transfers(brand_cloud_id,requested_by_user_id,target_user_id,token_hash,status,expires_at)
		VALUES($1,$2,$3,'expired-limit-token','expired',now()-interval '1 hour')`, other.BrandCloud.ID, other.User.ID, target.User.ID); err != nil {
		t.Fatal(err)
	}
	quota, err = env.store.GetOwnerTransferQuota(ctx, other.BrandCloud.ID)
	if err != nil || quota.Used != 1 || quota.Remaining != 2 {
		t.Fatalf("legacy transfer count=%+v %v", quota, err)
	}
	if _, err := env.store.SetOwnerTransferLimit(ctx, source.User.ID, other.BrandCloud.ID, 0); err != nil {
		t.Fatal(err)
	}
	quota, err = env.store.GetOwnerTransferQuota(ctx, other.BrandCloud.ID)
	if err != nil || quota.Used != 1 || quota.Remaining != 0 {
		t.Fatalf("limit below usage=%+v %v", quota, err)
	}
	quota, err = env.store.SetOwnerTransferLimit(ctx, source.User.ID, other.BrandCloud.ID, 2)
	if err != nil || quota.Used != 1 || quota.Remaining != 1 {
		t.Fatalf("raising the limit=%+v %v", quota, err)
	}
}

func TestAcceptedOwnerTransferCompletesAfterLimitIsLowered(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	_, ack, _ := readyCommitFixture(t, env)
	quota, err := env.store.SetOwnerTransferLimit(ctx, ack.SourceUserID, ack.CloudID, 0)
	if err != nil || quota.Used != 0 || quota.Remaining != 0 {
		t.Fatalf("lower accepted quota=%+v %v", quota, err)
	}
	if _, err := env.store.CommitOwnerHandoff(ctx, ack.CloudID, ack.OperationID); err != nil {
		t.Fatalf("accepted transfer must finish: %v", err)
	}
	quota, err = env.store.GetOwnerTransferQuota(ctx, ack.CloudID)
	if err != nil || quota.Used != 1 || quota.Remaining != 0 {
		t.Fatalf("committed quota=%+v %v", quota, err)
	}
	if _, err := env.store.CommitOwnerHandoff(ctx, ack.CloudID, ack.OperationID); err != nil {
		t.Fatalf("commit replay: %v", err)
	}
	quota, err = env.store.GetOwnerTransferQuota(ctx, ack.CloudID)
	if err != nil || quota.Used != 1 {
		t.Fatalf("replay double counted: %+v %v", quota, err)
	}
	cloud, err := env.store.GetManagedBrandCloud(ctx, ack.TargetUserID, ack.CloudID)
	if err != nil || cloud.Limit != 0 || cloud.Used != 1 || cloud.Remaining != 0 {
		t.Fatalf("managed projection=%+v %v", cloud.OwnerTransferQuota, err)
	}
}

func TestConcurrentOwnerTransferAcceptanceCannotStartTwoHandoffs(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	configureTestHandoff(t, env)
	source := handoffDeveloper(t, env, "concurrent-limit-source")
	first := handoffDeveloper(t, env, "concurrent-limit-first")
	second := handoffDeveloper(t, env, "concurrent-limit-second")
	if _, err := env.store.SetOwnerTransferLimit(ctx, source.User.ID, source.BrandCloud.ID, 1); err != nil {
		t.Fatal(err)
	}
	handoffRequest(t, env, source, first, "concurrent-limit-first-token")
	handoffRequest(t, env, source, second, "concurrent-limit-second-token")
	type attempt struct {
		err error
	}
	results := make(chan attempt, 2)
	var wg sync.WaitGroup
	for _, invite := range []struct{ user, token string }{{first.User.ID, "concurrent-limit-first-token"}, {second.User.ID, "concurrent-limit-second-token"}} {
		wg.Add(1)
		go func(user, token string) {
			defer wg.Done()
			_, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, user, token, time.Now())
			results <- attempt{err: err}
		}(invite.user, invite.token)
	}
	wg.Wait()
	close(results)
	successes := 0
	for result := range results {
		if result.err == nil {
			successes++
		} else if !errors.Is(result.err, ErrConflict) && !errors.Is(result.err, ErrNotFound) {
			t.Fatalf("concurrent acceptance: %v", result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("accepted %d concurrent invitations, want 1", successes)
	}
	var handoffs int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM cloud_ownership_handoffs WHERE brand_cloud_id=$1`, source.BrandCloud.ID).Scan(&handoffs); err != nil || handoffs != 1 {
		t.Fatalf("handoffs=%d err=%v", handoffs, err)
	}
	quota, err := env.store.GetOwnerTransferQuota(ctx, source.BrandCloud.ID)
	if err != nil || quota.Used != 0 || quota.Remaining != 1 {
		t.Fatalf("acceptance consumed quota before commit: %+v %v", quota, err)
	}
}
