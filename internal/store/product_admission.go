package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// All Product collaboration mutations serialize on the cloud. User locks come
// first, matching ownership/quota writes. Membership revocation takes the same
// cloud lock via the membership trigger, so admission cannot race removal.
func lockProductCollaborationTx(ctx context.Context, tx pgx.Tx, cloudID string, userIDs ...string) error {
	rows, err := tx.Query(ctx, `SELECT id FROM users WHERE id::text=ANY($1::text[]) ORDER BY id FOR UPDATE`, userIDs)
	if err != nil {
		return err
	}
	for rows.Next() {
	} // Drain the lock query before acquiring the cloud lock.
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var id string
	err = tx.QueryRow(ctx, `SELECT id::text FROM organizations WHERE id::text=$1 AND organization_kind='brand_cloud' AND deleted_at IS NULL FOR UPDATE`, cloudID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

const canManageProductCollaboratorsSQL = `SELECT EXISTS (
    SELECT 1 FROM organization_members m
    WHERE m.organization_id::text=$2 AND m.user_id::text=$1
      AND user_can_access_brand_cloud_product($1,$2,$3)
      AND (m.role='owner' OR EXISTS (
          SELECT 1 FROM role_assignments ra JOIN roles r ON r.id=ra.role_id
          WHERE ra.actor_type='user' AND ra.actor_id=$1 AND ra.organization_id=m.organization_id
            AND ra.scope_type='product' AND ra.scope_id=$3 AND ra.disabled_at IS NULL
            AND r.name='product_owner' AND r.disabled_at IS NULL
      ))
)`

func requireProductManagerTx(ctx context.Context, tx pgx.Tx, actor, cloud, product string) error {
	var allowed bool
	if err := tx.QueryRow(ctx, canManageProductCollaboratorsSQL, actor, cloud, product).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return ErrNotFound
	}
	return nil
}

func lockProductInvitationMutationTx(ctx context.Context, tx pgx.Tx, in ProductCollaboratorInvitationMutation) (string, error) {
	var target string
	err := tx.QueryRow(ctx, `SELECT target_user_id::text FROM product_collaborator_invitations WHERE id::text=$1 AND brand_cloud_id::text=$2 AND product_id::text=$3`, in.InvitationID, in.BrandCloudID, in.ProductID).Scan(&target)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if err := lockProductCollaborationTx(ctx, tx, in.BrandCloudID, in.ActorUserID, target); err != nil {
		return "", err
	}
	if err := requireProductManagerTx(ctx, tx, in.ActorUserID, in.BrandCloudID, in.ProductID); err != nil {
		return "", err
	}
	return target, nil
}

// A cloud owner can approve a new Product scope for an already admitted cloud
// member. A delegated Product owner can only invite within existing approval.
// This check never creates or re-enables a membership.
func productInvitationAdmissionTx(ctx context.Context, tx pgx.Tx, actor, target, cloud, product string) (bool, error) {
	var cloudOwner, admitted bool
	err := tx.QueryRow(ctx, `SELECT m.role='owner',user_can_access_brand_cloud_product($2,$3,$4)
        FROM organization_members m WHERE m.organization_id::text=$3 AND m.user_id::text=$1
        AND user_can_access_brand_cloud($1,$3) AND user_can_access_brand_cloud($2,$3)`, actor, target, cloud, product).Scan(&cloudOwner, &admitted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if !cloudOwner && !admitted {
		return false, ErrNotFound
	}
	return cloudOwner, nil
}
