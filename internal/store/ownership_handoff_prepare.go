package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Trusted participant adapter input, never a browser assertion or HTTP success
// flag. The adapter verifies the durable remote hold AND its drained checkpoint
// against this complete scope before calling the store. Financial settlement and
// participant balance confirmations are separate Billing evidence.
type HandoffPrepareAck struct {
	OperationID, CloudID, SourceUserID, TargetUserID      string
	OwnershipVersion                                      int64
	Cutoff                                                time.Time
	Participant, HoldReceiptSHA256, DrainCheckpointSHA256 string
}

// AllParticipantsPrepared is only the preparation evidence gate. It does not
// imply eligible accounts, a confirmable amount, a commit grant or an owner swap.
type CloudHandoffPreparation struct {
	OperationID               string
	OwnershipVersion          int64
	Cutoff                    time.Time
	Phase                     string
	MissingParticipants       []string
	PendingFactoryEnrollments int
	AllParticipantsPrepared   bool
}

func handoffPreparation(ctx context.Context, q handoffQuerier, id string) (CloudHandoffPreparation, error) {
	var out CloudHandoffPreparation
	var hasInventory bool
	err := q.QueryRow(ctx, `SELECT h.id::text,h.ownership_version,h.cutoff,h.phase,
		ARRAY(SELECT p.participant FROM cloud_handoff_participants p
			WHERE p.operation_id=h.id AND NOT EXISTS(SELECT 1 FROM cloud_handoff_prepare_acknowledgments a
				WHERE a.operation_id=p.operation_id AND a.participant=p.participant) ORDER BY p.participant),
		EXISTS(SELECT 1 FROM cloud_handoff_participants WHERE operation_id=h.id AND participant='billing')
		AND EXISTS(SELECT 1 FROM cloud_handoff_participants WHERE operation_id=h.id AND participant<>'billing'),
		(SELECT count(*) FROM factory_enrollment_reservations e JOIN factory_production_runs r ON r.id=e.production_run_id
		 WHERE r.brand_cloud_id=h.brand_cloud_id AND e.status='reserved')
		FROM cloud_ownership_handoffs h WHERE h.id::text=$1`, id).
		Scan(&out.OperationID, &out.OwnershipVersion, &out.Cutoff, &out.Phase, &out.MissingParticipants, &hasInventory, &out.PendingFactoryEnrollments)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	out.AllParticipantsPrepared = err == nil && hasInventory && len(out.MissingParticipants) == 0 && out.PendingFactoryEnrollments == 0 && (out.Phase == "preparing" || out.Phase == "committing")
	return out, err
}

// Participant-only read, including a target who has no membership. Admission
// fencing is intentionally not bypassed for any ordinary cloud/resource route.
func (s *Store) GetCloudHandoffPreparation(ctx context.Context, in BrandCloudOwnerTransferQuery) (CloudHandoffPreparation, error) {
	transfer, err := s.GetBrandCloudOwnerTransfer(ctx, in, time.Now().UTC())
	if err != nil {
		return CloudHandoffPreparation{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CloudHandoffPreparation{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockBrandCloudCollaborationTx(ctx, tx, in.BrandCloudID, transfer.RequestedByUserID, transfer.TargetUserID); err != nil {
		return CloudHandoffPreparation{}, err
	}
	// Recheck after locking: a participant may have been disabled while waiting.
	var eligible bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id::text=$1
		AND disabled_at IS NULL AND email_verified AND NOT signup_pending_verification)`, in.RequesterID).Scan(&eligible); err != nil {
		return CloudHandoffPreparation{}, err
	}
	if !eligible {
		return CloudHandoffPreparation{}, ErrNotFound
	}
	return handoffPreparation(ctx, tx, transfer.ID)
}

func (s *Store) RecordCloudHandoffPrepareAck(ctx context.Context, in HandoffPrepareAck) (CloudHandoffPreparation, error) {
	if in.Cutoff.IsZero() || in.OwnershipVersion < 1 || !handoffParticipantName.MatchString(in.Participant) ||
		!handoffEvidenceDigest.MatchString(in.HoldReceiptSHA256) || !handoffEvidenceDigest.MatchString(in.DrainCheckpointSHA256) {
		return CloudHandoffPreparation{}, ErrConflict
	}
	var source, target string
	err := s.db.QueryRow(ctx, `SELECT source_user_id::text,target_user_id::text FROM cloud_ownership_handoffs
		WHERE id::text=$1 AND brand_cloud_id::text=$2 AND ownership_version=$3
		AND source_user_id::text=$4 AND target_user_id::text=$5 AND cutoff=$6`,
		in.OperationID, in.CloudID, in.OwnershipVersion, in.SourceUserID, in.TargetUserID, in.Cutoff).
		Scan(&source, &target)
	if errors.Is(err, pgx.ErrNoRows) {
		return CloudHandoffPreparation{}, ErrNotFound
	}
	if err != nil {
		return CloudHandoffPreparation{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CloudHandoffPreparation{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockBrandCloudCollaborationTx(ctx, tx, in.CloudID, source, target); err != nil {
		return CloudHandoffPreparation{}, err
	}
	var phase string
	if err := tx.QueryRow(ctx, `SELECT phase FROM cloud_ownership_handoffs WHERE id=$1 FOR UPDATE`, in.OperationID).Scan(&phase); err != nil {
		return CloudHandoffPreparation{}, err
	}
	var hold, checkpoint string
	err = tx.QueryRow(ctx, `SELECT hold_receipt_sha256,drain_checkpoint_sha256
		FROM cloud_handoff_prepare_acknowledgments WHERE operation_id=$1 AND participant=$2`, in.OperationID, in.Participant).
		Scan(&hold, &checkpoint)
	if err == nil {
		if hold != in.HoldReceiptSHA256 || checkpoint != in.DrainCheckpointSHA256 {
			return CloudHandoffPreparation{}, ErrConflict
		}
		return handoffPreparation(ctx, tx, in.OperationID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CloudHandoffPreparation{}, err
	}
	// A late prepare receipt during cancellation is retained, never treated as a
	// release acknowledgment. Terminal cancellation cannot acquire a new hold.
	if phase != "preparing" && phase != "canceling" {
		return CloudHandoffPreparation{}, ErrConflict
	}
	var expected bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cloud_handoff_outbox
		WHERE operation_id=$1 AND participant=$2 AND action='prepare')`, in.OperationID, in.Participant).Scan(&expected); err != nil {
		return CloudHandoffPreparation{}, err
	}
	if !expected {
		return CloudHandoffPreparation{}, ErrNotFound
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cloud_handoff_prepare_acknowledgments
		(operation_id,participant,hold_receipt_sha256,drain_checkpoint_sha256) VALUES($1,$2,$3,$4)`,
		in.OperationID, in.Participant, in.HoldReceiptSHA256, in.DrainCheckpointSHA256); err != nil {
		return CloudHandoffPreparation{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_owner_transfer_prepared_acknowledged", OrganizationID: &in.CloudID,
		SubjectType: "brand_cloud_owner_transfer", SubjectID: in.OperationID, Payload: map[string]any{
			"participant": in.Participant, "ownership_version": in.OwnershipVersion, "cutoff": in.Cutoff,
			"hold_receipt_sha256": in.HoldReceiptSHA256, "drain_checkpoint_sha256": in.DrainCheckpointSHA256,
		}}); err != nil {
		return CloudHandoffPreparation{}, err
	}
	out, err := handoffPreparation(ctx, tx, in.OperationID)
	if err != nil {
		return CloudHandoffPreparation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CloudHandoffPreparation{}, err
	}
	return out, nil
}
