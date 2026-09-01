package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCloudDeletionWorkerCancellationWakeBeatsStaleBackoff(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "deletion-wake")
	f := newDeletionRecoveryFixture()
	configureDeletionRecovery(t, env.store, f)
	op, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID, "wake")
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := env.store.ClaimCloudDeletionJobs(ctx, 1, time.Minute)
	if err != nil || len(jobs) != 1 || jobs[0].Attempts != 1 {
		t.Fatalf("claim %+v %v", jobs, err)
	}
	if _, err = env.store.RequestCloudDeletionCancellation(ctx, owner.User.ID, op.CloudID, op.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := env.store.FinishCloudDeletionJob(ctx, jobs[0], time.Hour); err != nil || !ok {
		t.Fatalf("finish %v %v", ok, err)
	}
	next, err := env.store.ClaimCloudDeletionJobs(ctx, 1, time.Minute)
	if err != nil || len(next) != 1 || next[0].Generation <= jobs[0].Generation || next[0].Attempts != 2 {
		t.Fatalf("wake overwritten %+v %v", next, err)
	}
	// Blocker-only changes are not progress and must retain bounded backoff.
	if _, err = env.store.ProcessCloudDeletionJob(ctx, next[0]); err == nil {
		t.Fatal("missing releases should wait")
	}
	if ok, err := env.store.FinishCloudDeletionJob(ctx, next[0], time.Hour); err != nil || !ok {
		t.Fatalf("retry finish %v %v", ok, err)
	}
	if none, err := env.store.ClaimCloudDeletionJobs(ctx, 1, time.Minute); err != nil || len(none) != 0 {
		t.Fatalf("blockers caused hot loop %+v %v", none, err)
	}
}

func TestCloudDeletionRecoveryOnlyCannotAdmitOrInventProducers(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "recovery-only")
	f := newDeletionRecoveryFixture()
	recovery := New(env.db)
	if err := recovery.ConfigureCloudDeletionRecovery(CloudDeletionOptions{Billing: f}); err != nil {
		t.Fatal(err)
	}
	// Even later observer setup must not turn a recovery process into admission.
	if err := recovery.ConfigureCloudDeletionPreflight(CloudDeletionPreflightOptions{Billing: deletionBillingFunc(syntheticDeletionBilling), Resources: map[string]CloudDeletionResourceObserver{"test_resources": deletionResourceFunc(syntheticDeletionResources)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := recovery.RequestDeveloperCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID, "no-admission"); !errors.Is(err, ErrHandoffUnavailable) {
		t.Fatalf("recovery enabled admission %v", err)
	}
	configureDeletionRecovery(t, env.store, f)
	op, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID, "admission")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = recovery.AdvanceCloudDeletion(ctx, op.CloudID, op.ID); !errors.Is(err, ErrHandoffUnavailable) {
		t.Fatalf("missing producer bypassed %v", err)
	}
	var receipts int
	if err = env.db.QueryRow(ctx, `SELECT count(*) FROM cloud_deletion_resource_receipts WHERE operation_id=$1`, op.ID).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("invented receipts %d %v", receipts, err)
	}
	// Complete real persisted preparation with the explicit synthetic producer,
	// then lose the Billing reply. Recovery needs no fresh producer fabrication.
	f.failReply = true
	if _, err = env.store.AdvanceCloudDeletion(ctx, op.CloudID, op.ID); err == nil {
		t.Fatal("expected lost reply")
	}
	op, err = recovery.AdvanceCloudDeletion(ctx, op.CloudID, op.ID)
	if err != nil || op.State != "succeeded" {
		t.Fatalf("fixed-command recovery %+v %v", op, err)
	}
}

func TestCloudDeletionRecoveryConfigurationRejectsPartialProtocol(t *testing.T) {
	for _, opts := range []CloudDeletionOptions{
		{}, {Billing: newDeletionCoordinatorFixture()},
		{Billing: newDeletionRecoveryFixture(), Producers: map[string]CloudDeletionProducer{"billing": newDeletionRecoveryFixture()}},
		{Billing: newDeletionRecoveryFixture(), Producers: map[string]CloudDeletionProducer{"test_resources": newDeletionCoordinatorFixture()}},
	} {
		if err := (&Store{}).ConfigureCloudDeletionRecovery(opts); err == nil {
			t.Fatal("partial recovery config accepted")
		}
	}
}
