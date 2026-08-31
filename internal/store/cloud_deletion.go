package store

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/billinghandoff"
	"rtk_account_manager/internal/model"
)

var ErrCloudDeletionBlocked = errors.New("cloud deletion preflight or lifecycle blocked")

type cloudDeletionBlockers struct{ blockers []model.CloudBlocker }

func (e *cloudDeletionBlockers) Error() string { return ErrCloudDeletionBlocked.Error() }
func (e *cloudDeletionBlockers) Unwrap() error { return ErrCloudDeletionBlocked }
func closureFinancialBlockers(codes []string) []model.CloudBlocker {
	result := []model.CloudBlocker{}
	seen := map[string]bool{}
	for _, code := range codes {
		switch code {
		case "balance_negative", "balance_positive":
			code = "balance_nonzero"
		case "usage_unsettled":
		case "outstanding_debt", "unpaid_invoices":
			code = "debt_outstanding"
		case "payments_pending", "payment_setups_pending", "provider_revocations_pending":
			code = "payment_pending"
		case "refunds_pending":
			code = "refund_pending"
		case "disputes_open":
			code = "dispute_pending"
		case "account_not_active":
			code = "lifecycle_conflict"
		default:
			code = "evidence_unavailable"
		}
		if !seen[code] {
			seen[code] = true
			result = append(result, model.CloudBlocker{Code: code, Retryable: true})
		}
	}
	if len(result) == 0 {
		result = append(result, model.CloudBlocker{Code: "evidence_unavailable", Retryable: true})
	}
	return result
}

type CloudDeletionBillingCoordinator interface {
	PrepareCloudClosure(context.Context, billinghandoff.ClosureBinding, string) (billinghandoff.ClosureOperation, error)
	CloudClosureStatus(context.Context, billinghandoff.ClosureBinding) (billinghandoff.ClosureStatus, error)
	CloseCloud(context.Context, billinghandoff.ClosureBinding, string, string) (billinghandoff.ClosureAcknowledgment, error)
}
type CloudDeletionHold struct {
	Binding              billinghandoff.ClosureBinding
	AuthorizationVersion int64
	Participant          string
	Held, Empty          bool
	ReceiptSHA256        string
}

// The producer persists its hold and proves all resource/job inventory is empty.
// An observer response alone cannot implement this interface or authorize closure.
type CloudDeletionProducer interface {
	PrepareCloudDeletion(context.Context, billinghandoff.ClosureBinding, int64) (CloudDeletionHold, error)
}
type CloudDeletionOptions struct {
	Billing   CloudDeletionBillingCoordinator
	Producers map[string]CloudDeletionProducer
}

func (s *Store) ConfigureCloudDeletion(in CloudDeletionOptions) error {
	if s.deletion != nil || s.deletionPreflight == nil || in.Billing == nil || len(in.Producers) == 0 || len(in.Producers) != len(s.deletionPreflight.Resources) {
		return ErrConflict
	}
	copy := CloudDeletionOptions{Billing: in.Billing, Producers: map[string]CloudDeletionProducer{}}
	for name, p := range in.Producers {
		if p == nil || s.deletionPreflight.Resources[name] == nil {
			return ErrConflict
		}
		copy.Producers[name] = p
	}
	s.deletion = &copy
	return nil
}

type CloudDeletionOperation struct {
	ID                   string               `json:"id"`
	CloudID              string               `json:"brand_cloud_id"`
	Type                 string               `json:"type"`
	State                string               `json:"state"`
	Phase                string               `json:"phase"`
	Blockers             []model.CloudBlocker `json:"blockers"`
	CreatedAt            time.Time            `json:"created_at"`
	UpdatedAt            time.Time            `json:"updated_at"`
	OwnerUserID          string               `json:"-"`
	OwnershipVersion     int64                `json:"-"`
	AuthorizationVersion int64                `json:"-"`
	Cutoff               time.Time            `json:"-"`
	RequestSHA256        string               `json:"-"`
	Key                  string               `json:"-"`
}

func (op CloudDeletionOperation) binding() billinghandoff.ClosureBinding {
	return billinghandoff.ClosureBinding{CloudDeletionScope: billinghandoff.CloudDeletionScope{CloudID: op.CloudID, OwnerUserID: op.OwnerUserID, OwnershipVersion: op.OwnershipVersion}, OperationID: op.ID, Cutoff: op.Cutoff}
}

const deletionColumns = `id::text,brand_cloud_id::text,owner_user_id::text,ownership_version,authorization_version,idempotency_key,cutoff,request_sha256,phase,blockers,created_at,updated_at`

func scanDeletion(row scanner) (CloudDeletionOperation, error) {
	var op CloudDeletionOperation
	var raw []byte
	err := row.Scan(&op.ID, &op.CloudID, &op.OwnerUserID, &op.OwnershipVersion, &op.AuthorizationVersion, &op.Key, &op.Cutoff, &op.RequestSHA256, &op.Phase, &raw, &op.CreatedAt, &op.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return op, ErrNotFound
	}
	if err != nil {
		return op, err
	}
	if err = json.Unmarshal(raw, &op.Blockers); err != nil {
		return op, err
	}
	op.Type = "delete"
	op.State = "running"
	if op.Phase == "succeeded" {
		op.State = "succeeded"
	} else if op.Phase == "canceled" {
		op.State = "canceled"
	} else if len(op.Blockers) > 0 {
		op.State = "blocked"
	}
	return op, nil
}

// Historical operation readback is available only to the original, still
// enabled/verified global owner, not a shared member or another cloud owner.
func (s *Store) GetDeveloperCloudDeletion(ctx context.Context, user, cloud, operation string) (CloudDeletionOperation, error) {
	return scanDeletion(s.db.QueryRow(ctx, `SELECT `+deletionColumns+` FROM cloud_deletion_operations d WHERE d.id::text=$3 AND d.brand_cloud_id::text=$2 AND d.owner_user_id::text=$1
        AND EXISTS(SELECT 1 FROM users WHERE id=d.owner_user_id AND disabled_at IS NULL AND email_verified AND NOT signup_pending_verification)`, user, cloud, operation))
}
func lockDeletionOwner(ctx context.Context, tx pgx.Tx, user, cloud string) (int64, int64, error) {
	u, err := getDeveloperUserTx(ctx, tx, user)
	if err != nil {
		return 0, 0, err
	}
	if !u.EmailVerified || u.SignupPendingVerification {
		return 0, 0, ErrAccountNotActivated
	}
	var ownership, authorization int64
	err = tx.QueryRow(ctx, `SELECT o.ownership_version,o.authorization_version FROM organizations o WHERE o.id::text=$1 AND o.organization_kind='brand_cloud'
        AND EXISTS(SELECT 1 FROM organization_members WHERE organization_id=o.id AND user_id::text=$2 AND role='owner') FOR UPDATE`, cloud, user).Scan(&ownership, &authorization)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	return ownership, authorization, err
}
func cloudDeletionEmptyTx(ctx context.Context, tx pgx.Tx, cloud string) error {
	var empty bool
	err := tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM device_item_profiles WHERE brand_cloud_id::text=$1)
        AND NOT EXISTS(SELECT 1 FROM devices WHERE organization_id::text=$1)
        AND NOT EXISTS(SELECT 1 FROM device_operations WHERE organization_id::text=$1 AND (status NOT IN ('succeeded','failed') OR completed_at IS NULL))
        AND NOT EXISTS(SELECT 1 FROM factory_production_runs WHERE brand_cloud_id::text=$1 AND status='active' AND valid_until>now())`, cloud).Scan(&empty)
	if err != nil {
		return err
	}
	if !empty {
		return ErrCloudDeletionBlocked
	}
	return nil
}
func (s *Store) RequestDeveloperCloudDeletion(ctx context.Context, user, cloud, key string) (CloudDeletionOperation, error) {
	if !ValidManagedCloudKey(key) {
		return CloudDeletionOperation{}, ErrInvalidManagedCloudWrite
	}
	// Read/replay before dependency calls. Same key remains usable after deletion;
	// it does not grant any resource permission or create a second operation.
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CloudDeletionOperation{}, err
	}
	defer tx.Rollback(ctx)
	ownership, authorization, err := lockDeletionOwner(ctx, tx, user, cloud)
	if err != nil {
		return CloudDeletionOperation{}, err
	}
	prior, err := scanDeletion(tx.QueryRow(ctx, `SELECT `+deletionColumns+` FROM cloud_deletion_operations WHERE brand_cloud_id::text=$1 AND ((owner_user_id::text=$3 AND idempotency_key=$2) OR phase<>'canceled') ORDER BY (owner_user_id::text=$3 AND idempotency_key=$2) DESC LIMIT 1`, cloud, key, user))
	if err == nil {
		if prior.OwnerUserID != user || prior.Key != key {
			return CloudDeletionOperation{}, ErrConflict
		}
		return prior, tx.Commit(ctx)
	}
	if !errors.Is(err, ErrNotFound) {
		return CloudDeletionOperation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return CloudDeletionOperation{}, err
	}
	if s.deletion == nil {
		return CloudDeletionOperation{}, ErrHandoffUnavailable
	}
	preflight, err := s.PreflightDeveloperBrandCloudDeletion(ctx, user, cloud)
	if err != nil {
		return CloudDeletionOperation{}, err
	}
	if !preflight.Eligible {
		// Another identical request may have installed the fence while this
		// request was observing dependencies. Return its durable result.
		prior, replayErr := scanDeletion(s.db.QueryRow(ctx, `SELECT `+deletionColumns+` FROM cloud_deletion_operations WHERE brand_cloud_id::text=$1 AND idempotency_key=$2 AND owner_user_id::text=$3`, cloud, key, user))
		if replayErr == nil && prior.OwnerUserID == user && prior.Key == key {
			return s.GetDeveloperCloudDeletion(ctx, user, cloud, prior.ID)
		}
		return CloudDeletionOperation{}, &cloudDeletionBlockers{blockers: preflight.Blockers}
	}
	tx, err = s.db.Begin(ctx)
	if err != nil {
		return CloudDeletionOperation{}, err
	}
	defer tx.Rollback(ctx)
	currentOwner, currentAuth, err := lockDeletionOwner(ctx, tx, user, cloud)
	if err != nil {
		return CloudDeletionOperation{}, err
	}
	prior, err = scanDeletion(tx.QueryRow(ctx, `SELECT `+deletionColumns+` FROM cloud_deletion_operations WHERE brand_cloud_id::text=$1 AND ((owner_user_id::text=$3 AND idempotency_key=$2) OR phase<>'canceled') ORDER BY (owner_user_id::text=$3 AND idempotency_key=$2) DESC LIMIT 1`, cloud, key, user))
	if err == nil {
		if prior.OwnerUserID != user || prior.Key != key {
			return CloudDeletionOperation{}, ErrConflict
		}
		return prior, tx.Commit(ctx)
	}
	if !errors.Is(err, ErrNotFound) {
		return CloudDeletionOperation{}, err
	}
	if currentOwner != ownership || currentAuth != authorization {
		return CloudDeletionOperation{}, ErrConflict
	}
	if err = cloudDeletionEmptyTx(ctx, tx, cloud); err != nil {
		return CloudDeletionOperation{}, err
	}
	op := CloudDeletionOperation{CloudID: cloud, OwnerUserID: user, OwnershipVersion: ownership, AuthorizationVersion: authorization, Key: key}
	if err = tx.QueryRow(ctx, `SELECT gen_random_uuid()::text,clock_timestamp()`).Scan(&op.ID, &op.Cutoff); err != nil {
		return op, err
	}
	op.RequestSHA256 = handoffDigest(struct {
		Binding              billinghandoff.ClosureBinding
		AuthorizationVersion int64
		Key                  string
	}{op.binding(), authorization, key})
	op, err = scanDeletion(tx.QueryRow(ctx, `INSERT INTO cloud_deletion_operations(id,brand_cloud_id,owner_user_id,ownership_version,authorization_version,idempotency_key,cutoff,request_sha256)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+deletionColumns, op.ID, cloud, user, ownership, authorization, key, op.Cutoff, op.RequestSHA256))
	if err != nil {
		return op, err
	}
	names := make([]string, 0, len(s.deletion.Producers))
	for name := range s.deletion.Producers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err = tx.Exec(ctx, `INSERT INTO cloud_deletion_participants(operation_id,participant) VALUES($1,$2)`, op.ID, name); err != nil {
			return op, err
		}
	}
	if err = deletionAudit(ctx, tx, op, "requested", op.RequestSHA256); err != nil {
		return op, err
	}
	return op, tx.Commit(ctx)
}
func deletionAudit(ctx context.Context, tx pgx.Tx, op CloudDeletionOperation, action, sha string) error {
	return createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_deletion_" + action, ActorUserID: &op.OwnerUserID, OrganizationID: &op.CloudID, SubjectType: "cloud_deletion", SubjectID: op.ID, Payload: map[string]any{"evidence_sha256": sha, "ownership_version": op.OwnershipVersion}})
}
func (s *Store) loadDeletion(ctx context.Context, cloud, operation string) (CloudDeletionOperation, error) {
	return scanDeletion(s.db.QueryRow(ctx, `SELECT `+deletionColumns+` FROM cloud_deletion_operations WHERE brand_cloud_id::text=$1 AND id::text=$2`, cloud, operation))
}

// Durable resource acknowledgment. No network I/O under an AM transaction.
func (s *Store) recordDeletionHold(ctx context.Context, op CloudDeletionOperation, in CloudDeletionHold) error {
	if in.Binding.CloudDeletionScope != op.binding().CloudDeletionScope || in.Binding.OperationID != op.ID || !in.Binding.Cutoff.Equal(op.Cutoff) || in.AuthorizationVersion != op.AuthorizationVersion+1 || !in.Held || !in.Empty || !handoffEvidenceDigest.MatchString(in.ReceiptSHA256) {
		return ErrHandoffUnavailable
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, _, err = lockDeletionOwner(ctx, tx, op.OwnerUserID, op.CloudID); err != nil {
		return err
	}
	var old string
	err = tx.QueryRow(ctx, `SELECT receipt_sha256 FROM cloud_deletion_resource_receipts WHERE operation_id=$1 AND participant=$2`, op.ID, in.Participant).Scan(&old)
	if err == nil {
		if old != in.ReceiptSHA256 {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO cloud_deletion_resource_receipts(operation_id,participant,receipt_sha256) SELECT id,$2,$3 FROM cloud_deletion_operations WHERE id=$1 AND phase='preparing'`, op.ID, in.Participant, in.ReceiptSHA256); err != nil {
		return err
	}
	if err = deletionAudit(ctx, tx, op, "producer_held", in.ReceiptSHA256); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) prepareDeletionClose(ctx context.Context, op CloudDeletionOperation, receipt string) (string, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	ownership, _, err := lockDeletionOwner(ctx, tx, op.OwnerUserID, op.CloudID)
	if err != nil {
		return "", err
	}
	if ownership != op.OwnershipVersion {
		return "", ErrConflict
	}
	current, err := scanDeletion(tx.QueryRow(ctx, `SELECT `+deletionColumns+` FROM cloud_deletion_operations WHERE id=$1`, op.ID))
	if err != nil {
		return "", err
	}
	if current.Phase != "preparing" && current.Phase != "closing" {
		return "", ErrConflict
	}
	if command, err := activeDeletionClose(ctx, tx, op.ID); err == nil {
		return command.SHA, nil
	} else if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	var operational bool
	if err = tx.QueryRow(ctx, `SELECT user_can_access_brand_cloud_without_handoff($1,$2)`, op.OwnerUserID, op.CloudID).Scan(&operational); err != nil {
		return "", err
	}
	if !operational {
		return "", ErrDisabled
	}
	if err = cloudDeletionEmptyTx(ctx, tx, op.CloudID); err != nil {
		return "", err
	}
	var manifest []byte
	if err = tx.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_array(participant,receipt_sha256) ORDER BY participant),'[]'::jsonb) FROM cloud_deletion_resource_receipts WHERE operation_id=$1`, op.ID).Scan(&manifest); err != nil {
		return "", err
	}
	sha := handoffDigest(struct {
		Binding  billinghandoff.ClosureBinding
		Receipt  string
		Manifest json.RawMessage
	}{op.binding(), receipt, manifest})
	query := `INSERT INTO cloud_deletion_close_commands(operation_id,settlement_id,readiness_sha256) VALUES($1,$2,$3)`
	if current.Phase == "closing" {
		query = `INSERT INTO cloud_deletion_close_attempts(operation_id,settlement_id,readiness_sha256) VALUES($1,$2,$3)`
	}
	if _, err = tx.Exec(ctx, query, op.ID, receipt, sha); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE cloud_deletion_operations SET phase='closing',blockers='[]',updated_at=now() WHERE id=$1 AND phase='preparing'`, op.ID); err != nil {
		return "", err
	}
	if err = deletionAudit(ctx, tx, op, "close_requested", sha); err != nil {
		return "", err
	}
	return sha, tx.Commit(ctx)
}

// Once Billing closes, global owner disablement cannot turn this into a rollback.
// Only the already-authorized, durable command can finish the matching tombstone.
func (s *Store) completeDeletion(ctx context.Context, op CloudDeletionOperation, ack billinghandoff.ClosureAcknowledgment) error {
	if ack.OperationID != op.ID || ack.Phase != "closed" || ack.ClosedAt.IsZero() || !handoffEvidenceDigest.MatchString(ack.ReceiptSHA256) {
		return ErrHandoffUnavailable
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, op.OwnerUserID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, op.CloudID); err != nil {
		return err
	}
	current, err := scanDeletion(tx.QueryRow(ctx, `SELECT `+deletionColumns+` FROM cloud_deletion_operations WHERE id=$1`, op.ID))
	if err != nil {
		return err
	}
	if current.Phase == "succeeded" {
		return nil
	}
	if current.Phase != "closing" && current.Phase != "canceling" {
		return ErrConflict
	}
	if err = cloudDeletionEmptyTx(ctx, tx, op.CloudID); err != nil {
		return err
	}
	var deletedAt time.Time
	if err = tx.QueryRow(ctx, `INSERT INTO cloud_deletion_completions(operation_id,billing_closed_at,billing_receipt_sha256) VALUES($1,$2,$3) RETURNING deleted_at`, op.ID, ack.ClosedAt, ack.ReceiptSHA256).Scan(&deletedAt); err != nil {
		return err
	}
	// Membership/ACL invalidation runs before the tombstone so its existing
	// authorization-version triggers can update the still-live organization row.
	for _, query := range []string{
		`UPDATE organization_members SET disabled_at=COALESCE(disabled_at,now()) WHERE organization_id=$1`,
		`UPDATE brand_cloud_member_invitations SET status='canceled',canceled_at=now(),updated_at=now() WHERE brand_cloud_id=$1 AND status='pending'`,
		`UPDATE brand_cloud_owner_transfers SET status='canceled',canceled_at=now(),updated_at=now() WHERE brand_cloud_id=$1 AND status='pending'`,
		`UPDATE role_assignments SET disabled_at=COALESCE(disabled_at,now()) WHERE organization_id=$1`,
		`UPDATE external_group_mappings SET disabled_at=COALESCE(disabled_at,now()) WHERE organization_id=$1`,
		`UPDATE device_claim_tokens SET revoked_at=COALESCE(revoked_at,now()) WHERE organization_id=$1 AND claimed_at IS NULL`,
	} {
		if _, err = tx.Exec(ctx, query, op.CloudID); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE organizations SET deleted_at=$2,authorization_version=authorization_version+1,updated_at=now() WHERE id=$1`, op.CloudID, deletedAt); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE cloud_deletion_operations SET phase='succeeded',blockers='[]',updated_at=now() WHERE id=$1`, op.ID); err != nil {
		return err
	}
	if err = deletionAudit(ctx, tx, op, "succeeded", ack.ReceiptSHA256); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// One recoverable coordinator attempt. A worker retries the persisted operation,
// never manufactures evidence or changes command content after ambiguous delivery.
func (s *Store) AdvanceCloudDeletion(ctx context.Context, cloud, operation string) (CloudDeletionOperation, error) {
	op, err := s.advanceCloudDeletion(ctx, cloud, operation)
	if err != nil && op.ID != "" {
		code := "evidence_unavailable"
		if errors.Is(err, ErrCloudDeletionBlocked) {
			code = "lifecycle_conflict"
		}
		safe := []model.CloudBlocker{{Code: code, Retryable: true}}
		var detailed *cloudDeletionBlockers
		if errors.As(err, &detailed) {
			safe = detailed.blockers
		}
		blockers, _ := json.Marshal(safe)
		// Scheduling/status metadata cannot change a durable phase or decision.
		tx, writeErr := s.db.Begin(ctx)
		if writeErr != nil {
			return op, writeErr
		}
		defer tx.Rollback(ctx)
		// Same user -> cloud -> operation order as admission/finalization.
		if _, writeErr = tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, op.OwnerUserID); writeErr != nil {
			return op, writeErr
		}
		if _, writeErr = tx.Exec(ctx, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, op.CloudID); writeErr != nil {
			return op, writeErr
		}
		_, writeErr = tx.Exec(ctx, `UPDATE cloud_deletion_operations SET blockers=$3,updated_at=now() WHERE id::text=$1 AND brand_cloud_id::text=$2 AND phase NOT IN ('succeeded','canceled')`, operation, cloud, blockers)
		if writeErr != nil {
			return op, writeErr
		}
		if writeErr = tx.Commit(ctx); writeErr != nil {
			return op, writeErr
		}
		if current, readErr := s.loadDeletion(ctx, cloud, operation); readErr == nil {
			op = current
		}
	}
	return op, err
}
func (s *Store) advanceCloudDeletion(ctx context.Context, cloud, operation string) (CloudDeletionOperation, error) {
	op, err := s.loadDeletion(ctx, cloud, operation)
	if err != nil || op.Phase == "succeeded" || op.Phase == "canceled" {
		return op, err
	}
	if s.deletion == nil {
		return op, ErrHandoffUnavailable
	}
	if op.Phase == "canceling" {
		return s.advanceDeletionCancellation(ctx, op)
	}
	if op.Phase == "preparing" {
		prepared, err := s.deletion.Billing.PrepareCloudClosure(ctx, op.binding(), op.RequestSHA256)
		if err != nil {
			return op, err
		}
		if prepared.ID != op.ID || prepared.OwnerUserID != op.OwnerUserID || prepared.OwnershipVersion != op.OwnershipVersion || prepared.Phase != "preparing" {
			return op, ErrHandoffUnavailable
		}
		rows, err := s.db.Query(ctx, `SELECT p.participant FROM cloud_deletion_participants p WHERE operation_id=$1 AND NOT EXISTS(SELECT 1 FROM cloud_deletion_resource_receipts r WHERE r.operation_id=p.operation_id AND r.participant=p.participant) ORDER BY participant`, op.ID)
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
			producer := s.deletion.Producers[name]
			if producer == nil {
				return op, ErrHandoffUnavailable
			}
			hold, err := producer.PrepareCloudDeletion(ctx, op.binding(), op.AuthorizationVersion+1)
			if err != nil {
				return op, err
			}
			if hold.Participant != name {
				return op, ErrHandoffUnavailable
			}
			if err = s.recordDeletionHold(ctx, op, hold); err != nil {
				return op, err
			}
		}
		status, err := s.deletion.Billing.CloudClosureStatus(ctx, op.binding())
		if err != nil {
			return op, err
		}
		if !status.Ready || status.ReceiptID == "" {
			return op, &cloudDeletionBlockers{blockers: closureFinancialBlockers(status.Blockers)}
		}
		if _, err = s.prepareDeletionClose(ctx, op, status.ReceiptID); err != nil {
			return op, err
		}
	}
	command, err := activeDeletionClose(ctx, s.db, op.ID)
	if errors.Is(err, ErrNotFound) {
		status, statusErr := s.deletion.Billing.CloudClosureStatus(ctx, op.binding())
		if statusErr != nil {
			return op, statusErr
		}
		if !status.Ready || status.ReceiptID == "" {
			return op, &cloudDeletionBlockers{blockers: closureFinancialBlockers(status.Blockers)}
		}
		if _, err = s.prepareDeletionClose(ctx, op, status.ReceiptID); err != nil {
			return op, err
		}
		command, err = activeDeletionClose(ctx, s.db, op.ID)
	}
	if err != nil {
		return op, err
	}
	ack, err := s.deletion.Billing.CloseCloud(ctx, op.binding(), command.Receipt, command.SHA)
	if err != nil {
		var httpErr *billinghandoff.HTTPError
		if errors.As(err, &httpErr) && httpErr.Status == 409 && (httpErr.Code == "BILLING_CLOSURE_NOT_READY" || httpErr.Code == "BILLING_CLOSURE_COMMAND_RETIRED") {
			closed, resolveErr := s.resolveDeletionClose(ctx, op, command)
			if resolveErr != nil {
				return op, resolveErr
			}
			if closed {
				return s.loadDeletion(ctx, cloud, operation)
			}
			return op, ErrHandoffSnapshotNotReady
		}
		return op, err
	}
	if err = s.completeDeletion(ctx, op, ack); err != nil {
		return op, err
	}
	return s.loadDeletion(ctx, cloud, operation)
}
