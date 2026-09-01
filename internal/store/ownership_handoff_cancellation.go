package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/billinghandoff"
)

type HandoffCanceledDecision struct {
	Binding                                         billinghandoff.Binding
	CancellationID, AuthorizationID, DecisionSHA256 string
	CanceledAt                                      time.Time
}

func loadHandoffCancellation(ctx context.Context, q handoffQuerier, cloud, operation string) (HandoffCanceledDecision, error) {
	var d HandoffCanceledDecision
	err := q.QueryRow(ctx, `SELECT h.brand_cloud_id::text,h.id::text,h.source_user_id::text,h.target_user_id::text,h.ownership_version,h.cutoff,
		c.cancellation_id::text,COALESCE(c.authorization_id::text,''),c.canceled_at,c.decision_sha256
		FROM cloud_ownership_handoffs h JOIN cloud_handoff_canceled_decisions c ON c.operation_id=h.id
		WHERE h.id::text=$1 AND h.brand_cloud_id::text=$2`, operation, cloud).Scan(&d.Binding.CloudID, &d.Binding.OperationID, &d.Binding.SourceUserID, &d.Binding.TargetUserID,
		&d.Binding.OwnershipVersion, &d.Binding.Cutoff, &d.CancellationID, &d.AuthorizationID, &d.CanceledAt, &d.DecisionSHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return d, err
}

// Call only under ordered operation locks. Existing pre-059 canceling operations
// are recoverable from their durable phase, never inferred from a timeout.
func ensureHandoffCancellationTx(ctx context.Context, tx pgx.Tx, cloud, operation string) (HandoffCanceledDecision, error) {
	if d, err := loadHandoffCancellation(ctx, tx, cloud, operation); err == nil {
		return d, nil
	} else if !errors.Is(err, ErrNotFound) {
		return d, err
	}
	var d HandoffCanceledDecision
	err := tx.QueryRow(ctx, `SELECT h.brand_cloud_id::text,h.id::text,h.source_user_id::text,h.target_user_id::text,h.ownership_version,h.cutoff,
		gen_random_uuid()::text,COALESCE(r.authorization_id::text,''),h.updated_at FROM cloud_ownership_handoffs h
		LEFT JOIN cloud_handoff_commit_requests r ON r.operation_id=h.id WHERE h.id=$1 AND h.brand_cloud_id=$2 AND h.phase='canceling'
		AND NOT EXISTS(SELECT 1 FROM cloud_handoff_committed_decisions WHERE operation_id=h.id)`, operation, cloud).
		Scan(&d.Binding.CloudID, &d.Binding.OperationID, &d.Binding.SourceUserID, &d.Binding.TargetUserID, &d.Binding.OwnershipVersion, &d.Binding.Cutoff, &d.CancellationID, &d.AuthorizationID, &d.CanceledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrConflict
	}
	if err != nil {
		return d, err
	}
	d.DecisionSHA256 = handoffDigest(d)
	if _, err = tx.Exec(ctx, `INSERT INTO cloud_handoff_canceled_decisions(operation_id,cancellation_id,authorization_id,canceled_at,decision_sha256) VALUES($1,$2,NULLIF($3,'')::uuid,$4,$5)`, operation, d.CancellationID, d.AuthorizationID, d.CanceledAt, d.DecisionSHA256); err != nil {
		return d, err
	}
	if err = createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_owner_transfer_cancellation_decided", OrganizationID: &cloud, SubjectType: "brand_cloud_owner_transfer", SubjectID: operation,
		Payload: map[string]any{"cancellation_id": d.CancellationID, "authorization_id": d.AuthorizationID, "decision_sha256": d.DecisionSHA256}}); err != nil {
		return d, err
	}
	return d, nil
}

func (s *Store) GetHandoffCanceledDecision(ctx context.Context, cloud, operation string) (HandoffCanceledDecision, error) {
	tx, view, err := s.lockHandoffOperation(ctx, cloud, operation)
	if err != nil {
		return HandoffCanceledDecision{}, err
	}
	defer tx.Rollback(ctx)
	if view.OperationPhase != "canceling" && view.OperationPhase != "canceled" {
		return HandoffCanceledDecision{}, ErrConflict
	}
	d, err := ensureHandoffCancellationTx(ctx, tx, cloud, operation)
	if err != nil {
		return d, err
	}
	return d, tx.Commit(ctx)
}

// Abort success means only abort_pending until Billing independently certifies
// release. An HTTP 200 or local timeout never creates an AM release receipt.
func (s *Store) AdvanceBillingHandoffAbort(ctx context.Context, cloud, operation string) (string, error) {
	d, err := s.GetHandoffCanceledDecision(ctx, cloud, operation)
	if err != nil {
		return "", err
	}
	remote, ok := s.handoffBilling.(interface {
		Prepare(context.Context, billinghandoff.Binding) (billinghandoff.Prepared, error)
		Abort(context.Context, billinghandoff.Binding, string, string, string) (billinghandoff.ProtocolAck, error)
	})
	if !ok {
		return "", ErrHandoffUnavailable
	}
	// Cancellation may beat the first prepare delivery. Create/replay the same
	// durable remote operation before aborting; never treat 404 as released, and
	// never allow a late prepare to create a new hold after cancellation.
	prepared, err := remote.Prepare(ctx, d.Binding)
	if err != nil || prepared.ID != operation || prepared.SourceUserID != d.Binding.SourceUserID || prepared.TargetUserID != d.Binding.TargetUserID || prepared.OwnershipVersion != d.Binding.OwnershipVersion || !prepared.Cutoff.Equal(d.Binding.Cutoff) {
		return "", ErrHandoffUnavailable
	}
	ack, err := remote.Abort(ctx, d.Binding, d.CancellationID, d.AuthorizationID, d.DecisionSHA256)
	if err != nil || ack.OperationID != operation || (ack.Phase != "abort_pending" && ack.Phase != "aborted") {
		return "", ErrHandoffUnavailable
	}
	if ack.Phase == "abort_pending" {
		return "canceling", nil
	}
	return s.RecordCloudHandoffAbortAck(ctx, HandoffAbortAck{CloudID: cloud, OperationID: operation, OwnershipVersion: d.Binding.OwnershipVersion, Participant: "billing",
		ReceiptSHA256: handoffDigest(struct {
			Decision string
			Ack      billinghandoff.ProtocolAck
		}{d.DecisionSHA256, ack})})
}
