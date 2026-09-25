package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

type OwnerTransferQuota struct {
	Limit     int `json:"owner_transfer_limit"`
	Used      int `json:"owner_transfer_used"`
	Remaining int `json:"owner_transfer_remaining"`
}

// Older accepted invitations changed ownership immediately. Newer accepted
// invitations only start a handoff, so count their durable commit decisions.
const ownerTransferUsedSQL = `(SELECT count(*) FROM cloud_handoff_committed_decisions d
    JOIN cloud_ownership_handoffs h ON h.id=d.operation_id WHERE h.brand_cloud_id=o.id)
    + (SELECT count(*) FROM brand_cloud_owner_transfers t
       WHERE t.brand_cloud_id=o.id AND t.status='accepted' AND t.ownership_version IS NULL)`

func ownerTransferQuota(ctx context.Context, q rowQuerier, cloudID string) (OwnerTransferQuota, error) {
	var quota OwnerTransferQuota
	err := q.QueryRow(ctx, `SELECT o.owner_transfer_limit, `+ownerTransferUsedSQL+`
		FROM organizations o WHERE o.id::text=$1 AND o.organization_kind='brand_cloud' AND o.deleted_at IS NULL`, cloudID).Scan(&quota.Limit, &quota.Used)
	if errors.Is(err, pgx.ErrNoRows) {
		return quota, ErrNotFound
	}
	if err != nil {
		return quota, err
	}
	quota.Remaining = max(quota.Limit-quota.Used, 0)
	return quota, nil
}

func (s *Store) GetOwnerTransferQuota(ctx context.Context, cloudID string) (OwnerTransferQuota, error) {
	return ownerTransferQuota(ctx, s.db, cloudID)
}

func (s *Store) SetOwnerTransferLimit(ctx context.Context, actorID, cloudID string, limit int) (OwnerTransferQuota, error) {
	if limit < 0 || limit > 200 {
		return OwnerTransferQuota{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return OwnerTransferQuota{}, err
	}
	defer tx.Rollback(ctx)
	var previous int
	err = tx.QueryRow(ctx, `SELECT owner_transfer_limit FROM organizations
		WHERE id::text=$1 AND organization_kind='brand_cloud' AND deleted_at IS NULL FOR UPDATE`, cloudID).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return OwnerTransferQuota{}, ErrNotFound
	}
	if err != nil {
		return OwnerTransferQuota{}, err
	}
	if previous != limit {
		if _, err = tx.Exec(ctx, `UPDATE organizations SET owner_transfer_limit=$2,updated_at=now() WHERE id::text=$1`, cloudID, limit); err != nil {
			return OwnerTransferQuota{}, err
		}
		if err = createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_owner_transfer_limit_updated", ActorUserID: &actorID,
			OrganizationID: &cloudID, SubjectType: "brand_cloud", SubjectID: cloudID,
			Payload: map[string]any{"previous_limit": previous, "owner_transfer_limit": limit}}); err != nil {
			return OwnerTransferQuota{}, err
		}
	}
	quota, err := ownerTransferQuota(ctx, tx, cloudID)
	if err != nil {
		return OwnerTransferQuota{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return OwnerTransferQuota{}, err
	}
	return quota, nil
}

func checkOwnerTransferQuota(ctx context.Context, q rowQuerier, cloudID string) error {
	quota, err := ownerTransferQuota(ctx, q, cloudID)
	if err != nil {
		return err
	}
	if quota.Remaining == 0 {
		return ErrOwnerTransferLimitReached
	}
	return nil
}
