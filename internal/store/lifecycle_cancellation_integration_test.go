package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/billinghandoff"
)

func TestHandoffCanceledCallsPreserveOwnerDecisionsAndWorkerLease(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	_, ack, query := readyCommitFixture(t, env)
	ctx := context.Background()
	jobs, err := env.store.ClaimHandoffJobs(ctx, 1, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %+v %v", jobs, err)
	}
	job := jobs[0]
	before := lifecycleSnapshot(t, env)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	query.RequesterID = ack.SourceUserID
	sha := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"accept replay", func() error {
			_, err := env.store.AcceptBrandCloudOwnerTransfer(canceled, ack.TargetUserID, "prepare-token", time.Now())
			return err
		}},
		{"cancel", func() error {
			_, err := env.store.CancelBrandCloudOwnerTransfer(canceled, query, time.Now())
			return err
		}},
		{"preview", func() error { _, err := env.store.PreviewOwnerHandoff(canceled, query); return err }},
		{"prepare acknowledgment", func() error { _, err := env.store.RecordCloudHandoffPrepareAck(canceled, ack); return err }},
		{"commit", func() error {
			_, err := env.store.CommitOwnerHandoff(canceled, ack.CloudID, ack.OperationID)
			return err
		}},
		{"finalize", func() error {
			_, err := env.store.FinalizeOwnerHandoff(canceled, ack.CloudID, ack.OperationID)
			return err
		}},
		{"finalization acknowledgment", func() error {
			_, err := env.store.RecordHandoffFinalizationAck(canceled, HandoffFinalizationAck{CloudID: ack.CloudID, OperationID: ack.OperationID, DecisionSHA256: sha, ReceiptSHA256: sha, Participant: "billing", OwnershipVersion: 1})
			return err
		}},
		{"abort acknowledgment", func() error {
			_, err := env.store.RecordCloudHandoffAbortAck(canceled, HandoffAbortAck{CloudID: ack.CloudID, OperationID: ack.OperationID, ReceiptSHA256: sha, Participant: "billing", OwnershipVersion: 1})
			return err
		}},
		{"abort", func() error {
			_, err := env.store.AdvanceBillingHandoffAbort(canceled, ack.CloudID, ack.OperationID)
			return err
		}},
		{"claim jobs", func() error { _, err := env.store.ClaimHandoffJobs(canceled, 1, time.Minute); return err }},
		{"advance job", func() error { _, err := env.store.AdvanceHandoffJob(canceled, job); return err }},
		{"finish job", func() error {
			_, err := env.store.FinishHandoffJob(canceled, job, "evidence_unavailable", true, time.Second)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			if after := lifecycleSnapshot(t, env); after != before {
				t.Fatal("canceled request changed durable lifecycle state")
			}
		})
	}
	assertHandoffOwner(t, env, ack, ack.SourceUserID, "preparing", 1)
	if err = env.store.checkHandoffLease(ctx, job); err != nil {
		t.Fatalf("canceled worker lost lease: %v", err)
	}
}

func TestDeletionCanceledCallsPreserveFenceAndWorkerLease(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "deletion-canceled-context")
	f := newDeletionRecoveryFixture()
	configureDeletionRecovery(t, env.store, f)
	op, err := env.store.RequestDeveloperCloudDeletion(ctx, owner.User.ID, owner.BrandCloud.ID, "canceled-context")
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := env.store.ClaimCloudDeletionJobs(ctx, 1, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %+v %v", jobs, err)
	}
	job := jobs[0]
	hold, err := f.PrepareCloudDeletion(ctx, op.binding(), op.AuthorizationVersion+1)
	if err != nil {
		t.Fatal(err)
	}
	before := lifecycleSnapshot(t, env)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"request", func() error {
			_, err := env.store.RequestDeveloperCloudDeletion(canceled, owner.User.ID, op.CloudID, "canceled-context")
			return err
		}},
		{"preflight", func() error {
			_, err := env.store.PreflightDeveloperBrandCloudDeletion(canceled, owner.User.ID, op.CloudID)
			return err
		}},
		{"cancel", func() error {
			_, err := env.store.RequestCloudDeletionCancellation(canceled, owner.User.ID, op.CloudID, op.ID)
			return err
		}},
		{"advance", func() error { _, err := env.store.AdvanceCloudDeletion(canceled, op.CloudID, op.ID); return err }},
		{"record hold", func() error { return env.store.recordDeletionHold(canceled, op, hold) }},
		{"prepare close", func() error { _, err := env.store.prepareDeletionClose(canceled, op, op.ID); return err }},
		{"record release", func() error {
			return env.store.recordDeletionRelease(canceled, op, "test_resources", strings.Repeat("a", 64))
		}},
		{"complete", func() error {
			return env.store.completeDeletion(canceled, op, billinghandoff.ClosureAcknowledgment{OperationID: op.ID, Phase: "closed", ClosedAt: time.Now(), ReceiptSHA256: strings.Repeat("a", 64)})
		}},
		{"claim job", func() error { _, err := env.store.ClaimCloudDeletionJobs(canceled, 1, time.Minute); return err }},
		{"process job", func() error { _, err := env.store.ProcessCloudDeletionJob(canceled, job); return err }},
		{"finish job", func() error { _, err := env.store.FinishCloudDeletionJob(canceled, job, time.Second); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			if after := lifecycleSnapshot(t, env); after != before {
				t.Fatal("canceled request changed durable lifecycle state")
			}
		})
	}
	var allowed bool
	if err = env.db.QueryRow(ctx, `SELECT user_can_access_brand_cloud($1,$2)`, owner.User.ID, op.CloudID).Scan(&allowed); err != nil || allowed {
		t.Fatalf("canceled request released fence: %t %v", allowed, err)
	}
	if released, err := env.store.FinishCloudDeletionJob(ctx, job, 0); err != nil || !released {
		t.Fatalf("original lease not recoverable: %t %v", released, err)
	}
}

// Capture the durable state, including timestamps, evidence, audit and leases.
// The query only reads the serialized, task-owned integration database.
func lifecycleSnapshot(t *testing.T, env storeIntegrationEnv) string {
	t.Helper()
	var snapshot string
	if err := env.db.QueryRow(context.Background(), `SELECT jsonb_build_object(
	 'owners',(SELECT jsonb_agg(to_jsonb(m) ORDER BY organization_id,user_id) FROM organization_members m),
	 'clouds',(SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM organizations o),
	 'handoffs',(SELECT jsonb_agg(to_jsonb(h) ORDER BY id) FROM cloud_ownership_handoffs h),
	 'handoff_jobs',(SELECT jsonb_agg(to_jsonb(j) ORDER BY operation_id) FROM cloud_handoff_jobs j),
	 'handoff_outbox',(SELECT jsonb_agg(to_jsonb(o) ORDER BY operation_id,participant,action) FROM cloud_handoff_outbox o),
	 'handoff_acks',(SELECT jsonb_agg(to_jsonb(a) ORDER BY operation_id,participant) FROM cloud_handoff_prepare_acknowledgments a),
	 'deletions',(SELECT jsonb_agg(to_jsonb(d) ORDER BY id) FROM cloud_deletion_operations d),
	 'deletion_jobs',(SELECT jsonb_agg(to_jsonb(j) ORDER BY operation_id) FROM cloud_deletion_jobs j),
	 'deletion_holds',(SELECT jsonb_agg(to_jsonb(r) ORDER BY operation_id,participant) FROM cloud_deletion_resource_receipts r),
	 'deletion_releases',(SELECT jsonb_agg(to_jsonb(r) ORDER BY operation_id,participant) FROM cloud_deletion_release_receipts r),
	 'audit',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM audit_events a))::text`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
