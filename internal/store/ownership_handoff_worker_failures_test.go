package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/billinghandoff"
)

type workerFailureParticipant struct {
	workerTestParticipant
	failure string
}

func (p workerFailureParticipant) Prepare(ctx context.Context, in billinghandoff.Binding) (HandoffPrepareAck, error) {
	if p.failure == "unavailable" {
		return HandoffPrepareAck{}, errors.New("producer is draining")
	}
	out, err := p.workerTestParticipant.Prepare(ctx, in)
	if p.failure == "invalid receipt" {
		out.DrainCheckpointSHA256 = ""
	}
	return out, err
}
func (p workerFailureParticipant) Abort(ctx context.Context, in HandoffCanceledDecision) (HandoffAbortAck, error) {
	if p.failure == "unavailable" {
		return HandoffAbortAck{}, errors.New("producer release unavailable")
	}
	out, err := p.workerTestParticipant.Abort(ctx, in)
	if p.failure == "wrong scope" {
		out.OwnershipVersion++
	}
	if p.failure == "invalid receipt" {
		out.ReceiptSHA256 = ""
	}
	return out, err
}
func (p workerFailureParticipant) Release(ctx context.Context, in HandoffCommittedDecision) (HandoffFinalizationAck, error) {
	if p.failure == "unavailable" {
		return HandoffFinalizationAck{}, errors.New("producer release unavailable")
	}
	out, err := p.workerTestParticipant.Release(ctx, in)
	if p.failure == "wrong scope" {
		out.DecisionSHA256 = "another decision"
	}
	if p.failure == "invalid receipt" {
		out.ReceiptSHA256 = ""
	}
	return out, err
}

func TestHandoffWorkerDoesNotReleaseOnUnprovenParticipantResponses(t *testing.T) {
	for _, phase := range []string{"preparing", "canceling", "finalizing"} {
		t.Run(phase, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			var ack HandoffPrepareAck
			var query BrandCloudOwnerTransferQuery
			var billing *commitTestBilling
			if phase == "finalizing" {
				billing, ack, query = readyCommitFixture(t, env)
			} else {
				ack, query = preparedAckFixture(t, env)
				billing = &commitTestBilling{balanceTestBilling: &balanceTestBilling{version: 2, confirmed: map[string]bool{}}}
			}
			remote := &workerTestBilling{commitTestBilling: billing}
			if err := env.store.ConfigureHandoffBilling(remote); err != nil {
				t.Fatal(err)
			}
			owner, version := ack.SourceUserID, int64(1)
			if phase == "canceling" {
				query.RequesterID = ack.SourceUserID
				if _, err := env.store.CancelBrandCloudOwnerTransfer(ctx, query, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "finalizing" {
				if _, err := env.store.CommitOwnerHandoff(ctx, ack.CloudID, ack.OperationID); err != nil {
					t.Fatal(err)
				}
				if _, err := env.store.FinalizeOwnerHandoff(ctx, ack.CloudID, ack.OperationID); err != nil {
					t.Fatal(err)
				}
				owner, version = ack.TargetUserID, 2
			}
			for _, failure := range []string{"missing adapter", "unavailable", "wrong scope", "invalid receipt"} {
				t.Run(failure, func(t *testing.T) {
					adapters := map[string]HandoffParticipant{}
					if failure != "missing adapter" {
						adapters["test_resources"] = workerFailureParticipant{workerTestParticipant: workerTestParticipant{billing: remote, badScope: failure == "wrong scope"}, failure: failure}
					}
					if err := env.store.ConfigureHandoffParticipants(adapters); err != nil {
						t.Fatal(err)
					}
					step := advanceOneHandoff(t, env.store)
					want := "lifecycle_conflict"
					if failure == "missing adapter" {
						want = "participant_unavailable"
					}
					if failure == "unavailable" {
						want = "waiting_release"
						if phase == "preparing" {
							want = "waiting_preparation"
						}
					}
					if step.Outcome != want || !step.Retry {
						t.Fatalf("unproven response progressed: %+v, want %s retry", step, want)
					}
					assertHandoffOwner(t, env, ack, owner, phase, version)
					var permitted bool
					if err := env.db.QueryRow(ctx, `SELECT user_can_access_brand_cloud($1,$2)`, owner, ack.CloudID).Scan(&permitted); err != nil || permitted {
						t.Fatalf("unproven response released fence: %t %v", permitted, err)
					}
				})
			}
		})
	}
}

type workerFailureBilling struct {
	*workerTestBilling
	failure string
}

func (b workerFailureBilling) Prepare(ctx context.Context, in billinghandoff.Binding) (billinghandoff.Prepared, error) {
	if b.failure == "prepare unavailable" {
		return billinghandoff.Prepared{}, ErrHandoffUnavailable
	}
	out, err := b.workerTestBilling.Prepare(ctx, in)
	if b.failure == "prepare wrong scope" {
		out.TargetUserID = in.SourceUserID
	}
	return out, err
}
func (b workerFailureBilling) Settlement(ctx context.Context, in billinghandoff.Binding) (billinghandoff.Settlement, error) {
	if b.failure == "settlement unavailable" {
		return billinghandoff.Settlement{}, ErrHandoffUnavailable
	}
	out, err := b.workerTestBilling.Settlement(ctx, in)
	switch b.failure {
	case "negative balance":
		out.Snapshot = nil
		out.Blockers = []string{"balance_negative"}
	case "unsettled usage":
		out.Snapshot = nil
		out.Blockers = []string{"usage_unsettled"}
	case "wrong operation":
		out.OperationID = "another"
	case "wrong currency":
		out.Snapshot.Currency = "USD"
	case "wrong cutoff":
		out.Snapshot.Cutoff = out.Snapshot.Cutoff.Add(time.Second)
	case "old snapshot":
		out.Snapshot.Version = 1
	}
	return out, err
}

func TestHandoffWorkerRejectsUnavailableOrUnboundSettlement(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	ack, _ := preparedAckFixture(t, env)
	ack.Participant = "test_resources"
	if _, err := env.store.RecordCloudHandoffPrepareAck(ctx, ack); err != nil {
		t.Fatal(err)
	}
	remote := &workerTestBilling{commitTestBilling: &commitTestBilling{balanceTestBilling: &balanceTestBilling{version: 2, confirmed: map[string]bool{}}}}
	for _, failure := range []string{"prepare unavailable", "prepare wrong scope", "settlement unavailable", "negative balance", "unsettled usage", "wrong operation", "wrong currency", "wrong cutoff", "old snapshot"} {
		t.Run(failure, func(t *testing.T) {
			if err := env.store.ConfigureHandoffBilling(workerFailureBilling{workerTestBilling: remote, failure: failure}); err != nil {
				t.Fatal(err)
			}
			step := advanceOneHandoff(t, env.store)
			want := "evidence_unavailable"
			if failure == "negative balance" {
				want = "balance_negative"
			}
			if failure == "unsettled usage" {
				want = "waiting_preparation"
			}
			if step.Outcome != want {
				t.Fatalf("invalid settlement progressed: %+v want %s", step, want)
			}
			assertHandoffOwner(t, env, ack, ack.SourceUserID, "preparing", 1)
			var count int
			if err := env.db.QueryRow(ctx, `SELECT count(*) FROM cloud_handoff_prepare_acknowledgments WHERE operation_id=$1 AND participant='billing'`, ack.OperationID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("invalid settlement persisted: %d %v", count, err)
			}
		})
	}
}
