package store

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/billinghandoff"
	"rtk_account_manager/internal/model"
)

// This boundary is implemented by a trusted Billing client, never browser input.
// Eligibility is advisory; it does not authorize ownership commit or settlement.
type HandoffEligibilityRequest = billinghandoff.OwnershipEligibilityRequest
type HandoffEligibility = billinghandoff.OwnershipEligibility
type HandoffEligibilityProvider interface {
	CheckOwnershipEligibility(context.Context, HandoffEligibilityRequest) (HandoffEligibility, error)
}

var _ HandoffEligibilityProvider = (*billinghandoff.Client)(nil)

type OwnershipHandoffOptions struct {
	Eligibility HandoffEligibilityProvider
	// Explicit reviewed producer inventory. It must match the current release
	// contract exactly; callers cannot opt a participant out.
	Producers []string
}

const (
	HandoffParticipantFactory           = "factory"
	HandoffParticipantMQTTUsage         = "mqtt_usage"
	HandoffParticipantVideoControlPlane = "video_control_plane"
)

var requiredHandoffProducers = [...]string{
	HandoffParticipantFactory,
	HandoffParticipantMQTTUsage,
	HandoffParticipantVideoControlPlane,
}

// RequiredHandoffProducers returns a copy so deployment composition and tests
// use the reviewed inventory without exposing mutable package state.
func RequiredHandoffProducers() []string {
	return append([]string(nil), requiredHandoffProducers[:]...)
}

func exactHandoffProducerInventory(names []string) bool {
	if len(names) != len(requiredHandoffProducers) {
		return false
	}
	copy := append([]string(nil), names...)
	slices.Sort(copy)
	return slices.Equal(copy, requiredHandoffProducers[:])
}

var handoffParticipantName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var handoffEvidenceDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Configure once before serving requests. Missing transport/inventory fails closed.
func (s *Store) ConfigureOwnershipHandoff(in OwnershipHandoffOptions) error {
	if in.Eligibility == nil {
		return ErrHandoffUnavailable
	}
	if !exactHandoffProducerInventory(in.Producers) {
		return ErrConflict
	}
	in.Producers = RequiredHandoffProducers()
	s.ownershipHandoff = &in
	return nil
}
func (s *Store) checkHandoffEligibility(ctx context.Context, in HandoffEligibilityRequest, now time.Time) (HandoffEligibility, error) {
	if s.ownershipHandoff == nil {
		return HandoffEligibility{}, ErrHandoffUnavailable
	}
	evidence, err := s.ownershipHandoff.Eligibility.CheckOwnershipEligibility(ctx, in)
	if err != nil {
		return HandoffEligibility{}, ErrHandoffUnavailable
	}
	if current := time.Now().UTC(); current.After(now) {
		now = current
	}
	if err := validateHandoffEligibility(evidence, in, now); err != nil {
		return HandoffEligibility{}, err
	}
	return evidence, nil
}
func validateHandoffEligibility(evidence HandoffEligibility, in HandoffEligibilityRequest, now time.Time) error {
	if evidence.Request != in || evidence.ReceiptID == "" || !handoffEvidenceDigest.MatchString(evidence.EvidenceSHA256) || evidence.Currency != "TWD" || !evidence.Complete ||
		evidence.ObservedAt.After(now) || evidence.ObservedAt.Before(now.Add(-5*time.Minute)) || !evidence.ExpiresAt.After(now) || evidence.ExpiresAt.After(evidence.ObservedAt.Add(5*time.Minute)) {
		return ErrHandoffUnavailable
	}
	if evidence.BalanceMinor < 0 {
		return ErrHandoffBalanceNegative
	}
	if len(evidence.Blockers) != 0 {
		return ErrHandoffFinancialBlocked
	}
	return nil
}

type handoffQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func handoffVersion(ctx context.Context, q handoffQuerier, cloud, source, target string) (int64, error) {
	var version int64
	err := q.QueryRow(ctx, `SELECT o.ownership_version FROM organizations o
		JOIN organization_members m ON m.organization_id=o.id AND m.role='owner'
		JOIN users u ON u.id=m.user_id JOIN users target ON target.id::text=$3
		WHERE o.id::text=$1 AND m.user_id::text=$2 AND o.organization_kind='brand_cloud'
		AND user_can_access_brand_cloud($2,$1) AND target.disabled_at IS NULL
		AND target.email_verified AND NOT target.signup_pending_verification`, cloud, source, target).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return version, err
}
func countReservedCloudsTx(ctx context.Context, q handoffQuerier, user string) (int, error) {
	var count int
	err := q.QueryRow(ctx, `SELECT count(*) FROM cloud_ownership_handoffs WHERE target_user_id::text=$1 AND phase IN ('preparing','committing','canceling')`, user).Scan(&count)
	return count, err
}
func countCloudQuotaUsageTx(ctx context.Context, q handoffQuerier, user string) (int, error) {
	owned, err := countDeveloperBrandCloudsTx(ctx, q, user)
	if err != nil {
		return 0, err
	}
	reserved, err := countReservedCloudsTx(ctx, q, user)
	return owned + reserved, err
}

const transferColumns = `id::text,brand_cloud_id::text,requested_by_user_id::text,target_user_id::text,status,expires_at,accepted_at,canceled_at,created_at,updated_at`

func hydrateHandoff(ctx context.Context, q handoffQuerier, transfer model.BrandCloudOwnerTransfer) (model.BrandCloudOwnerTransfer, error) {
	var created, updated *time.Time
	err := q.QueryRow(ctx, `SELECT COALESCE(t.ownership_version,0),COALESCE(h.phase,''),h.created_at,h.updated_at FROM brand_cloud_owner_transfers t LEFT JOIN cloud_ownership_handoffs h ON h.id=t.id WHERE t.id=$1`, transfer.ID).Scan(&transfer.OwnershipVersion, &transfer.OperationPhase, &created, &updated)
	if err != nil {
		return transfer, err
	}
	if created != nil && updated != nil {
		transfer.Operation = &model.CloudOperation{ID: transfer.ID, BrandCloudID: transfer.BrandCloudID, Type: "owner_transfer", CreatedAt: *created, UpdatedAt: *updated}
	}
	var snapshot model.CloudBalanceSnapshot
	snapshot.OwnershipVersion = transfer.OwnershipVersion
	err = q.QueryRow(ctx, `SELECT billing_snapshot_version,balance_minor,currency FROM cloud_handoff_billing_snapshots WHERE operation_id=$1 ORDER BY billing_snapshot_version DESC LIMIT 1`, transfer.ID).
		Scan(&snapshot.BillingSnapshotVersion, &snapshot.BalanceMinor, &snapshot.Currency)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return transfer, err
	}
	if err == nil {
		transfer.BalanceSnapshot = &snapshot
	}
	return projectHandoffStatus(transfer), nil
}

func enqueueHandoffCommands(ctx context.Context, tx pgx.Tx, operation, action string) error {
	_, err := tx.Exec(ctx, `INSERT INTO cloud_handoff_outbox(operation_id,participant,action,payload)
		SELECT h.id,p.participant,$2,jsonb_build_object('operation_id',h.id,'cloud_id',h.brand_cloud_id,
		'source_user_id',h.source_user_id,'target_user_id',h.target_user_id,'ownership_version',h.ownership_version,'cutoff',h.cutoff,'action',$2::text)
		FROM cloud_ownership_handoffs h JOIN cloud_handoff_participants p ON p.operation_id=h.id WHERE h.id=$1
		ON CONFLICT(operation_id,participant,action) DO NOTHING`, operation, action)
	return err
}

func (s *Store) acceptOwnerHandoff(ctx context.Context, target, token string, now time.Time) (model.BrandCloudOwnerTransfer, error) {
	transfer, err := scanBrandCloudOwnerTransfer(s.db.QueryRow(ctx, `SELECT `+transferColumns+` FROM brand_cloud_owner_transfers
		WHERE token_hash=$1 AND target_user_id::text=$2 AND status IN ('pending','accepted') AND expires_at>$3`, token, target, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.BrandCloudOwnerTransfer{}, ErrNotFound
	}
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	if transfer.Status == "accepted" {
		prior, err := s.GetBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferQuery{BrandCloudID: transfer.BrandCloudID, TransferID: transfer.ID, RequesterID: target}, now)
		if err == nil && prior.OperationPhase == "" {
			return model.BrandCloudOwnerTransfer{}, ErrHandoffUnavailable
		}
		return prior, err
	}
	version, err := handoffVersion(ctx, s.db, transfer.BrandCloudID, transfer.RequestedByUserID, target)
	if err != nil {
		// A concurrent identical acceptance may have installed the cloud fence
		// between our request lookup and preflight read. Return only its durable
		// participant-authorized result; never infer that a failed preflight passed.
		if errors.Is(err, ErrNotFound) {
			prior, readErr := s.GetBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferQuery{BrandCloudID: transfer.BrandCloudID, TransferID: transfer.ID, RequesterID: target}, now)
			if readErr == nil && prior.Status == "accepted" && prior.OperationPhase != "" {
				return prior, nil
			}
		}
		return model.BrandCloudOwnerTransfer{}, err
	}
	evidence, err := s.checkHandoffEligibility(ctx, HandoffEligibilityRequest{CloudID: transfer.BrandCloudID, SourceUserID: transfer.RequestedByUserID, TargetUserID: target, TransferID: transfer.ID, Action: "accept", OwnershipVersion: version}, now)
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockBrandCloudCollaborationTx(ctx, tx, transfer.BrandCloudID, transfer.RequestedByUserID, target); err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	user, err := getDeveloperUserTx(ctx, tx, target)
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	if !user.EmailVerified || user.SignupPendingVerification {
		return model.BrandCloudOwnerTransfer{}, ErrNotFound
	}
	// The remote eligibility check and user/cloud locks may outlive the invite.
	// A request-start timestamp must not authorize an already expired acceptance.
	if current := time.Now().UTC(); current.After(now) {
		now = current
	}
	transfer, err = scanBrandCloudOwnerTransfer(tx.QueryRow(ctx, `SELECT `+transferColumns+` FROM brand_cloud_owner_transfers WHERE id=$1 AND token_hash=$2 AND expires_at>$3 FOR UPDATE`, transfer.ID, token, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.BrandCloudOwnerTransfer{}, ErrNotFound
	}
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	if transfer.Status == "accepted" {
		prior, err := hydrateHandoff(ctx, tx, transfer)
		if err == nil && prior.OperationPhase == "" {
			return model.BrandCloudOwnerTransfer{}, ErrHandoffUnavailable
		}
		return prior, err
	}
	if transfer.Status != "pending" {
		return model.BrandCloudOwnerTransfer{}, ErrNotFound
	}
	if err := validateHandoffEligibility(evidence, evidence.Request, time.Now().UTC()); err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	currentVersion, err := handoffVersion(ctx, tx, transfer.BrandCloudID, transfer.RequestedByUserID, target)
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	var requestedVersion *int64
	if err := tx.QueryRow(ctx, `SELECT ownership_version FROM brand_cloud_owner_transfers WHERE id=$1`, transfer.ID).Scan(&requestedVersion); err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	if requestedVersion == nil || *requestedVersion != version || currentVersion != version {
		return model.BrandCloudOwnerTransfer{}, ErrConflict
	}
	used, err := countCloudQuotaUsageTx(ctx, tx, target)
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	if used >= user.DeveloperCloudLimit {
		return model.BrandCloudOwnerTransfer{}, ErrDeveloperCloudLimitExceeded
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cloud_ownership_handoffs(id,brand_cloud_id,source_user_id,target_user_id,ownership_version,cutoff,acceptance_eligibility)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, transfer.ID, transfer.BrandCloudID, transfer.RequestedByUserID, target, version, now, encoded); err != nil {
		if isUniqueViolation(err) {
			err = ErrConflict
		}
		return model.BrandCloudOwnerTransfer{}, err
	}
	participants := append([]string{"billing"}, s.ownershipHandoff.Producers...)
	for _, participant := range participants {
		if _, err := tx.Exec(ctx, `INSERT INTO cloud_handoff_participants(operation_id,participant) VALUES($1,$2)`, transfer.ID, participant); err != nil {
			return model.BrandCloudOwnerTransfer{}, err
		}
	}
	if err := enqueueHandoffCommands(ctx, tx, transfer.ID, "prepare"); err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	transfer, err = scanBrandCloudOwnerTransfer(tx.QueryRow(ctx, `UPDATE brand_cloud_owner_transfers SET status='accepted',accepted_at=$2 WHERE id=$1 RETURNING `+transferColumns, transfer.ID, now))
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_owner_transfer_preparation_requested", ActorUserID: &target, OrganizationID: &transfer.BrandCloudID, SubjectType: "brand_cloud_owner_transfer", SubjectID: transfer.ID, Payload: map[string]any{"ownership_version": version, "target_user_id": target, "source_user_id": transfer.RequestedByUserID}}); err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	transfer.OperationPhase, transfer.OwnershipVersion = "preparing", version
	return hydrateHandoff(ctx, s.db, transfer)
}
