package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"rtk_account_manager/internal/billinghandoff"
)

type workerTestBilling struct {
	*commitTestBilling
	abortMu          sync.Mutex
	aborted          bool
	lastCancellation string
}

func (b *workerTestBilling) Prepare(_ context.Context, in billinghandoff.Binding) (billinghandoff.Prepared, error) {
	return billinghandoff.Prepared{ID: in.OperationID, AccountID: in.CloudID, SourceUserID: in.SourceUserID, TargetUserID: in.TargetUserID, OwnershipVersion: in.OwnershipVersion, Cutoff: in.Cutoff, Phase: "preparing", Version: 1, CreatedAt: in.Cutoff}, nil
}
func (b *workerTestBilling) Abort(_ context.Context, in billinghandoff.Binding, cancelID, authID, digest string) (billinghandoff.ProtocolAck, error) {
	b.abortMu.Lock()
	defer b.abortMu.Unlock()
	if b.lastCancellation != "" && b.lastCancellation != cancelID {
		return billinghandoff.ProtocolAck{}, ErrConflict
	}
	b.lastCancellation = cancelID
	phase := "abort_pending"
	if b.aborted {
		phase = "aborted"
	}
	return billinghandoff.ProtocolAck{OperationID: in.OperationID, Phase: phase}, nil
}

type workerTestParticipant struct {
	billing  *workerTestBilling
	badScope bool
}

func (p workerTestParticipant) Prepare(_ context.Context, in billinghandoff.Binding) (HandoffPrepareAck, error) {
	ack := HandoffPrepareAck{CloudID: in.CloudID, OperationID: in.OperationID, SourceUserID: in.SourceUserID, TargetUserID: in.TargetUserID, OwnershipVersion: in.OwnershipVersion, Cutoff: in.Cutoff, Participant: "test_resources", HoldReceiptSHA256: strings.Repeat("a", 64), DrainCheckpointSHA256: strings.Repeat("b", 64)}
	if p.badScope {
		ack.CloudID = in.OperationID
	}
	return ack, nil
}
func (p workerTestParticipant) Abort(_ context.Context, d HandoffCanceledDecision) (HandoffAbortAck, error) {
	p.billing.abortMu.Lock()
	p.billing.aborted = true
	p.billing.abortMu.Unlock()
	return HandoffAbortAck{CloudID: d.Binding.CloudID, OperationID: d.Binding.OperationID, OwnershipVersion: d.Binding.OwnershipVersion, Participant: "test_resources", ReceiptSHA256: strings.Repeat("c", 64)}, nil
}
func (p workerTestParticipant) Release(_ context.Context, d HandoffCommittedDecision) (HandoffFinalizationAck, error) {
	return HandoffFinalizationAck{CloudID: d.Binding.CloudID, OperationID: d.Binding.OperationID, OwnershipVersion: d.Binding.OwnershipVersion, DecisionSHA256: d.DecisionSHA256, Participant: "test_resources", ReceiptSHA256: strings.Repeat("d", 64)}, nil
}

func claimOneHandoff(t *testing.T, s *Store) HandoffJob {
	t.Helper()
	jobs, err := s.ClaimHandoffJobs(context.Background(), 1, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim jobs %+v %v", jobs, err)
	}
	return jobs[0]
}
func advanceOneHandoff(t *testing.T, s *Store) HandoffStep {
	t.Helper()
	job := claimOneHandoff(t, s)
	step, err := s.AdvanceHandoffJob(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := s.FinishHandoffJob(context.Background(), job, step.Outcome, step.Retry, 0); err != nil || !applied {
		t.Fatalf("finish: %t %v", applied, err)
	}
	return step
}

func TestHandoffJobLeaseRecoveryAndNewWakeWinsOverOldBackoff(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	ack, q := preparedAckFixture(t, env)
	first := claimOneHandoff(t, env.store)
	if jobs, err := env.store.ClaimHandoffJobs(ctx, 10, time.Minute); err != nil || len(jobs) != 0 {
		t.Fatalf("active lease reclaimed: %+v %v", jobs, err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE cloud_handoff_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE operation_id=$1`, ack.OperationID); err != nil {
		t.Fatal(err)
	}
	reclaimed := claimOneHandoff(t, New(env.db))
	if reclaimed.LeaseID == first.LeaseID {
		t.Fatal("lease token reused")
	}
	if applied, err := env.store.FinishHandoffJob(ctx, first, "complete", false, 0); err != nil || applied {
		t.Fatalf("stale lease changed job: %t %v", applied, err)
	}
	if _, err := env.store.AdvanceHandoffJob(ctx, first); !errors.Is(err, ErrHandoffLeaseLost) {
		t.Fatalf("expired worker advanced: %v", err)
	}
	q.RequesterID = ack.SourceUserID
	if _, err := env.store.CancelBrandCloudOwnerTransfer(ctx, q, time.Now()); err != nil {
		t.Fatal(err)
	}
	if applied, err := env.store.FinishHandoffJob(ctx, reclaimed, "evidence_unavailable", true, time.Hour); err != nil || !applied {
		t.Fatal("finish old phase", err)
	}
	latest := claimOneHandoff(t, env.store)
	if latest.Phase != "canceling" || latest.Generation <= reclaimed.Generation {
		t.Fatalf("lost cancellation wake: %+v", latest)
	}
	assertHandoffOwner(t, env, ack, ack.SourceUserID, "canceling", 1)
}

func TestHandoffJobConcurrentClaimsArePartitioned(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	preparedAckFixture(t, env)
	results := make(chan []HandoffJob, 6)
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		go func() {
			jobs, err := env.store.ClaimHandoffJobs(context.Background(), 1, time.Minute)
			results <- jobs
			errs <- err
		}()
	}
	total := 0
	for i := 0; i < 6; i++ {
		total += len(<-results)
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if total != 1 {
		t.Fatalf("claimed %d copies", total)
	}
}

func TestHandoffWorkerPreparationConsentCommitAndFinalize(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	ack, q := preparedAckFixture(t, env)
	remote := &workerTestBilling{commitTestBilling: &commitTestBilling{balanceTestBilling: &balanceTestBilling{version: 2, confirmed: map[string]bool{}}}}
	if err := env.store.ConfigureHandoffBilling(remote); err != nil {
		t.Fatal(err)
	}
	if step := advanceOneHandoff(t, env.store); step.Outcome != "participant_unavailable" {
		t.Fatalf("missing participant guessed success: %+v", step)
	}
	if err := env.store.ConfigureHandoffParticipants(map[string]HandoffParticipant{"test_resources": workerTestParticipant{billing: remote, badScope: true}}); err != nil {
		t.Fatal(err)
	}
	if step := advanceOneHandoff(t, env.store); step.Outcome != "lifecycle_conflict" {
		t.Fatalf("cross-scope receipt accepted: %+v", step)
	}
	if err := env.store.ConfigureHandoffParticipants(map[string]HandoffParticipant{"test_resources": workerTestParticipant{billing: remote}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if step := advanceOneHandoff(t, env.store); step.Outcome != "progress" {
			t.Fatalf("prepare progress: %+v", step)
		}
	}
	if step := advanceOneHandoff(t, env.store); step.Outcome != "waiting_confirmation" {
		t.Fatalf("worker invented consent: %+v", step)
	}
	view, err := env.store.PreviewOwnerHandoff(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	q.RequesterID = ack.SourceUserID
	confirmBalance(t, env, q, *view.BalanceSnapshot, "source-worker")
	// The user accepted this intent, but the remote acknowledgment was lost.
	q.RequesterID = ack.TargetUserID
	remote.loseReply = true
	if _, err := env.store.ConfirmOwnerHandoff(ctx, HandoffConfirmationInput{Query: q, Snapshot: *view.BalanceSnapshot, IdempotencyKey: "target-lost"}); err == nil {
		t.Fatal("lost reply ignored")
	}
	if step := advanceOneHandoff(t, env.store); step.Outcome != "progress" {
		t.Fatalf("intent replay: %+v", step)
	}
	remote.loseGrant = true
	if step := advanceOneHandoff(t, env.store); !step.Retry {
		t.Fatalf("lost grant not retried: %+v", step)
	}
	assertHandoffOwner(t, env, ack, ack.SourceUserID, "committing", 1)
	// Process restart continues the same persisted request, not a second transfer.
	restarted := New(env.db)
	restarted.ConfigureHandoffBilling(remote)
	restarted.ConfigureHandoffParticipants(map[string]HandoffParticipant{"test_resources": workerTestParticipant{billing: remote}})
	if step := advanceOneHandoff(t, restarted); step.Outcome != "progress" {
		t.Fatalf("commit retry: %+v", step)
	}
	assertHandoffOwner(t, env, ack, ack.TargetUserID, "finalizing", 2)
	remote.loseFinalize = true
	if step := advanceOneHandoff(t, restarted); !step.Retry {
		t.Fatalf("lost finalize not retried: %+v", step)
	}
	for i := 0; i < 2; i++ {
		if step := advanceOneHandoff(t, restarted); step.Outcome != "progress" {
			t.Fatalf("finalization progress: %+v", step)
		}
	}
	assertHandoffOwner(t, env, ack, ack.TargetUserID, "succeeded", 2)
	if jobs, err := restarted.ClaimHandoffJobs(ctx, 10, time.Minute); err != nil || len(jobs) != 0 {
		t.Fatalf("terminal handoff rescheduled: %+v %v", jobs, err)
	}
}

func TestHandoffWorkerCancellationRequiresAllReleaseReceipts(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	b, ack, q := readyCommitFixture(t, env)
	remote := &workerTestBilling{commitTestBilling: b}
	env.store.ConfigureHandoffBilling(remote)
	env.store.ConfigureHandoffParticipants(map[string]HandoffParticipant{"test_resources": workerTestParticipant{billing: remote}})
	remote.loseGrant = true
	if _, err := env.store.CommitOwnerHandoff(ctx, ack.CloudID, ack.OperationID); !errors.Is(err, ErrHandoffUnavailable) {
		t.Fatal("lost grant fixture", err)
	}
	q.RequesterID = ack.SourceUserID
	if _, err := env.store.CancelBrandCloudOwnerTransfer(ctx, q, time.Now()); err != nil {
		t.Fatal(err)
	}
	d, err := env.store.GetHandoffCanceledDecision(ctx, ack.CloudID, ack.OperationID)
	if err != nil || d.AuthorizationID == "" {
		t.Fatalf("cancellation lost attempted grant: %+v %v", d, err)
	}
	if step := advanceOneHandoff(t, env.store); step.Outcome != "progress" {
		t.Fatalf("cancel producers: %+v", step)
	}
	assertHandoffOwner(t, env, ack, ack.SourceUserID, "canceling", 1)
	advanceOneHandoff(t, env.store)
	assertHandoffOwner(t, env, ack, ack.SourceUserID, "canceled", 1)
	if replay, err := New(env.db).GetHandoffCanceledDecision(ctx, ack.CloudID, ack.OperationID); err != nil || replay.DecisionSHA256 != d.DecisionSHA256 {
		t.Fatalf("cancel decision replay: %+v %v", replay, err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE cloud_handoff_canceled_decisions SET decision_sha256=repeat('e',64) WHERE operation_id=$1`, ack.OperationID); err == nil {
		t.Fatal("canceled decision mutable")
	}
}
