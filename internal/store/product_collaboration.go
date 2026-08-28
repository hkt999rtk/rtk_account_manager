package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

const (
	ProductOwnerRole  = "product_owner"
	ProductEditorRole = "product_editor"
	ProductViewerRole = "product_viewer"
)

type ProductCollaboratorInvitationInput struct {
	BrandCloudID, ProductID, InvitedByUserID, TargetEmail, Role, TokenHash string
	ExpiresAt                                                              time.Time
	Email                                                                  *EmailOutboxInput
}

type ProductCollaboratorInvitationMutation struct {
	BrandCloudID, ProductID, InvitationID, ActorUserID, TokenHash string
	ExpiresAt                                                     time.Time
	Email                                                         *EmailOutboxInput
}

func validProductCollaboratorRole(role string) bool {
	return role == ProductEditorRole || role == ProductViewerRole
}

func (s *Store) CanManageProductCollaborators(ctx context.Context, actorUserID, brandCloudID, productID string) (bool, error) {
	var allowed bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM device_item_profiles dip
			JOIN role_assignments ra ON ra.actor_type = 'user' AND ra.actor_id = $1
			JOIN roles r ON r.id = ra.role_id AND r.name = 'product_owner' AND r.disabled_at IS NULL
			WHERE dip.id::text = $3 AND dip.brand_cloud_id::text = $2
			  AND ra.scope_type = 'product' AND ra.scope_id = dip.id::text AND ra.disabled_at IS NULL
		)
	`, strings.TrimSpace(actorUserID), strings.TrimSpace(brandCloudID), strings.TrimSpace(productID)).Scan(&allowed)
	return allowed, err
}

func (s *Store) GetProductCollaboratorRole(ctx context.Context, brandCloudUserID, brandCloudID, productID string) (string, error) {
	var role string
	err := s.db.QueryRow(ctx, `
		SELECT CASE WHEN r.name='owner' AND ra.scope_type='organization' THEN 'brand_owner' ELSE r.name END
		FROM role_assignments ra JOIN roles r ON r.id=ra.role_id AND r.disabled_at IS NULL
		WHERE ra.actor_type='brand_cloud_user' AND ra.actor_id=$1 AND ra.organization_id::text=$2
		  AND ra.disabled_at IS NULL AND (ra.scope_type='organization' OR (ra.scope_type='product' AND ra.scope_id=$3))
		ORDER BY CASE WHEN r.name='product_owner' THEN 0 WHEN ra.scope_type='organization' THEN 1 WHEN r.name='product_editor' THEN 2 ELSE 3 END
		LIMIT 1
	`, brandCloudUserID, brandCloudID, productID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}

func (s *Store) GetUserProductCollaboratorRole(ctx context.Context, userID, brandCloudID, productID string) (string, error) {
	var role string
	err := s.db.QueryRow(ctx, `SELECT CASE WHEN r.name='owner' AND ra.scope_type='organization' THEN 'brand_owner' ELSE r.name END
		FROM role_assignments ra JOIN roles r ON r.id=ra.role_id AND r.disabled_at IS NULL
		WHERE ra.actor_type='user' AND ra.actor_id=$1 AND ra.organization_id::text=$2 AND ra.disabled_at IS NULL
		AND (ra.scope_type='organization' OR (ra.scope_type='product' AND ra.scope_id=$3))
		ORDER BY CASE WHEN r.name='product_owner' THEN 0 WHEN ra.scope_type='organization' THEN 1 WHEN r.name='product_editor' THEN 2 ELSE 3 END LIMIT 1`, userID, brandCloudID, productID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}

func (s *Store) ListProductCollaborators(ctx context.Context, brandCloudID, productID string) ([]model.ProductCollaborator, error) {
	rows, err := s.db.Query(ctx, `
		SELECT ra.id::text, ra.scope_id, u.id::text, u.email, u.display_name, r.name,
		       u.disabled_at, ra.created_at
		FROM role_assignments ra
		JOIN roles r ON r.id = ra.role_id AND r.name IN ('product_owner', 'product_editor', 'product_viewer')
		JOIN users u ON u.id::text = ra.actor_id
		WHERE ra.actor_type = 'user' AND ra.scope_type = 'product'
		  AND ra.scope_id = $2 AND ra.organization_id::text = $1 AND ra.disabled_at IS NULL
		ORDER BY CASE r.name WHEN 'product_owner' THEN 0 WHEN 'product_editor' THEN 1 ELSE 2 END,
		         lower(u.email)
	`, strings.TrimSpace(brandCloudID), strings.TrimSpace(productID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []model.ProductCollaborator{}
	for rows.Next() {
		var item model.ProductCollaborator
		if err := rows.Scan(&item.AssignmentID, &item.ProductID, &item.UserID, &item.Email, &item.DisplayName, &item.Role, &item.DisabledAt, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) CreateProductCollaboratorInvitation(ctx context.Context, in ProductCollaboratorInvitationInput, now time.Time) (model.ProductCollaboratorInvitation, bool, error) {
	if !validProductCollaboratorRole(strings.TrimSpace(in.Role)) {
		return model.ProductCollaboratorInvitation{}, false, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.ProductCollaboratorInvitation{}, false, err
	}
	defer tx.Rollback(ctx)
	var canManage bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM device_item_profiles dip
			JOIN role_assignments ra ON ra.actor_id=$1 AND ra.actor_type='user'
			JOIN roles r ON r.id=ra.role_id AND r.name='product_owner'
			WHERE dip.id::text=$3 AND dip.brand_cloud_id::text=$2 AND ra.scope_type='product'
			  AND ra.scope_id=dip.id::text AND ra.disabled_at IS NULL
		)
	`, in.InvitedByUserID, in.BrandCloudID, in.ProductID).Scan(&canManage); err != nil || !canManage {
		if err == nil {
			err = ErrNotFound
		}
		return model.ProductCollaboratorInvitation{}, false, err
	}
	target, err := getDeveloperUserByEmailTx(ctx, tx, in.TargetEmail)
	if err != nil || !target.EmailVerified || target.SignupPendingVerification {
		if err == nil {
			err = ErrNotFound
		}
		return model.ProductCollaboratorInvitation{}, false, err
	}
	var alreadyAssigned bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM role_assignments ra
		WHERE ra.actor_type='user' AND ra.actor_id=$2 AND ra.organization_id::text=$1 AND ra.scope_type='product'
		  AND ra.scope_id=$3 AND ra.disabled_at IS NULL
	)`, in.BrandCloudID, target.ID, in.ProductID).Scan(&alreadyAssigned); err != nil {
		return model.ProductCollaboratorInvitation{}, false, err
	}
	if alreadyAssigned {
		return model.ProductCollaboratorInvitation{}, false, ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE product_collaborator_invitations SET status='expired', updated_at=$2 WHERE product_id::text=$1 AND status='pending' AND expires_at <= $2`, in.ProductID, now); err != nil {
		return model.ProductCollaboratorInvitation{}, false, err
	}
	invitation, err := scanProductCollaboratorInvitation(tx.QueryRow(ctx, `
		INSERT INTO product_collaborator_invitations
		(brand_cloud_id, product_id, invited_by_user_id, target_user_id, target_email, role, token_hash, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id::text, brand_cloud_id::text, product_id::text, invited_by_user_id::text,
		          target_user_id::text, target_email, role, status, expires_at,
		          accepted_at, canceled_at, created_at, updated_at
	`, in.BrandCloudID, in.ProductID, in.InvitedByUserID, target.ID, target.Email, in.Role, in.TokenHash, in.ExpiresAt))
	if err != nil {
		if isUniqueViolation(err) {
			return model.ProductCollaboratorInvitation{}, false, ErrConflict
		}
		return model.ProductCollaboratorInvitation{}, false, err
	}
	if in.Email != nil {
		email := *in.Email
		email.Payload.RecipientEmail = invitation.TargetEmail
		if err := s.enqueueEmailTx(ctx, tx, email); err != nil {
			return model.ProductCollaboratorInvitation{}, false, err
		}
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "product_collaborator_invitation_created", ActorUserID: &in.InvitedByUserID, OrganizationID: &in.BrandCloudID, SubjectType: "product_collaborator_invitation", SubjectID: invitation.ID, Payload: map[string]any{"product_id": in.ProductID, "target_user_id": target.ID, "role": in.Role}}); err != nil {
		return model.ProductCollaboratorInvitation{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.ProductCollaboratorInvitation{}, false, err
	}
	return invitation, true, nil
}

func (s *Store) ListProductCollaboratorInvitations(ctx context.Context, brandCloudID, productID string, now time.Time) ([]model.ProductCollaboratorInvitation, error) {
	if _, err := s.db.Exec(ctx, `UPDATE product_collaborator_invitations SET status='expired', updated_at=$3 WHERE brand_cloud_id::text=$1 AND product_id::text=$2 AND status='pending' AND expires_at <= $3`, brandCloudID, productID, now); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT id::text, brand_cloud_id::text, product_id::text, invited_by_user_id::text,
		target_user_id::text, target_email, role, status, expires_at, accepted_at, canceled_at, created_at, updated_at
		FROM product_collaborator_invitations WHERE brand_cloud_id::text=$1 AND product_id::text=$2 ORDER BY created_at DESC`, brandCloudID, productID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []model.ProductCollaboratorInvitation{}
	for rows.Next() {
		item, err := scanProductCollaboratorInvitation(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) ResendProductCollaboratorInvitation(ctx context.Context, in ProductCollaboratorInvitationMutation, now time.Time) (model.ProductCollaboratorInvitation, error) {
	allowed, err := s.CanManageProductCollaborators(ctx, in.ActorUserID, in.BrandCloudID, in.ProductID)
	if err != nil || !allowed {
		if err == nil {
			err = ErrNotFound
		}
		return model.ProductCollaboratorInvitation{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.ProductCollaboratorInvitation{}, err
	}
	defer tx.Rollback(ctx)
	item, err := scanProductCollaboratorInvitation(tx.QueryRow(ctx, `UPDATE product_collaborator_invitations SET token_hash=$4, expires_at=$5, updated_at=$6
		WHERE id::text=$3 AND brand_cloud_id::text=$1 AND product_id::text=$2 AND status='pending'
		RETURNING id::text,brand_cloud_id::text,product_id::text,invited_by_user_id::text,target_user_id::text,target_email,role,status,expires_at,accepted_at,canceled_at,created_at,updated_at`, in.BrandCloudID, in.ProductID, in.InvitationID, in.TokenHash, in.ExpiresAt, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ProductCollaboratorInvitation{}, ErrNotFound
	}
	if err != nil {
		return model.ProductCollaboratorInvitation{}, err
	}
	if in.Email != nil {
		email := *in.Email
		email.Payload.RecipientEmail = item.TargetEmail
		if err := s.enqueueEmailTx(ctx, tx, email); err != nil {
			return model.ProductCollaboratorInvitation{}, err
		}
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "product_collaborator_invitation_resent", ActorUserID: &in.ActorUserID, OrganizationID: &in.BrandCloudID, SubjectType: "product_collaborator_invitation", SubjectID: item.ID, Payload: map[string]any{"product_id": in.ProductID}}); err != nil {
		return model.ProductCollaboratorInvitation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.ProductCollaboratorInvitation{}, err
	}
	return item, nil
}

func (s *Store) CancelProductCollaboratorInvitation(ctx context.Context, in ProductCollaboratorInvitationMutation, now time.Time) (model.ProductCollaboratorInvitation, error) {
	allowed, err := s.CanManageProductCollaborators(ctx, in.ActorUserID, in.BrandCloudID, in.ProductID)
	if err != nil || !allowed {
		if err == nil {
			err = ErrNotFound
		}
		return model.ProductCollaboratorInvitation{}, err
	}
	item, err := scanProductCollaboratorInvitation(s.db.QueryRow(ctx, `UPDATE product_collaborator_invitations SET status='canceled',canceled_at=$4,updated_at=$4
		WHERE brand_cloud_id::text=$1 AND product_id::text=$2 AND id::text=$3 AND status='pending'
		RETURNING id::text,brand_cloud_id::text,product_id::text,invited_by_user_id::text,target_user_id::text,target_email,role,status,expires_at,accepted_at,canceled_at,created_at,updated_at`, in.BrandCloudID, in.ProductID, in.InvitationID, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ProductCollaboratorInvitation{}, ErrNotFound
	}
	if err == nil {
		_ = s.CreateAuditEvent(ctx, AuditEventInput{EventType: "product_collaborator_invitation_canceled", ActorUserID: &in.ActorUserID, OrganizationID: &in.BrandCloudID, SubjectType: "product_collaborator_invitation", SubjectID: item.ID, Payload: map[string]any{"product_id": in.ProductID}})
	}
	return item, err
}

func (s *Store) AcceptProductCollaboratorInvitation(ctx context.Context, targetUserID, tokenHash string, now time.Time) (model.ProductCollaboratorInvitation, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.ProductCollaboratorInvitation{}, err
	}
	defer tx.Rollback(ctx)
	invitation, err := scanProductCollaboratorInvitation(tx.QueryRow(ctx, `SELECT i.id::text, i.brand_cloud_id::text, i.product_id::text,
		i.invited_by_user_id::text, i.target_user_id::text, i.target_email, i.role, i.status, i.expires_at,
		i.accepted_at, i.canceled_at, i.created_at, i.updated_at
		FROM product_collaborator_invitations i JOIN users u ON u.id=i.target_user_id
		WHERE i.token_hash=$1 AND i.target_user_id::text=$2 AND i.status='pending'
		  AND u.disabled_at IS NULL AND u.email_verified=true AND u.signup_pending_verification=false
		FOR UPDATE`, tokenHash, targetUserID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ProductCollaboratorInvitation{}, ErrNotFound
	}
	if err != nil {
		return model.ProductCollaboratorInvitation{}, err
	}
	if !invitation.ExpiresAt.After(now) {
		_, _ = tx.Exec(ctx, `UPDATE product_collaborator_invitations SET status='expired', updated_at=$2 WHERE id=$1`, invitation.ID, now)
		_ = tx.Commit(ctx)
		return model.ProductCollaboratorInvitation{}, ErrNotFound
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_members (organization_id,user_id,role) VALUES ($1,$2,'member')
		ON CONFLICT (organization_id,user_id) DO UPDATE SET disabled_at=NULL, updated_at=now()`, invitation.BrandCloudID, targetUserID); err != nil {
		return model.ProductCollaboratorInvitation{}, err
	}
	var roleID string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM roles WHERE name=$1 AND disabled_at IS NULL`, invitation.Role).Scan(&roleID); err != nil {
		return model.ProductCollaboratorInvitation{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO role_assignments (role_id,actor_type,actor_id,scope_type,scope_id,organization_id)
		VALUES ($1,'user',$2,'product',$3,$4)`, roleID, targetUserID, invitation.ProductID, invitation.BrandCloudID); err != nil {
		return model.ProductCollaboratorInvitation{}, err
	}
	accepted, err := scanProductCollaboratorInvitation(tx.QueryRow(ctx, `UPDATE product_collaborator_invitations
		SET status='accepted', accepted_at=$2, updated_at=$2 WHERE id=$1
		RETURNING id::text, brand_cloud_id::text, product_id::text, invited_by_user_id::text,
		target_user_id::text, target_email, role, status, expires_at, accepted_at, canceled_at, created_at, updated_at`, invitation.ID, now))
	if err != nil {
		return model.ProductCollaboratorInvitation{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "product_collaborator_invitation_accepted", ActorUserID: &targetUserID, OrganizationID: &invitation.BrandCloudID, SubjectType: "product", SubjectID: invitation.ProductID, Payload: map[string]any{"role": invitation.Role}}); err != nil {
		return model.ProductCollaboratorInvitation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.ProductCollaboratorInvitation{}, err
	}
	return accepted, nil
}

func (s *Store) UpdateProductCollaborator(ctx context.Context, actorUserID, brandCloudID, productID, targetUserID, role string) (model.ProductCollaborator, error) {
	if !validProductCollaboratorRole(role) {
		return model.ProductCollaborator{}, ErrConflict
	}
	allowed, err := s.CanManageProductCollaborators(ctx, actorUserID, brandCloudID, productID)
	if err != nil || !allowed {
		if err == nil {
			err = ErrNotFound
		}
		return model.ProductCollaborator{}, err
	}
	var roleID string
	if err := s.db.QueryRow(ctx, `SELECT id::text FROM roles WHERE name=$1 AND disabled_at IS NULL`, role).Scan(&roleID); err != nil {
		return model.ProductCollaborator{}, err
	}
	var assignmentID string
	err = s.db.QueryRow(ctx, `UPDATE role_assignments ra SET role_id=$1, updated_at=now()
		FROM roles current_product_role
		WHERE ra.actor_type='user' AND ra.actor_id=$2 AND ra.role_id=current_product_role.id
		  AND ra.organization_id::text=$3 AND ra.scope_type='product' AND ra.scope_id=$4
		  AND current_product_role.name IN ('product_editor','product_viewer') AND ra.disabled_at IS NULL
		RETURNING ra.id::text`, roleID, targetUserID, brandCloudID, productID).Scan(&assignmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ProductCollaborator{}, ErrNotFound
	}
	if err != nil {
		return model.ProductCollaborator{}, err
	}
	items, err := s.ListProductCollaborators(ctx, brandCloudID, productID)
	if err != nil {
		return model.ProductCollaborator{}, err
	}
	for _, item := range items {
		if item.AssignmentID == assignmentID {
			_ = s.CreateAuditEvent(ctx, AuditEventInput{EventType: "product_collaborator_role_changed", ActorUserID: &actorUserID, OrganizationID: &brandCloudID, SubjectType: "product", SubjectID: productID, Payload: map[string]any{"target_user_id": targetUserID, "role": role}})
			return item, nil
		}
	}
	return model.ProductCollaborator{}, ErrNotFound
}

func (s *Store) RemoveProductCollaborator(ctx context.Context, actorUserID, brandCloudID, productID, targetUserID string) error {
	allowed, err := s.CanManageProductCollaborators(ctx, actorUserID, brandCloudID, productID)
	if err != nil || !allowed {
		if err == nil {
			err = ErrNotFound
		}
		return err
	}
	tag, err := s.db.Exec(ctx, `UPDATE role_assignments ra SET disabled_at=now(), updated_at=now()
		FROM roles r WHERE ra.actor_type='user' AND ra.actor_id=$1 AND ra.role_id=r.id
		  AND ra.organization_id::text=$2 AND ra.scope_type='product' AND ra.scope_id=$3
		  AND r.name IN ('product_editor','product_viewer') AND ra.disabled_at IS NULL`, targetUserID, brandCloudID, productID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	_ = s.CreateAuditEvent(ctx, AuditEventInput{EventType: "product_collaborator_removed", ActorUserID: &actorUserID, OrganizationID: &brandCloudID, SubjectType: "product", SubjectID: productID, Payload: map[string]any{"target_user_id": targetUserID}})
	return nil
}

func (s *Store) TransferProductOwnership(ctx context.Context, actorUserID, brandCloudID, productID, targetUserID string) error {
	allowed, err := s.CanManageProductCollaborators(ctx, actorUserID, brandCloudID, productID)
	if err != nil || !allowed {
		if err == nil {
			err = ErrNotFound
		}
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM device_item_profiles WHERE id::text=$1 AND brand_cloud_id::text=$2 FOR UPDATE`, productID, brandCloudID); err != nil {
		return err
	}
	var ownerRoleID, editorRoleID string
	if err := tx.QueryRow(ctx, `SELECT max(id::text) FILTER (WHERE name='product_owner'), max(id::text) FILTER (WHERE name='product_editor') FROM roles WHERE name IN ('product_owner','product_editor') AND disabled_at IS NULL`).Scan(&ownerRoleID, &editorRoleID); err != nil {
		return err
	}
	var targetAssignmentID string
	if err := tx.QueryRow(ctx, `SELECT ra.id::text FROM role_assignments ra JOIN roles r ON r.id=ra.role_id
		WHERE ra.actor_type='user' AND ra.actor_id=$1 AND ra.organization_id::text=$2 AND ra.scope_type='product' AND ra.scope_id=$3
		AND r.name IN ('product_editor','product_viewer') AND ra.disabled_at IS NULL FOR UPDATE`, targetUserID, brandCloudID, productID).Scan(&targetAssignmentID); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE role_assignments SET role_id=$1, updated_at=now()
		WHERE actor_type='user' AND organization_id::text=$4 AND scope_type='product' AND scope_id=$2 AND role_id=$3 AND disabled_at IS NULL`, editorRoleID, productID, ownerRoleID, brandCloudID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE role_assignments SET role_id=$1, updated_at=now() WHERE id=$2`, ownerRoleID, targetAssignmentID); err != nil {
		return err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "product_owner_transferred", ActorUserID: &actorUserID, OrganizationID: &brandCloudID, SubjectType: "product", SubjectID: productID, Payload: map[string]any{"target_user_id": targetUserID}}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func scanProductCollaboratorInvitation(row scanner) (model.ProductCollaboratorInvitation, error) {
	var item model.ProductCollaboratorInvitation
	err := row.Scan(&item.ID, &item.BrandCloudID, &item.ProductID, &item.InvitedByUserID, &item.TargetUserID,
		&item.TargetEmail, &item.Role, &item.Status, &item.ExpiresAt, &item.AcceptedAt, &item.CanceledAt,
		&item.CreatedAt, &item.UpdatedAt)
	return item, err
}
