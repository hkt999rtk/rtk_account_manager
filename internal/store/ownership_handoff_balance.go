package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/billinghandoff"
	"rtk_account_manager/internal/model"
)

type HandoffBilling interface {
	Settlement(context.Context, billinghandoff.Binding) (billinghandoff.Settlement, error)
	Confirm(context.Context, billinghandoff.Binding, billinghandoff.Confirmation) (billinghandoff.Settlement, error)
}

func (s *Store) ConfigureHandoffBilling(client HandoffBilling) error {
	if client == nil {
		return ErrHandoffUnavailable
	}
	s.handoffBilling = client
	return nil
}

var handoffIdempotencyKey = regexp.MustCompile(`^[!-~]{1,128}$`)

type HandoffConfirmationInput struct {
	Query          BrandCloudOwnerTransferQuery
	IdempotencyKey string
	Snapshot       model.CloudBalanceSnapshot
}

// Stored previews survive outages/cancellation, but stored flags are never
// treated as fresh financial consent. Only a live Billing response supplies them.
func projectHandoffStatus(view model.BrandCloudOwnerTransfer) model.BrandCloudOwnerTransfer {
	view.SourceUserID = view.RequestedByUserID
	view.HasSettledSnapshot = view.BalanceSnapshot != nil
	view.SourceConfirmed, view.TargetConfirmed = nil, nil
	if view.HasSettledSnapshot {
		source, target := false, false
		view.SourceConfirmed, view.TargetConfirmed = &source, &target
	}
	phase := view.Status
	blockers := []model.CloudBlocker{}
	switch view.Status {
	case "pending":
		phase = "awaiting_acceptance"
	case "accepted":
		phase = "preparing"
		if view.OperationPhase == "canceling" {
			phase = "blocked"
			blockers = handoffBlockers("lifecycle_conflict")
		} else if view.OperationPhase == "canceled" {
			phase = "canceled"
		} else if view.OperationPhase == "" {
			phase = "blocked"
			blockers = handoffBlockers("evidence_unavailable")
		} else if view.HasSettledSnapshot {
			phase = "blocked"
			blockers = handoffBlockers("confirmation_stale")
		}
	}
	return handoffView(view, phase, blockers)
}
func handoffBlockers(codes ...string) []model.CloudBlocker {
	out := make([]model.CloudBlocker, 0, len(codes))
	for _, code := range codes {
		out = append(out, model.CloudBlocker{Code: code, Retryable: code != "balance_negative"})
	}
	return out
}
func handoffView(view model.BrandCloudOwnerTransfer, phase string, blockers []model.CloudBlocker) model.BrandCloudOwnerTransfer {
	view.Phase, view.Blockers = phase, blockers
	if view.Operation != nil {
		view.Operation.Phase, view.Operation.Blockers = phase, blockers
		view.Operation.State = "running"
		if phase == "blocked" {
			view.Operation.State = "blocked"
		}
		if phase == "canceled" {
			view.Operation.State = "canceled"
		}
		if view.OperationPhase == "canceling" {
			view.Operation.Phase = "canceling"
		}
	}
	return view
}

func (s *Store) lockHandoffParticipant(ctx context.Context, in BrandCloudOwnerTransferQuery) (pgx.Tx, model.BrandCloudOwnerTransfer, error) {
	view, err := s.GetBrandCloudOwnerTransfer(ctx, in, time.Now().UTC())
	if err != nil {
		return nil, view, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, view, err
	}
	if err = lockBrandCloudCollaborationTx(ctx, tx, in.BrandCloudID, view.RequestedByUserID, view.TargetUserID); err == nil {
		view, err = getHandoffParticipant(ctx, tx, in, time.Now().UTC())
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, view, err
	}
	return tx, view, nil
}

func confirmableHandoff(ctx context.Context, tx pgx.Tx, view model.BrandCloudOwnerTransfer) (billinghandoff.Binding, error) {
	in := billinghandoff.Binding{CloudID: view.BrandCloudID, OperationID: view.ID, SourceUserID: view.RequestedByUserID, TargetUserID: view.TargetUserID, OwnershipVersion: view.OwnershipVersion}
	if view.Status != "accepted" || view.OperationPhase != "preparing" || !view.ExpiresAt.After(time.Now().UTC()) || view.OwnershipVersion < 1 {
		return in, ErrHandoffSnapshotNotReady
	}
	var currentVersion int64
	err := tx.QueryRow(ctx, `SELECT h.cutoff,o.ownership_version FROM cloud_ownership_handoffs h
		JOIN organizations o ON o.id=h.brand_cloud_id JOIN organization_members m ON m.organization_id=o.id AND m.role='owner'
		JOIN users target ON target.id=h.target_user_id
		WHERE h.id=$1 AND m.user_id=h.source_user_id AND user_can_access_brand_cloud_without_handoff(h.source_user_id::text,o.id::text)
		AND target.disabled_at IS NULL AND target.email_verified AND NOT target.signup_pending_verification`, view.ID).Scan(&in.Cutoff, &currentVersion)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && currentVersion != in.OwnershipVersion {
		return in, ErrHandoffSnapshotNotReady
	}
	if err != nil {
		return in, err
	}
	preparation, err := handoffPreparation(ctx, tx, view.ID)
	if err != nil {
		return in, err
	}
	if !preparation.AllParticipantsPrepared {
		return in, ErrHandoffSnapshotNotReady
	}
	return in, nil
}

// Status refresh does not grant membership, unfreeze any service or commit an
// owner. Remote I/O occurs outside AM locks; identity/version/expiry/holds are
// revalidated before any observation or acknowledgment is persisted/returned.
func (s *Store) GetOwnerHandoffStatus(ctx context.Context, in BrandCloudOwnerTransferQuery) (model.BrandCloudOwnerTransfer, error) {
	tx, view, err := s.lockHandoffParticipant(ctx, in)
	if err != nil {
		return view, err
	}
	if view.Status != "accepted" || view.OperationPhase != "preparing" {
		_ = tx.Rollback(ctx)
		return view, nil
	}
	binding, err := confirmableHandoff(ctx, tx, view)
	_ = tx.Rollback(ctx)
	if errors.Is(err, ErrHandoffSnapshotNotReady) {
		return handoffView(view, "blocked", handoffBlockers("evidence_unavailable")), nil
	}
	if err != nil {
		return view, err
	}
	if s.handoffBilling == nil {
		return handoffView(view, "blocked", handoffBlockers("evidence_unavailable")), nil
	}
	status, remoteErr := s.handoffBilling.Settlement(ctx, binding)
	return s.recordHandoffBalance(ctx, in, binding, status, remoteErr, "")
}
func (s *Store) PreviewOwnerHandoff(ctx context.Context, in BrandCloudOwnerTransferQuery) (model.BrandCloudOwnerTransfer, error) {
	view, err := s.GetOwnerHandoffStatus(ctx, in)
	if err == nil && view.Phase != "awaiting_balance_confirmation" {
		err = ErrHandoffSnapshotNotReady
	}
	return view, err
}

func billingBlockers(codes []string) []model.CloudBlocker {
	seen := map[string]bool{}
	result := []model.CloudBlocker{}
	for _, code := range codes {
		switch code {
		case "balance_negative", "usage_unsettled":
		case "outstanding_debt", "unpaid_invoices":
			code = "debt_outstanding"
		case "payments_pending", "payment_setups_pending":
			code = "payment_pending"
		case "refunds_pending":
			code = "refund_pending"
		case "disputes_open":
			code = "dispute_pending"
		case "settlement_evidence_stale":
			code = "confirmation_stale"
		default:
			code = "evidence_unavailable"
		}
		if !seen[code] {
			result = append(result, handoffBlockers(code)...)
			seen[code] = true
		}
	}
	if len(result) == 0 {
		return handoffBlockers("evidence_unavailable")
	}
	return result
}

func (s *Store) recordHandoffBalance(ctx context.Context, in BrandCloudOwnerTransferQuery, expected billinghandoff.Binding, status billinghandoff.Settlement, remoteErr error, requestID string) (model.BrandCloudOwnerTransfer, error) {
	tx, view, err := s.lockHandoffParticipant(ctx, in)
	if err != nil {
		return view, err
	}
	defer tx.Rollback(ctx)
	binding, err := confirmableHandoff(ctx, tx, view)
	if errors.Is(err, ErrHandoffSnapshotNotReady) {
		if view.Phase == "canceled" {
			return view, nil
		}
		return handoffView(view, "blocked", handoffBlockers("lifecycle_conflict")), nil
	}
	if err != nil {
		return view, err
	}
	if !sameHandoffBinding(binding, expected) || remoteErr != nil || status.OperationID != view.ID {
		return handoffView(view, "blocked", handoffBlockers("evidence_unavailable")), nil
	}
	if status.Snapshot == nil || len(status.Blockers) != 0 {
		return handoffView(view, "blocked", billingBlockers(status.Blockers)), nil
	}
	snapshot := status.Snapshot
	if status.Phase != "prepared" || snapshot.Version < 2 || snapshot.BalanceMinor < 0 || snapshot.Currency != "TWD" || !snapshot.Cutoff.Equal(binding.Cutoff) {
		return handoffView(view, "blocked", handoffBlockers("evidence_unavailable")), nil
	}
	if requestID != "" {
		var version, amount int64
		var actor, currency string
		if err := tx.QueryRow(ctx, `SELECT r.user_id::text,r.billing_snapshot_version,s.balance_minor,s.currency
			FROM cloud_handoff_confirmation_requests r JOIN cloud_handoff_billing_snapshots s USING(operation_id,billing_snapshot_version)
			WHERE r.id=$1 AND r.operation_id=$2`, requestID, view.ID).Scan(&actor, &version, &amount, &currency); err != nil {
			return view, err
		}
		if actor != in.RequesterID || version != snapshot.Version || amount != snapshot.BalanceMinor || currency != snapshot.Currency ||
			(actor == binding.SourceUserID && !snapshot.SourceConfirmed) || (actor == binding.TargetUserID && !snapshot.TargetConfirmed) {
			return view, ErrHandoffSnapshotNotReady
		}
	}
	inserted, err := tx.Exec(ctx, `INSERT INTO cloud_handoff_billing_snapshots(operation_id,billing_snapshot_version,balance_minor,currency,cutoff)
		VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, view.ID, snapshot.Version, snapshot.BalanceMinor, snapshot.Currency, snapshot.Cutoff)
	if err != nil {
		return view, err
	}
	view, err = hydrateHandoff(ctx, tx, view)
	if err != nil {
		return view, err
	}
	if view.BalanceSnapshot == nil || view.BalanceSnapshot.BillingSnapshotVersion != snapshot.Version || view.BalanceSnapshot.BalanceMinor != snapshot.BalanceMinor || view.BalanceSnapshot.Currency != snapshot.Currency {
		return handoffView(view, "blocked", handoffBlockers("confirmation_stale")), nil
	}
	if inserted.RowsAffected() != 0 {
		if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_owner_transfer_balance_previewed", ActorUserID: &in.RequesterID, OrganizationID: &in.BrandCloudID,
			SubjectType: "brand_cloud_owner_transfer", SubjectID: view.ID, Payload: map[string]any{"billing_snapshot_version": snapshot.Version, "balance_minor": snapshot.BalanceMinor, "currency": snapshot.Currency}}); err != nil {
			return view, err
		}
	}
	if requestID != "" {
		encoded, _ := json.Marshal(map[string]any{"operation_id": view.ID, "ownership_version": binding.OwnershipVersion, "user_id": in.RequesterID, "snapshot_version": snapshot.Version,
			"balance_minor": snapshot.BalanceMinor, "currency": snapshot.Currency, "cutoff": snapshot.Cutoff})
		sum := sha256.Sum256(encoded)
		digest := hex.EncodeToString(sum[:])
		inserted, err := tx.Exec(ctx, `INSERT INTO cloud_handoff_confirmation_acknowledgments(request_id,receipt_sha256) VALUES($1,$2) ON CONFLICT DO NOTHING`, requestID, digest)
		if err != nil {
			return view, err
		}
		var prior string
		if err := tx.QueryRow(ctx, `SELECT receipt_sha256 FROM cloud_handoff_confirmation_acknowledgments WHERE request_id=$1`, requestID).Scan(&prior); err != nil {
			return view, err
		}
		if prior != digest {
			return view, ErrConflict
		}
		if inserted.RowsAffected() != 0 {
			if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_owner_transfer_balance_confirmed", ActorUserID: &in.RequesterID, OrganizationID: &in.BrandCloudID,
				SubjectType: "brand_cloud_owner_transfer", SubjectID: view.ID, Payload: map[string]any{"request_id": requestID, "billing_snapshot_version": snapshot.Version, "receipt_sha256": digest}}); err != nil {
				return view, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return view, err
	}
	view.SourceConfirmed, view.TargetConfirmed = &snapshot.SourceConfirmed, &snapshot.TargetConfirmed
	return handoffView(view, "awaiting_balance_confirmation", []model.CloudBlocker{}), nil
}

func sameHandoffBinding(a, b billinghandoff.Binding) bool {
	return a.CloudID == b.CloudID && a.OperationID == b.OperationID &&
		a.SourceUserID == b.SourceUserID && a.TargetUserID == b.TargetUserID &&
		a.OwnershipVersion == b.OwnershipVersion && a.Cutoff.Equal(b.Cutoff)
}

func (s *Store) ConfirmOwnerHandoff(ctx context.Context, in HandoffConfirmationInput) (model.BrandCloudOwnerTransfer, error) {
	if !handoffIdempotencyKey.MatchString(in.IdempotencyKey) || in.Snapshot.OwnershipVersion < 1 || in.Snapshot.BillingSnapshotVersion < 2 || in.Snapshot.BalanceMinor < 0 || in.Snapshot.Currency != "TWD" {
		return model.BrandCloudOwnerTransfer{}, ErrConflict
	}
	view, err := s.PreviewOwnerHandoff(ctx, in.Query)
	if err != nil {
		return view, err
	}
	if view.BalanceSnapshot == nil || *view.BalanceSnapshot != in.Snapshot {
		return view, ErrHandoffSnapshotNotReady
	}
	tx, view, err := s.lockHandoffParticipant(ctx, in.Query)
	if err != nil {
		return view, err
	}
	defer tx.Rollback(ctx)
	binding, err := confirmableHandoff(ctx, tx, view)
	if err != nil {
		return view, err
	}
	if view.BalanceSnapshot == nil || *view.BalanceSnapshot != in.Snapshot {
		return view, ErrHandoffSnapshotNotReady
	}
	var requestID string
	var version int64
	err = tx.QueryRow(ctx, `SELECT id::text,billing_snapshot_version FROM cloud_handoff_confirmation_requests WHERE operation_id=$1 AND user_id=$2 AND idempotency_key=$3`, view.ID, in.Query.RequesterID, in.IdempotencyKey).Scan(&requestID, &version)
	if err == nil && version != in.Snapshot.BillingSnapshotVersion {
		return view, ErrConflict
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return view, err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.QueryRow(ctx, `INSERT INTO cloud_handoff_confirmation_requests(operation_id,billing_snapshot_version,user_id,idempotency_key)
			VALUES($1,$2,$3,$4) RETURNING id::text`, view.ID, in.Snapshot.BillingSnapshotVersion, in.Query.RequesterID, in.IdempotencyKey).Scan(&requestID); err != nil {
			return view, err
		}
		if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_owner_transfer_balance_confirmation_requested", ActorUserID: &in.Query.RequesterID, OrganizationID: &in.Query.BrandCloudID,
			SubjectType: "brand_cloud_owner_transfer", SubjectID: view.ID, Payload: map[string]any{"request_id": requestID, "billing_snapshot_version": in.Snapshot.BillingSnapshotVersion, "balance_minor": in.Snapshot.BalanceMinor, "currency": in.Snapshot.Currency}}); err != nil {
			return view, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return view, err
	}
	// This durable intent precedes the remote side effect. If delivery or local
	// acknowledgment fails, retry only this same actor/key/immutable snapshot.
	status, remoteErr := s.handoffBilling.Confirm(ctx, binding, billinghandoff.Confirmation{UserID: in.Query.RequesterID, SnapshotVersion: in.Snapshot.BillingSnapshotVersion, BalanceMinor: in.Snapshot.BalanceMinor, Currency: in.Snapshot.Currency})
	view, err = s.recordHandoffBalance(ctx, in.Query, binding, status, remoteErr, requestID)
	if err == nil && view.Phase != "awaiting_balance_confirmation" {
		err = ErrHandoffSnapshotNotReady
	}
	return view, err
}
