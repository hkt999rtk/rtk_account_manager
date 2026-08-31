package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/billinghandoff"
)

// An adapter must authenticate remote evidence and bind the complete decision.
// No default/no-op adapter is provided; unknown persisted participants hold work.
// Abort must persist a cancellation tombstone even before Prepare arrives; late
// Prepare cannot reacquire a released hold. Release must install the committed
// ownership/authorization boundary and invalidate stale queued access before its
// acknowledgment. Every call must honor context cancellation and bounded I/O.
type HandoffParticipant interface {
	Prepare(context.Context, billinghandoff.Binding) (HandoffPrepareAck, error)
	Abort(context.Context, HandoffCanceledDecision) (HandoffAbortAck, error)
	Release(context.Context, HandoffCommittedDecision) (HandoffFinalizationAck, error)
}

func (s *Store) ConfigureHandoffParticipants(adapters map[string]HandoffParticipant) error {
	copy := make(map[string]HandoffParticipant, len(adapters))
	for name, adapter := range adapters {
		if name == "billing" || !handoffParticipantName.MatchString(name) || adapter == nil {
			return ErrConflict
		}
		copy[name] = adapter
	}
	s.handoffParticipants = copy
	return nil
}

type HandoffStep struct {
	Outcome string
	Retry   bool
}

func handoffStepError(err error) HandoffStep {
	out := HandoffStep{Outcome: "evidence_unavailable", Retry: true}
	switch {
	case errors.Is(err, ErrHandoffBalanceNegative):
		out.Outcome = "balance_negative"
	case errors.Is(err, ErrHandoffFinancialBlocked):
		out.Outcome = "financial_blocked"
	case errors.Is(err, ErrDeveloperCloudLimitExceeded):
		out.Outcome = "quota_exceeded"
	case errors.Is(err, ErrConflict), errors.Is(err, ErrNotFound):
		out.Outcome = "lifecycle_conflict"
	}
	return out
}

func (s *Store) AdvanceHandoffJob(ctx context.Context, job HandoffJob) (HandoffStep, error) {
	if err := s.checkHandoffLease(ctx, job); err != nil {
		return HandoffStep{}, err
	}
	var phase string
	var binding billinghandoff.Binding
	err := s.db.QueryRow(ctx, `SELECT phase,brand_cloud_id::text,id::text,source_user_id::text,target_user_id::text,ownership_version,cutoff
		FROM cloud_ownership_handoffs WHERE id::text=$1 AND brand_cloud_id::text=$2`, job.OperationID, job.CloudID).Scan(&phase, &binding.CloudID, &binding.OperationID, &binding.SourceUserID, &binding.TargetUserID, &binding.OwnershipVersion, &binding.Cutoff)
	if err != nil {
		return HandoffStep{}, err
	}
	switch phase {
	case "canceled", "succeeded":
		return HandoffStep{Outcome: "complete"}, nil
	case "preparing":
		return s.advanceHandoffPreparation(ctx, job, binding)
	case "committing":
		if _, err := s.CommitOwnerHandoff(ctx, job.CloudID, job.OperationID); err != nil {
			return handoffStepError(err), nil
		}
		return HandoffStep{Outcome: "progress"}, nil
	case "finalizing":
		return s.advanceHandoffFinalization(ctx, job)
	case "canceling":
		return s.advanceHandoffCancellation(ctx, job)
	default:
		return HandoffStep{Outcome: "lifecycle_conflict", Retry: true}, nil
	}
}

func (s *Store) missingHandoffParticipants(ctx context.Context, operation, stage string) ([]string, error) {
	table := "cloud_handoff_prepare_acknowledgments"
	if stage == "release" {
		table = "cloud_handoff_finalization_acknowledgments"
	} else if stage == "abort" {
		table = "cloud_handoff_abort_acknowledgments"
	}
	rows, err := s.db.Query(ctx, `SELECT p.participant FROM cloud_handoff_participants p WHERE p.operation_id=$1
		AND NOT EXISTS(SELECT 1 FROM `+table+` a WHERE a.operation_id=p.operation_id AND a.participant=p.participant)
		ORDER BY p.participant='billing' DESC,p.participant`, operation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func (s *Store) advanceHandoffPreparation(ctx context.Context, job HandoffJob, binding billinghandoff.Binding) (HandoffStep, error) {
	missing, err := s.missingHandoffParticipants(ctx, job.OperationID, "prepare")
	if err != nil {
		return HandoffStep{}, err
	}
	// Install Billing's durable monetary hold even while a resource is draining.
	// Prepare success itself is never recorded as drained/settled evidence.
	var billingPrepared *billinghandoff.Prepared
	if len(missing) > 0 && missing[0] == "billing" {
		remote, ok := s.handoffBilling.(interface {
			Prepare(context.Context, billinghandoff.Binding) (billinghandoff.Prepared, error)
		})
		if !ok {
			return HandoffStep{Outcome: "participant_unavailable", Retry: true}, nil
		}
		prepared, err := remote.Prepare(ctx, binding)
		if err != nil {
			return handoffStepError(err), nil
		}
		billingPrepared = &prepared
		if prepared.ID != binding.OperationID || prepared.SourceUserID != binding.SourceUserID || prepared.TargetUserID != binding.TargetUserID || prepared.OwnershipVersion != binding.OwnershipVersion || !prepared.Cutoff.Equal(binding.Cutoff) {
			return handoffStepError(ErrHandoffUnavailable), nil
		}
	}
	for _, name := range missing {
		if name == "billing" {
			continue
		}
		if err := s.checkHandoffLease(ctx, job); err != nil {
			return HandoffStep{}, err
		}
		adapter := s.handoffParticipants[name]
		if adapter == nil {
			return HandoffStep{Outcome: "participant_unavailable", Retry: true}, nil
		}
		ack, err := adapter.Prepare(ctx, binding)
		if err != nil {
			return HandoffStep{Outcome: "waiting_preparation", Retry: true}, nil
		}
		if ack.Participant != name || ack.CloudID != binding.CloudID || ack.OperationID != binding.OperationID || ack.SourceUserID != binding.SourceUserID || ack.TargetUserID != binding.TargetUserID || ack.OwnershipVersion != binding.OwnershipVersion || !ack.Cutoff.Equal(binding.Cutoff) {
			return handoffStepError(ErrConflict), nil
		}
		if _, err := s.RecordCloudHandoffPrepareAck(ctx, ack); err != nil {
			return handoffStepError(err), nil
		}
		return HandoffStep{Outcome: "progress"}, nil // bounded work; next job drains another producer
	}
	if billingPrepared != nil {
		status, err := s.handoffBilling.Settlement(ctx, binding)
		if err != nil {
			return handoffStepError(err), nil
		}
		if status.Snapshot == nil || status.Phase != "prepared" || len(status.Blockers) != 0 {
			for _, code := range status.Blockers {
				if code == "balance_negative" {
					return handoffStepError(ErrHandoffBalanceNegative), nil
				}
			}
			return HandoffStep{Outcome: "waiting_preparation"}, nil
		}
		snap := status.Snapshot
		if status.OperationID != binding.OperationID || snap.Version < 2 || snap.BalanceMinor < 0 || snap.Currency != "TWD" || !snap.Cutoff.Equal(binding.Cutoff) {
			return handoffStepError(ErrHandoffUnavailable), nil
		}
		ack := HandoffPrepareAck{CloudID: binding.CloudID, OperationID: binding.OperationID, SourceUserID: binding.SourceUserID, TargetUserID: binding.TargetUserID, OwnershipVersion: binding.OwnershipVersion, Cutoff: binding.Cutoff, Participant: "billing",
			HoldReceiptSHA256: handoffDigest(struct {
				Binding billinghandoff.Binding
				Account string
			}{binding, billingPrepared.AccountID}),
			DrainCheckpointSHA256: handoffDigest(struct {
				Binding         billinghandoff.Binding
				Version, Amount int64
				Currency        string
			}{binding, snap.Version, snap.BalanceMinor, snap.Currency})}
		if _, err := s.RecordCloudHandoffPrepareAck(ctx, ack); err != nil {
			return handoffStepError(err), nil
		}
		return HandoffStep{Outcome: "progress"}, nil
	}
	// Replay only an original authenticated intent. Never manufacture either
	// participant's consent or retry an intent for an older observed snapshot.
	var input HandoffConfirmationInput
	input.Query = BrandCloudOwnerTransferQuery{BrandCloudID: binding.CloudID, TransferID: binding.OperationID}
	err = s.db.QueryRow(ctx, `SELECT r.user_id::text,r.idempotency_key,s.billing_snapshot_version,s.balance_minor,s.currency
		FROM cloud_handoff_confirmation_requests r JOIN cloud_handoff_billing_snapshots s USING(operation_id,billing_snapshot_version)
		WHERE r.operation_id=$1 AND NOT EXISTS(SELECT 1 FROM cloud_handoff_confirmation_acknowledgments a WHERE a.request_id=r.id)
		AND r.billing_snapshot_version=(SELECT max(billing_snapshot_version) FROM cloud_handoff_billing_snapshots WHERE operation_id=$1)
		ORDER BY r.created_at,r.id LIMIT 1`, binding.OperationID).Scan(&input.Query.RequesterID, &input.IdempotencyKey, &input.Snapshot.BillingSnapshotVersion, &input.Snapshot.BalanceMinor, &input.Snapshot.Currency)
	if err == nil {
		input.Snapshot.OwnershipVersion = binding.OwnershipVersion
		if _, err := s.ConfirmOwnerHandoff(ctx, input); err != nil {
			return handoffStepError(err), nil
		}
		return HandoffStep{Outcome: "progress"}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return HandoffStep{}, err
	}
	var confirmed int
	err = s.db.QueryRow(ctx, `SELECT count(DISTINCT r.user_id) FROM cloud_handoff_confirmation_requests r JOIN cloud_handoff_confirmation_acknowledgments a ON a.request_id=r.id
		WHERE r.operation_id=$1 AND r.billing_snapshot_version=(SELECT max(billing_snapshot_version) FROM cloud_handoff_billing_snapshots WHERE operation_id=$1)`, binding.OperationID).Scan(&confirmed)
	if err != nil {
		return HandoffStep{}, err
	}
	if confirmed != 2 {
		return HandoffStep{Outcome: "waiting_confirmation"}, nil
	}
	if err := s.checkHandoffLease(ctx, job); err != nil {
		return HandoffStep{}, err
	}
	if _, err := s.CommitOwnerHandoff(ctx, binding.CloudID, binding.OperationID); err != nil {
		return handoffStepError(err), nil
	}
	return HandoffStep{Outcome: "progress"}, nil
}

func (s *Store) advanceHandoffFinalization(ctx context.Context, job HandoffJob) (HandoffStep, error) {
	missing, err := s.missingHandoffParticipants(ctx, job.OperationID, "release")
	if err != nil {
		return HandoffStep{}, err
	}
	if len(missing) == 0 {
		return HandoffStep{Outcome: "complete"}, nil
	}
	if missing[0] == "billing" {
		if _, err := s.FinalizeOwnerHandoff(ctx, job.CloudID, job.OperationID); err != nil {
			return handoffStepError(err), nil
		}
		return HandoffStep{Outcome: "progress"}, nil
	}
	d, err := loadHandoffDecision(ctx, s.db, job.CloudID, job.OperationID)
	if err != nil {
		return HandoffStep{}, err
	}
	name := missing[0]
	adapter := s.handoffParticipants[name]
	if adapter == nil {
		return HandoffStep{Outcome: "participant_unavailable", Retry: true}, nil
	}
	ack, err := adapter.Release(ctx, d)
	if err != nil {
		return HandoffStep{Outcome: "waiting_release", Retry: true}, nil
	}
	if ack.Participant != name || ack.CloudID != job.CloudID || ack.OperationID != job.OperationID || ack.OwnershipVersion != d.Binding.OwnershipVersion || ack.DecisionSHA256 != d.DecisionSHA256 {
		return handoffStepError(ErrConflict), nil
	}
	if _, err := s.RecordHandoffFinalizationAck(ctx, ack); err != nil {
		return handoffStepError(err), nil
	}
	return HandoffStep{Outcome: "progress"}, nil
}

func (s *Store) advanceHandoffCancellation(ctx context.Context, job HandoffJob) (HandoffStep, error) {
	d, err := s.GetHandoffCanceledDecision(ctx, job.CloudID, job.OperationID)
	if err != nil {
		return handoffStepError(err), nil
	}
	missing, err := s.missingHandoffParticipants(ctx, job.OperationID, "abort")
	if err != nil {
		return HandoffStep{}, err
	}
	// Request Billing cancellation before releasing producers, but do not wait
	// for Billing's final release receipt before delivering producer cancellations.
	if len(missing) > 0 && missing[0] == "billing" {
		if _, err := s.AdvanceBillingHandoffAbort(ctx, job.CloudID, job.OperationID); err != nil {
			return handoffStepError(err), nil
		}
	}
	for _, name := range missing {
		if name == "billing" {
			continue
		}
		if err := s.checkHandoffLease(ctx, job); err != nil {
			return HandoffStep{}, err
		}
		adapter := s.handoffParticipants[name]
		if adapter == nil {
			return HandoffStep{Outcome: "participant_unavailable", Retry: true}, nil
		}
		ack, err := adapter.Abort(ctx, d)
		if err != nil {
			return HandoffStep{Outcome: "waiting_release", Retry: true}, nil
		}
		if ack.Participant != name || ack.CloudID != job.CloudID || ack.OperationID != job.OperationID || ack.OwnershipVersion != d.Binding.OwnershipVersion {
			return handoffStepError(ErrConflict), nil
		}
		if _, err := s.RecordCloudHandoffAbortAck(ctx, ack); err != nil {
			return handoffStepError(err), nil
		}
		return HandoffStep{Outcome: "progress"}, nil
	}
	return HandoffStep{Outcome: "waiting_release"}, nil
}
