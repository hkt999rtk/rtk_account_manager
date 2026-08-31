package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/billinghandoff"
	"rtk_account_manager/internal/model"
)

// Implemented by the dedicated authenticated Billing adapter, never browser input.
type HandoffCommitBilling interface {
	AuthorizeCommit(context.Context, billinghandoff.Binding, string, int64) (billinghandoff.Authorization, error)
	Finalize(context.Context, billinghandoff.Binding, string, time.Time, string) (billinghandoff.ProtocolAck, error)
}

type HandoffCommittedDecision struct {
	Binding                                           billinghandoff.Binding
	AuthorizationID                                   string
	BillingSnapshotVersion, CommittedOwnershipVersion int64
	CommittedAt                                       time.Time
	DecisionSHA256                                    string
}

func loadHandoffDecision(ctx context.Context, q handoffQuerier, cloud, operation string) (HandoffCommittedDecision, error) {
	var out HandoffCommittedDecision
	err := q.QueryRow(ctx, `SELECT h.brand_cloud_id::text,h.id::text,h.source_user_id::text,h.target_user_id::text,h.ownership_version,h.cutoff,
		d.authorization_id::text,d.billing_snapshot_version,d.committed_ownership_version,d.committed_at,d.decision_sha256
		FROM cloud_handoff_committed_decisions d JOIN cloud_ownership_handoffs h ON h.id=d.operation_id
		WHERE h.id::text=$1 AND h.brand_cloud_id::text=$2`, operation, cloud).Scan(&out.Binding.CloudID, &out.Binding.OperationID, &out.Binding.SourceUserID, &out.Binding.TargetUserID,
		&out.Binding.OwnershipVersion, &out.Binding.Cutoff, &out.AuthorizationID, &out.BillingSnapshotVersion, &out.CommittedOwnershipVersion, &out.CommittedAt, &out.DecisionSHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return out, err
}

// Internal coordinator lock; unlike participant HTTP reads, a committed recovery
// must still run if either global user is later disabled. Fresh commit rechecks
// both users explicitly, but finalization never depends on source access.
func (s *Store) lockHandoffOperation(ctx context.Context, cloud, operation string) (pgx.Tx, model.BrandCloudOwnerTransfer, error) {
	var source, target string
	err := s.db.QueryRow(ctx, `SELECT source_user_id::text,target_user_id::text FROM cloud_ownership_handoffs WHERE id::text=$1 AND brand_cloud_id::text=$2`, operation, cloud).Scan(&source, &target)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, model.BrandCloudOwnerTransfer{}, ErrNotFound
	}
	if err != nil {
		return nil, model.BrandCloudOwnerTransfer{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, model.BrandCloudOwnerTransfer{}, err
	}
	if err := lockBrandCloudCollaborationTx(ctx, tx, cloud, source, target); err != nil {
		_ = tx.Rollback(ctx)
		return nil, model.BrandCloudOwnerTransfer{}, err
	}
	view, err := scanBrandCloudOwnerTransfer(tx.QueryRow(ctx, `SELECT `+transferColumns+` FROM brand_cloud_owner_transfers WHERE id=$1 FOR UPDATE`, operation))
	if err == nil {
		view, err = hydrateHandoff(ctx, tx, view)
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, view, err
	}
	return tx, view, nil
}

func validateHandoffCommitTx(ctx context.Context, tx pgx.Tx, view model.BrandCloudOwnerTransfer) (billinghandoff.Binding, error) {
	if view.OperationPhase != "preparing" && view.OperationPhase != "committing" {
		return billinghandoff.Binding{}, ErrConflict
	}
	copy := view
	copy.OperationPhase = "preparing"
	binding, err := confirmableHandoff(ctx, tx, copy)
	if err != nil {
		return binding, err
	}
	if binding.OwnershipVersion == math.MaxInt64 || view.BalanceSnapshot == nil {
		return binding, ErrConflict
	}
	var disabledMember bool
	var limit int
	if err := tx.QueryRow(ctx, `SELECT u.developer_cloud_limit,EXISTS(SELECT 1 FROM organization_members WHERE organization_id=$2 AND user_id=u.id AND disabled_at IS NOT NULL)
		FROM users u WHERE u.id=$1`, binding.TargetUserID, binding.CloudID).Scan(&limit, &disabledMember); err != nil {
		return binding, err
	}
	if disabledMember {
		return binding, ErrConflict
	}
	used, err := countCloudQuotaUsageTx(ctx, tx, binding.TargetUserID)
	if err != nil {
		return binding, err
	}
	if used > limit {
		return binding, ErrDeveloperCloudLimitExceeded
	}
	var confirmed int
	err = tx.QueryRow(ctx, `SELECT count(DISTINCT r.user_id) FROM cloud_handoff_confirmation_requests r
		JOIN cloud_handoff_confirmation_acknowledgments a ON a.request_id=r.id
		WHERE r.operation_id=$1 AND r.billing_snapshot_version=$2 AND r.user_id IN ($3,$4)`, view.ID, view.BalanceSnapshot.BillingSnapshotVersion, binding.SourceUserID, binding.TargetUserID).Scan(&confirmed)
	if err != nil {
		return binding, err
	}
	if confirmed != 2 {
		return binding, ErrHandoffSnapshotNotReady
	}
	return binding, nil
}

func handoffDigest(value any) string {
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// CommitOwnerHandoff is a trusted coordinator operation, not a public owner API.
// The authorization request is durable BEFORE Billing can freeze a commit grant.
// Replay uses its same ID/version. The owner decision, revocation and committed
// outbox are one AM transaction; remote finalization is intentionally separate.
func (s *Store) CommitOwnerHandoff(ctx context.Context, cloud, operation string) (HandoffCommittedDecision, error) {
	if prior, err := loadHandoffDecision(ctx, s.db, cloud, operation); err == nil {
		return prior, nil
	} else if !errors.Is(err, ErrNotFound) {
		return prior, err
	}
	remote, ok := s.handoffBilling.(HandoffCommitBilling)
	if !ok {
		return HandoffCommittedDecision{}, ErrHandoffUnavailable
	}
	tx, view, err := s.lockHandoffOperation(ctx, cloud, operation)
	if err != nil {
		return HandoffCommittedDecision{}, err
	}
	defer tx.Rollback(ctx)
	if prior, err := loadHandoffDecision(ctx, tx, cloud, operation); err == nil {
		return prior, nil
	} else if !errors.Is(err, ErrNotFound) {
		return prior, err
	}
	binding, err := validateHandoffCommitTx(ctx, tx, view)
	if err != nil {
		return HandoffCommittedDecision{}, err
	}
	var authorizationID string
	var snapshotVersion int64
	err = tx.QueryRow(ctx, `SELECT authorization_id::text,billing_snapshot_version FROM cloud_handoff_commit_requests WHERE operation_id=$1`, operation).Scan(&authorizationID, &snapshotVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		snapshotVersion = view.BalanceSnapshot.BillingSnapshotVersion
		err = tx.QueryRow(ctx, `INSERT INTO cloud_handoff_commit_requests(operation_id,billing_snapshot_version) VALUES($1,$2) RETURNING authorization_id::text`, operation, snapshotVersion).Scan(&authorizationID)
		if err != nil {
			return HandoffCommittedDecision{}, err
		}
		if _, err = tx.Exec(ctx, `UPDATE cloud_ownership_handoffs SET phase='committing',version=version+1,updated_at=now() WHERE id=$1`, operation); err != nil {
			return HandoffCommittedDecision{}, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO cloud_handoff_outbox(operation_id,participant,action,payload) VALUES($1,'billing','authorize_commit',$2)`, operation, map[string]any{
			"binding": binding, "authorization_id": authorizationID, "snapshot_version": snapshotVersion}); err != nil {
			return HandoffCommittedDecision{}, err
		}
		if err = createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_owner_transfer_commit_requested", OrganizationID: &cloud, SubjectType: "brand_cloud_owner_transfer", SubjectID: operation,
			Payload: map[string]any{"authorization_id": authorizationID, "billing_snapshot_version": snapshotVersion}}); err != nil {
			return HandoffCommittedDecision{}, err
		}
	} else if err != nil {
		return HandoffCommittedDecision{}, err
	}
	if snapshotVersion != view.BalanceSnapshot.BillingSnapshotVersion {
		return HandoffCommittedDecision{}, ErrHandoffSnapshotNotReady
	}
	if err = tx.Commit(ctx); err != nil {
		return HandoffCommittedDecision{}, err
	}
	grant, err := remote.AuthorizeCommit(ctx, binding, authorizationID, snapshotVersion)
	if err != nil {
		return HandoffCommittedDecision{}, ErrHandoffUnavailable
	}
	if grant.OperationID != operation || grant.AuthorizationID != authorizationID || grant.SnapshotVersion != snapshotVersion || grant.CreatedAt.IsZero() {
		return HandoffCommittedDecision{}, ErrHandoffUnavailable
	}
	return s.commitAuthorizedHandoff(ctx, binding, grant)
}

func (s *Store) commitAuthorizedHandoff(ctx context.Context, expected billinghandoff.Binding, grant billinghandoff.Authorization) (HandoffCommittedDecision, error) {
	tx, view, err := s.lockHandoffOperation(ctx, expected.CloudID, expected.OperationID)
	if err != nil {
		return HandoffCommittedDecision{}, err
	}
	defer tx.Rollback(ctx)
	if prior, err := loadHandoffDecision(ctx, tx, expected.CloudID, expected.OperationID); err == nil {
		if prior.AuthorizationID != grant.AuthorizationID {
			return prior, ErrConflict
		}
		return prior, nil
	} else if !errors.Is(err, ErrNotFound) {
		return prior, err
	}
	binding, err := validateHandoffCommitTx(ctx, tx, view)
	if err != nil {
		return HandoffCommittedDecision{}, err
	}
	if view.OperationPhase != "committing" || !sameHandoffBinding(binding, expected) || view.BalanceSnapshot.BillingSnapshotVersion != grant.SnapshotVersion {
		return HandoffCommittedDecision{}, ErrConflict
	}
	var matches bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cloud_handoff_commit_requests WHERE operation_id=$1 AND authorization_id=$2 AND billing_snapshot_version=$3)`, view.ID, grant.AuthorizationID, grant.SnapshotVersion).Scan(&matches); err != nil {
		return HandoffCommittedDecision{}, err
	}
	if !matches {
		return HandoffCommittedDecision{}, ErrConflict
	}
	decision := HandoffCommittedDecision{Binding: binding, AuthorizationID: grant.AuthorizationID, BillingSnapshotVersion: grant.SnapshotVersion, CommittedOwnershipVersion: binding.OwnershipVersion + 1}
	if err = tx.QueryRow(ctx, `SELECT GREATEST(clock_timestamp(),$1::timestamptz)`, binding.Cutoff).Scan(&decision.CommittedAt); err != nil {
		return decision, err
	}
	decision.DecisionSHA256 = handoffDigest(decision)
	if _, err = tx.Exec(ctx, `INSERT INTO cloud_handoff_committed_decisions(operation_id,authorization_id,billing_snapshot_version,committed_ownership_version,committed_at,decision_sha256)
		VALUES($1,$2,$3,$4,$5,$6)`, view.ID, grant.AuthorizationID, grant.SnapshotVersion, decision.CommittedOwnershipVersion, decision.CommittedAt, decision.DecisionSHA256); err != nil {
		return decision, err
	}
	// Promote before removing source; exactly-one-owner is checked at commit.
	if _, err = tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'owner')
		ON CONFLICT(organization_id,user_id) DO UPDATE SET role='owner',access_scope=NULL`, binding.CloudID, binding.TargetUserID); err != nil {
		return decision, err
	}
	// Copy only source's actual active Product-owner duties, not collaborator roles.
	if _, err = tx.Exec(ctx, `UPDATE role_assignments target SET disabled_at=now() FROM roles r
		WHERE target.role_id=r.id AND r.name IN ('product_editor','product_viewer') AND target.actor_type='user' AND target.actor_id=$2
		AND target.organization_id=$1 AND target.scope_type='product' AND target.disabled_at IS NULL AND EXISTS(
		SELECT 1 FROM role_assignments source JOIN roles own ON own.id=source.role_id AND own.name='product_owner' AND own.disabled_at IS NULL
		WHERE source.organization_id=$1 AND source.actor_type='user' AND source.actor_id=$3 AND source.scope_type='product'
		AND source.scope_id=target.scope_id AND source.disabled_at IS NULL)`, binding.CloudID, binding.TargetUserID, binding.SourceUserID); err != nil {
		return decision, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type,scope_id,organization_id)
		SELECT DISTINCT ra.role_id,'user',$2,'product',ra.scope_id,ra.organization_id FROM role_assignments ra
		JOIN roles r ON r.id=ra.role_id AND r.name='product_owner' AND r.disabled_at IS NULL
		JOIN device_item_profiles p ON p.id::text=ra.scope_id AND p.brand_cloud_id=ra.organization_id
		WHERE ra.organization_id=$1 AND ra.actor_type='user' AND ra.actor_id=$3 AND ra.scope_type='product' AND ra.disabled_at IS NULL
		ON CONFLICT DO NOTHING`, binding.CloudID, binding.TargetUserID, binding.SourceUserID); err != nil {
		return decision, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM organization_members WHERE organization_id=$1 AND user_id=$2`, binding.CloudID, binding.SourceUserID); err != nil {
		return decision, err
	}
	// User ACLs/invitations/admissions are revoked by the membership trigger.
	// Revoke service delegation derived from source, preserving other collaborators.
	if _, err = tx.Exec(ctx, `UPDATE role_assignments SET disabled_at=COALESCE(disabled_at,now()) WHERE organization_id=$1 AND actor_type='service_account' AND created_by=$2`, binding.CloudID, binding.SourceUserID); err != nil {
		return decision, err
	}
	if _, err = tx.Exec(ctx, `UPDATE device_claim_tokens SET revoked_at=COALESCE(revoked_at,now()) WHERE organization_id=$1 AND created_by=$2 AND claimed_at IS NULL`, binding.CloudID, binding.SourceUserID); err != nil {
		return decision, err
	}
	if _, err = tx.Exec(ctx, `UPDATE organizations SET ownership_version=$2 WHERE id=$1`, binding.CloudID, decision.CommittedOwnershipVersion); err != nil {
		return decision, err
	}
	if _, err = tx.Exec(ctx, `UPDATE cloud_ownership_handoffs SET phase='finalizing',version=version+1,updated_at=now() WHERE id=$1`, view.ID); err != nil {
		return decision, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO cloud_handoff_outbox(operation_id,participant,action,payload) VALUES($1,'billing','finalize',$2)`, view.ID, decision); err != nil {
		return decision, err
	}
	if err = createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_owner_transfer_committed", OrganizationID: &binding.CloudID, SubjectType: "brand_cloud_owner_transfer", SubjectID: view.ID,
		Payload: map[string]any{"decision_sha256": decision.DecisionSHA256, "source_user_id": binding.SourceUserID, "owner_user_id": binding.TargetUserID, "ownership_version": decision.CommittedOwnershipVersion}}); err != nil {
		return decision, err
	}
	if err = tx.Commit(ctx); err != nil {
		return decision, err
	}
	return decision, nil
}

// Finalize never calls commit authorization again and works after source access
// is revoked. A failed/lost reply leaves finalizing and the same durable decision.
func (s *Store) FinalizeOwnerHandoff(ctx context.Context, cloud, operation string) (string, error) {
	decision, err := loadHandoffDecision(ctx, s.db, cloud, operation)
	if err != nil {
		return "", err
	}
	var phase string
	var acknowledged bool
	if err = s.db.QueryRow(ctx, `SELECT phase,EXISTS(SELECT 1 FROM cloud_handoff_finalization_acknowledgments WHERE operation_id=$1 AND participant='billing')
		FROM cloud_ownership_handoffs WHERE id=$1`, operation).Scan(&phase, &acknowledged); err != nil {
		return "", err
	}
	if acknowledged {
		return phase, nil
	}
	remote, ok := s.handoffBilling.(HandoffCommitBilling)
	if !ok {
		return phase, ErrHandoffUnavailable
	}
	ack, err := remote.Finalize(ctx, decision.Binding, decision.AuthorizationID, decision.CommittedAt, decision.DecisionSHA256)
	if err != nil || ack.OperationID != operation || ack.Phase != "finalized" {
		return phase, ErrHandoffUnavailable
	}
	return s.RecordHandoffFinalizationAck(ctx, HandoffFinalizationAck{CloudID: cloud, OperationID: operation, OwnershipVersion: decision.Binding.OwnershipVersion,
		DecisionSHA256: decision.DecisionSHA256, Participant: "billing", ReceiptSHA256: handoffDigest(struct {
			Decision string
			Ack      billinghandoff.ProtocolAck
		}{decision.DecisionSHA256, ack})})
}

// Trusted authenticated producer adapter only. A successful HTTP delivery is not
// a producer's durable release/authorization-version installation acknowledgment.
type HandoffFinalizationAck struct {
	CloudID, OperationID, DecisionSHA256, Participant, ReceiptSHA256 string
	OwnershipVersion                                                 int64
}

func (s *Store) RecordHandoffFinalizationAck(ctx context.Context, in HandoffFinalizationAck) (string, error) {
	if !handoffEvidenceDigest.MatchString(in.ReceiptSHA256) || !handoffEvidenceDigest.MatchString(in.DecisionSHA256) {
		return "", ErrConflict
	}
	tx, view, err := s.lockHandoffOperation(ctx, in.CloudID, in.OperationID)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	decision, err := loadHandoffDecision(ctx, tx, in.CloudID, in.OperationID)
	if err != nil {
		return "", err
	}
	if decision.DecisionSHA256 != in.DecisionSHA256 || decision.Binding.OwnershipVersion != in.OwnershipVersion {
		return "", ErrConflict
	}
	var prior string
	err = tx.QueryRow(ctx, `SELECT receipt_sha256 FROM cloud_handoff_finalization_acknowledgments WHERE operation_id=$1 AND participant=$2`, in.OperationID, in.Participant).Scan(&prior)
	if err == nil {
		if prior != in.ReceiptSHA256 {
			return "", ErrConflict
		}
		return view.OperationPhase, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if view.OperationPhase != "finalizing" {
		return "", ErrConflict
	}
	action := "release"
	if in.Participant == "billing" {
		action = "finalize"
	}
	var expected bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cloud_handoff_outbox WHERE operation_id=$1 AND participant=$2 AND action=$3)`, in.OperationID, in.Participant, action).Scan(&expected); err != nil {
		return "", err
	}
	if !expected {
		return "", ErrConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO cloud_handoff_finalization_acknowledgments(operation_id,participant,receipt_sha256) VALUES($1,$2,$3)`, in.OperationID, in.Participant, in.ReceiptSHA256); err != nil {
		return "", err
	}
	if in.Participant == "billing" {
		if _, err = tx.Exec(ctx, `INSERT INTO cloud_handoff_outbox(operation_id,participant,action,payload)
			SELECT operation_id,participant,'release',$2 FROM cloud_handoff_participants WHERE operation_id=$1 AND participant<>'billing' ON CONFLICT DO NOTHING`, in.OperationID, decision); err != nil {
			return "", err
		}
	}
	var missing bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cloud_handoff_participants p WHERE p.operation_id=$1 AND NOT EXISTS(
		SELECT 1 FROM cloud_handoff_finalization_acknowledgments a WHERE a.operation_id=p.operation_id AND a.participant=p.participant))`, in.OperationID).Scan(&missing); err != nil {
		return "", err
	}
	phase := view.OperationPhase
	if !missing {
		if _, err = tx.Exec(ctx, `UPDATE cloud_ownership_handoffs SET phase='succeeded',version=version+1,updated_at=now() WHERE id=$1`, in.OperationID); err != nil {
			return "", err
		}
		phase = "succeeded"
	}
	if err = createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_owner_transfer_finalization_acknowledged", OrganizationID: &in.CloudID, SubjectType: "brand_cloud_owner_transfer", SubjectID: in.OperationID,
		Payload: map[string]any{"participant": in.Participant, "phase": phase, "receipt_sha256": in.ReceiptSHA256, "decision_sha256": in.DecisionSHA256}}); err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	return phase, nil
}
