package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/billinghandoff"
)

type CloudDeletionRecoveryBilling interface {
	RetireCloudClose(context.Context, billinghandoff.ClosureBinding, string, string) (billinghandoff.CloseCommandResolution, error)
	CancelCloudClosure(context.Context, billinghandoff.ClosureBinding, string, string) (billinghandoff.ClosureOperation, error)
}
type CloudDeletionRelease struct {
	Binding                                    billinghandoff.ClosureBinding
	CancellationID, Participant, ReceiptSHA256 string
	Released                                   bool
}

// Cancellation is a persistent tombstone at the producer: even if prepare was
// never observed, a delayed prepare must not reacquire a released hold.
type CloudDeletionCancelProducer interface {
	CancelCloudDeletion(context.Context, billinghandoff.ClosureBinding, int64, string, string) (CloudDeletionRelease, error)
}
type deletionCloseAttempt struct{ Receipt, SHA string }

func activeDeletionClose(ctx context.Context, q rowQuerier, operation string) (deletionCloseAttempt, error) {
	var out deletionCloseAttempt
	err := q.QueryRow(ctx, `SELECT settlement_id::text,readiness_sha256 FROM cloud_deletion_close_attempts a WHERE operation_id=$1 AND NOT EXISTS(
        SELECT 1 FROM cloud_deletion_command_retirements r WHERE r.operation_id=a.operation_id AND r.readiness_sha256=a.readiness_sha256) ORDER BY created_at DESC LIMIT 1`, operation).Scan(&out.Receipt, &out.SHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	return out, err
}
func (s *Store) lockDeletionRecovery(ctx context.Context, op CloudDeletionOperation) (pgx.Tx, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, op.OwnerUserID); err == nil {
		_, err = tx.Exec(ctx, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, op.CloudID)
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}
func (s *Store) resolveDeletionClose(ctx context.Context, op CloudDeletionOperation, command deletionCloseAttempt) (bool, error) {
	remote, ok := s.deletion.Billing.(CloudDeletionRecoveryBilling)
	if !ok {
		return false, ErrHandoffUnavailable
	}
	out, err := remote.RetireCloudClose(ctx, op.binding(), command.Receipt, command.SHA)
	if err != nil {
		return false, err
	}
	if out.OperationID != op.ID || out.SettlementID != command.Receipt || out.AMReadinessSHA256 != command.SHA {
		return false, ErrHandoffUnavailable
	}
	if out.Outcome == "closed" {
		if out.Acknowledgment == nil || out.RetiredAt != nil || out.ReceiptSHA256 != "" {
			return false, ErrHandoffUnavailable
		}
		return true, s.completeDeletion(ctx, op, *out.Acknowledgment)
	}
	if out.Outcome != "retired" || out.RetiredAt == nil || out.RetiredAt.IsZero() || !handoffEvidenceDigest.MatchString(out.ReceiptSHA256) || out.Acknowledgment != nil {
		return false, ErrHandoffUnavailable
	}
	tx, err := s.lockDeletionRecovery(ctx, op)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var prior string
	err = tx.QueryRow(ctx, `SELECT receipt_sha256 FROM cloud_deletion_command_retirements WHERE operation_id=$1 AND readiness_sha256=$2`, op.ID, command.SHA).Scan(&prior)
	if err == nil {
		if prior != out.ReceiptSHA256 {
			return false, ErrConflict
		}
		return false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO cloud_deletion_command_retirements(operation_id,readiness_sha256,receipt_sha256,retired_at) VALUES($1,$2,$3,$4)`, op.ID, command.SHA, out.ReceiptSHA256, out.RetiredAt); err != nil {
		return false, err
	}
	if err = deletionAudit(ctx, tx, op, "command_retired", out.ReceiptSHA256); err != nil {
		return false, err
	}
	return false, tx.Commit(ctx)
}

// Authenticated owner intent, not an immediate release. If an already-issued
// close won the race, recovery completes deletion instead of reopening Billing.
// This store command is not exposed as an unauthenticated recovery endpoint.
func (s *Store) RequestCloudDeletionCancellation(ctx context.Context, user, cloud, operation string) (CloudDeletionOperation, error) {
	op, err := s.loadDeletion(ctx, cloud, operation)
	if err != nil {
		return op, err
	}
	if op.OwnerUserID != user {
		return CloudDeletionOperation{}, ErrNotFound
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return op, err
	}
	defer tx.Rollback(ctx)
	ownership, _, err := lockDeletionOwner(ctx, tx, user, cloud)
	if err != nil {
		return op, err
	}
	if ownership != op.OwnershipVersion {
		return op, ErrConflict
	}
	op, err = scanDeletion(tx.QueryRow(ctx, `SELECT `+deletionColumns+` FROM cloud_deletion_operations WHERE id=$1`, op.ID))
	if err != nil {
		return op, err
	}
	if op.Phase == "succeeded" {
		return op, ErrConflict
	}
	if op.Phase == "canceling" || op.Phase == "canceled" {
		return op, nil
	}
	var id string
	if err = tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		return op, err
	}
	sha := handoffDigest(struct {
		Binding        billinghandoff.ClosureBinding
		CancellationID string
	}{op.binding(), id})
	if _, err = tx.Exec(ctx, `INSERT INTO cloud_deletion_cancellations(operation_id,id,decision_sha256) VALUES($1,$2,$3)`, op.ID, id, sha); err != nil {
		return op, err
	}
	op, err = scanDeletion(tx.QueryRow(ctx, `UPDATE cloud_deletion_operations SET phase='canceling',blockers='[]',updated_at=now() WHERE id=$1 RETURNING `+deletionColumns, op.ID))
	if err != nil {
		return op, err
	}
	if _, err = tx.Exec(ctx, `UPDATE cloud_deletion_jobs SET available_at=clock_timestamp() WHERE operation_id=$1`, op.ID); err != nil {
		return op, err
	}
	if err = deletionAudit(ctx, tx, op, "cancellation_requested", sha); err != nil {
		return op, err
	}
	return op, tx.Commit(ctx)
}
func (s *Store) recordDeletionRelease(ctx context.Context, op CloudDeletionOperation, participant, sha string) error {
	if !handoffEvidenceDigest.MatchString(sha) {
		return ErrHandoffUnavailable
	}
	tx, err := s.lockDeletionRecovery(ctx, op)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var prior string
	err = tx.QueryRow(ctx, `SELECT receipt_sha256 FROM cloud_deletion_release_receipts WHERE operation_id=$1 AND participant=$2`, op.ID, participant).Scan(&prior)
	if err == nil {
		if prior != sha {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var valid bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cloud_deletion_operations WHERE id=$1 AND phase='canceling') AND ($2='billing' OR EXISTS(SELECT 1 FROM cloud_deletion_participants WHERE operation_id=$1 AND participant=$2))`, op.ID, participant).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO cloud_deletion_release_receipts(operation_id,participant,receipt_sha256) VALUES($1,$2,$3)`, op.ID, participant, sha); err != nil {
		return err
	}
	if err = deletionAudit(ctx, tx, op, "hold_released", sha); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) advanceDeletionCancellation(ctx context.Context, op CloudDeletionOperation) (CloudDeletionOperation, error) {
	remote, ok := s.deletion.Billing.(CloudDeletionRecoveryBilling)
	if !ok {
		return op, ErrHandoffUnavailable
	}
	command, err := activeDeletionClose(ctx, s.db, op.ID)
	if err == nil {
		closed, err := s.resolveDeletionClose(ctx, op, command)
		if err != nil {
			return op, err
		}
		if closed {
			return s.loadDeletion(ctx, op.CloudID, op.ID)
		}
	} else if !errors.Is(err, ErrNotFound) {
		return op, err
	}
	var id, sha string
	if err = s.db.QueryRow(ctx, `SELECT id::text,decision_sha256 FROM cloud_deletion_cancellations WHERE operation_id=$1`, op.ID).Scan(&id, &sha); err != nil {
		return op, err
	}
	// Idempotent preparation handles cancel-before-first-delivery, but does not
	// manufacture provider cancellation or release evidence.
	if _, err = s.deletion.Billing.PrepareCloudClosure(ctx, op.binding(), op.RequestSHA256); err != nil {
		return op, err
	}
	response, err := remote.CancelCloudClosure(ctx, op.binding(), id, sha)
	if err != nil {
		return op, err
	}
	if response.ID != op.ID || response.OwnerUserID != op.OwnerUserID || response.OwnershipVersion != op.OwnershipVersion || (response.Phase != "canceling" && response.Phase != "canceled") {
		return op, ErrHandoffUnavailable
	}
	rows, err := s.db.Query(ctx, `SELECT p.participant FROM cloud_deletion_participants p WHERE operation_id=$1 AND NOT EXISTS(SELECT 1 FROM cloud_deletion_release_receipts r WHERE r.operation_id=p.operation_id AND r.participant=p.participant) ORDER BY participant`, op.ID)
	if err != nil {
		return op, err
	}
	names := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return op, err
		}
		names = append(names, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return op, err
	}
	for _, name := range names {
		producer, ok := s.deletion.Producers[name].(CloudDeletionCancelProducer)
		if !ok {
			return op, ErrHandoffUnavailable
		}
		out, err := producer.CancelCloudDeletion(ctx, op.binding(), op.AuthorizationVersion+1, id, sha)
		if err != nil {
			return op, err
		}
		if out.Binding.CloudDeletionScope != op.binding().CloudDeletionScope || out.Binding.OperationID != op.ID || !out.Binding.Cutoff.Equal(op.Cutoff) || out.CancellationID != id || out.Participant != name || !out.Released {
			return op, ErrHandoffUnavailable
		}
		if err = s.recordDeletionRelease(ctx, op, name, out.ReceiptSHA256); err != nil {
			return op, err
		}
	}
	if response.Phase != "canceled" {
		return op, ErrHandoffSnapshotNotReady
	}
	// Billing's canceled phase is itself guarded by persisted provider revocation
	// and release acknowledgments. The coordinator cannot write those via HTTP.
	if err = s.recordDeletionRelease(ctx, op, "billing", handoffDigest(struct {
		CancellationID, DecisionSHA string
		Operation                   billinghandoff.ClosureOperation
	}{id, sha, response})); err != nil {
		return op, err
	}
	tx, err := s.lockDeletionRecovery(ctx, op)
	if err != nil {
		return op, err
	}
	defer tx.Rollback(ctx)
	op, err = scanDeletion(tx.QueryRow(ctx, `UPDATE cloud_deletion_operations SET phase='canceled',blockers='[]',updated_at=now() WHERE id=$1 AND phase IN ('canceling','canceled') RETURNING `+deletionColumns, op.ID))
	if err != nil {
		return op, err
	}
	if err = deletionAudit(ctx, tx, op, "canceled", sha); err != nil {
		return op, err
	}
	return op, tx.Commit(ctx)
}
