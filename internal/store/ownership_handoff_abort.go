package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/model"
)

func (s *Store) cancelOwnerHandoff(ctx context.Context, in BrandCloudOwnerTransferQuery, now time.Time) (model.BrandCloudOwnerTransfer, error) {
	transfer, err := s.GetBrandCloudOwnerTransfer(ctx, in, now)
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	if transfer.RequestedByUserID != in.RequesterID {
		return model.BrandCloudOwnerTransfer{}, ErrNotFound
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockBrandCloudCollaborationTx(ctx, tx, in.BrandCloudID, transfer.RequestedByUserID, transfer.TargetUserID); err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	transfer, err = scanBrandCloudOwnerTransfer(tx.QueryRow(ctx, `SELECT `+transferColumns+` FROM brand_cloud_owner_transfers WHERE id=$1 FOR UPDATE`, in.TransferID))
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	var eligible bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id::text=$1 AND disabled_at IS NULL AND email_verified AND NOT signup_pending_verification)`, in.RequesterID).Scan(&eligible); err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	if !eligible {
		return model.BrandCloudOwnerTransfer{}, ErrNotFound
	}
	transfer, err = hydrateHandoff(ctx, tx, transfer)
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	switch {
	case transfer.Status == "canceled" || transfer.OperationPhase == "canceling":
		return transfer, nil
	case transfer.Status == "pending" && transfer.ExpiresAt.After(now):
		transfer, err = scanBrandCloudOwnerTransfer(tx.QueryRow(ctx, `UPDATE brand_cloud_owner_transfers SET status='canceled',canceled_at=$2 WHERE id=$1 RETURNING `+transferColumns, in.TransferID, now))
		if err != nil {
			return model.BrandCloudOwnerTransfer{}, err
		}
	case transfer.Status == "accepted" && transfer.OperationPhase == "preparing":
		if _, err := tx.Exec(ctx, `UPDATE cloud_ownership_handoffs SET phase='canceling',version=version+1,updated_at=now() WHERE id=$1`, in.TransferID); err != nil {
			return model.BrandCloudOwnerTransfer{}, err
		}
		if err := enqueueHandoffCommands(ctx, tx, in.TransferID, "abort"); err != nil {
			return model.BrandCloudOwnerTransfer{}, err
		}
		transfer.OperationPhase = "canceling"
	default:
		return model.BrandCloudOwnerTransfer{}, ErrConflict
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_owner_transfer_cancel_requested", ActorUserID: &in.RequesterID, OrganizationID: &in.BrandCloudID, SubjectType: "brand_cloud_owner_transfer", SubjectID: in.TransferID, Payload: map[string]any{"phase": transfer.OperationPhase}}); err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	return hydrateHandoff(ctx, s.db, transfer)
}

type HandoffAbortAck struct {
	CloudID, OperationID, Participant, ReceiptSHA256 string
	OwnershipVersion                                 int64
}

// Only a separately authenticated participant adapter may call this. A timeout,
// HTTP delivery or a browser cancel response is not proof remote holds released.
func (s *Store) RecordCloudHandoffAbortAck(ctx context.Context, in HandoffAbortAck) (string, error) {
	if !handoffEvidenceDigest.MatchString(in.ReceiptSHA256) {
		return "", ErrConflict
	}
	var source, target string
	err := s.db.QueryRow(ctx, `SELECT source_user_id::text,target_user_id::text FROM cloud_ownership_handoffs WHERE id::text=$1 AND brand_cloud_id::text=$2 AND ownership_version=$3`, in.OperationID, in.CloudID, in.OwnershipVersion).Scan(&source, &target)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if err := lockBrandCloudCollaborationTx(ctx, tx, in.CloudID, source, target); err != nil {
		return "", err
	}
	var phase string
	if err := tx.QueryRow(ctx, `SELECT phase FROM cloud_ownership_handoffs WHERE id=$1 FOR UPDATE`, in.OperationID).Scan(&phase); err != nil {
		return "", err
	}
	var existing string
	err = tx.QueryRow(ctx, `SELECT receipt_sha256 FROM cloud_handoff_abort_acknowledgments WHERE operation_id=$1 AND participant=$2`, in.OperationID, in.Participant).Scan(&existing)
	if err == nil {
		if existing != in.ReceiptSHA256 {
			return "", ErrConflict
		}
		return phase, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if phase != "canceling" {
		return "", ErrConflict
	}
	var expected bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cloud_handoff_participants WHERE operation_id=$1 AND participant=$2)`, in.OperationID, in.Participant).Scan(&expected); err != nil {
		return "", err
	}
	if !expected {
		return "", ErrNotFound
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cloud_handoff_abort_acknowledgments(operation_id,participant,receipt_sha256) VALUES($1,$2,$3)`, in.OperationID, in.Participant, in.ReceiptSHA256); err != nil {
		return "", err
	}
	var missing bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cloud_handoff_participants p WHERE p.operation_id=$1 AND NOT EXISTS(SELECT 1 FROM cloud_handoff_abort_acknowledgments a WHERE a.operation_id=p.operation_id AND a.participant=p.participant))`, in.OperationID).Scan(&missing); err != nil {
		return "", err
	}
	if !missing {
		if _, err := tx.Exec(ctx, `UPDATE cloud_ownership_handoffs SET phase='canceled',version=version+1,updated_at=now() WHERE id=$1`, in.OperationID); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE brand_cloud_owner_transfers SET status='canceled',canceled_at=now() WHERE id=$1`, in.OperationID); err != nil {
			return "", err
		}
		phase = "canceled"
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_owner_transfer_abort_acknowledged", OrganizationID: &in.CloudID, SubjectType: "brand_cloud_owner_transfer", SubjectID: in.OperationID, Payload: map[string]any{"participant": in.Participant, "receipt_sha256": in.ReceiptSHA256, "phase": phase}}); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return phase, nil
}
