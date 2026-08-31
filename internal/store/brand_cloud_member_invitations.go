package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

func requireBrandCloudOwnerTx(ctx context.Context, tx pgx.Tx, brandCloudID, userID string) error {
	var role model.Role
	err := tx.QueryRow(ctx, `
		SELECT m.role
		FROM organization_members m
		JOIN organizations o ON o.id = m.organization_id
		JOIN users u ON u.id = m.user_id
		WHERE m.organization_id = $1 AND m.user_id = $2
		  AND o.organization_kind = 'brand_cloud'
		  AND user_can_access_brand_cloud(u.id::text,o.id::text)
	`, brandCloudID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if role != model.RoleOwner {
		return ErrNotFound
	}
	return nil
}

func lockCloudInvitationMutationTx(ctx context.Context, tx pgx.Tx, in BrandCloudMemberInvitationMutation) error {
	var target string
	err := tx.QueryRow(ctx, `SELECT target_user_id::text FROM brand_cloud_member_invitations WHERE id::text=$1 AND brand_cloud_id::text=$2`, in.InvitationID, in.BrandCloudID).Scan(&target)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if err := lockBrandCloudCollaborationTx(ctx, tx, in.BrandCloudID, in.ActorUserID, target); err != nil {
		return err
	}
	return requireBrandCloudOwnerTx(ctx, tx, in.BrandCloudID, in.ActorUserID)
}

func expireBrandCloudMemberInvitationsTx(ctx context.Context, tx pgx.Tx, brandCloudID string, now time.Time) (int64, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE brand_cloud_member_invitations
		SET status = 'expired', updated_at = now()
		WHERE brand_cloud_id = $1 AND status = 'pending' AND expires_at <= $2
	`, brandCloudID, now)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) CreateBrandCloudMemberInvitation(ctx context.Context, in BrandCloudMemberInvitationInput, now time.Time) (model.BrandCloudMemberInvitation, bool, error) {
	if in.Role != model.RoleAdmin && in.Role != model.RoleMember {
		return model.BrandCloudMemberInvitation{}, false, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.BrandCloudMemberInvitation{}, false, err
	}
	defer tx.Rollback(ctx)

	var targetID string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM users WHERE email=$1`, strings.ToLower(strings.TrimSpace(in.TargetEmail))).Scan(&targetID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return model.BrandCloudMemberInvitation{}, false, err
	}
	if err := lockBrandCloudCollaborationTx(ctx, tx, in.BrandCloudID, in.InvitedByUserID, targetID); err != nil {
		return model.BrandCloudMemberInvitation{}, false, err
	}
	if err := requireBrandCloudOwnerTx(ctx, tx, in.BrandCloudID, in.InvitedByUserID); err != nil {
		return model.BrandCloudMemberInvitation{}, false, err
	}
	target, err := getDeveloperUserByEmailTx(ctx, tx, in.TargetEmail)
	if err != nil || !target.EmailVerified || target.SignupPendingVerification {
		if err == nil {
			err = ErrNotFound
		}
		return model.BrandCloudMemberInvitation{}, false, err
	}
	var memberExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM organization_members WHERE organization_id = $1 AND user_id = $2)`, in.BrandCloudID, target.ID).Scan(&memberExists); err != nil {
		return model.BrandCloudMemberInvitation{}, false, err
	}
	if memberExists {
		return model.BrandCloudMemberInvitation{}, false, ErrConflict
	}
	if expired, err := expireBrandCloudMemberInvitationsTx(ctx, tx, in.BrandCloudID, now); err != nil {
		return model.BrandCloudMemberInvitation{}, false, err
	} else if expired > 0 {
		if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_member_invitation_expired", ActorUserID: &in.InvitedByUserID, OrganizationID: &in.BrandCloudID, SubjectType: "brand_cloud", SubjectID: in.BrandCloudID, Payload: map[string]any{"expired_count": expired}}); err != nil {
			return model.BrandCloudMemberInvitation{}, false, err
		}
	}
	existing, err := scanBrandCloudMemberInvitation(tx.QueryRow(ctx, `
		SELECT id::text, brand_cloud_id::text, invited_by_user_id::text, target_user_id::text,
		       target_email, role, status, expires_at, accepted_at, canceled_at, created_at, updated_at
		FROM brand_cloud_member_invitations
		WHERE brand_cloud_id = $1 AND target_user_id = $2 AND status = 'pending'
		FOR UPDATE
	`, in.BrandCloudID, target.ID))
	if err == nil {
		if existing.Role != in.Role {
			return model.BrandCloudMemberInvitation{}, false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return model.BrandCloudMemberInvitation{}, false, err
		}
		return existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return model.BrandCloudMemberInvitation{}, false, err
	}

	invitation, err := scanBrandCloudMemberInvitation(tx.QueryRow(ctx, `
		INSERT INTO brand_cloud_member_invitations (
			brand_cloud_id, invited_by_user_id, target_user_id, target_email, role, token_hash, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id::text, brand_cloud_id::text, invited_by_user_id::text, target_user_id::text,
		          target_email, role, status, expires_at, accepted_at, canceled_at, created_at, updated_at
	`, in.BrandCloudID, in.InvitedByUserID, target.ID, target.Email, in.Role, in.TokenHash, in.ExpiresAt))
	if err != nil {
		if isUniqueViolation(err) {
			return model.BrandCloudMemberInvitation{}, false, ErrConflict
		}
		return model.BrandCloudMemberInvitation{}, false, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_member_invitation_created", ActorUserID: &in.InvitedByUserID, OrganizationID: &in.BrandCloudID, SubjectType: "brand_cloud_member_invitation", SubjectID: invitation.ID, Payload: map[string]any{"target_user_id": target.ID, "target_email": target.Email, "role": in.Role}}); err != nil {
		return model.BrandCloudMemberInvitation{}, false, err
	}
	if in.Email != nil {
		email := *in.Email
		email.Payload.RecipientEmail = invitation.TargetEmail
		if err := s.enqueueEmailTx(ctx, tx, email); err != nil {
			return model.BrandCloudMemberInvitation{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return model.BrandCloudMemberInvitation{}, false, err
	}
	return invitation, true, nil
}

func (s *Store) ListBrandCloudMemberInvitations(ctx context.Context, brandCloudID, actorUserID string, now time.Time) ([]model.BrandCloudMemberInvitation, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := lockBrandCloudCollaborationTx(ctx, tx, brandCloudID, actorUserID); err != nil {
		return nil, err
	}
	if err := requireBrandCloudOwnerTx(ctx, tx, brandCloudID, actorUserID); err != nil {
		return nil, err
	}
	expired, err := expireBrandCloudMemberInvitationsTx(ctx, tx, brandCloudID, now)
	if err != nil {
		return nil, err
	}
	if expired > 0 {
		if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_member_invitation_expired", ActorUserID: &actorUserID, OrganizationID: &brandCloudID, SubjectType: "brand_cloud", SubjectID: brandCloudID, Payload: map[string]any{"expired_count": expired}}); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT id::text, brand_cloud_id::text, invited_by_user_id::text, target_user_id::text,
		       target_email, role, status, expires_at, accepted_at, canceled_at, created_at, updated_at
		FROM brand_cloud_member_invitations WHERE brand_cloud_id = $1
		ORDER BY created_at DESC
	`, brandCloudID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []model.BrandCloudMemberInvitation{}
	for rows.Next() {
		item, err := scanBrandCloudMemberInvitation(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) ResendBrandCloudMemberInvitation(ctx context.Context, in BrandCloudMemberInvitationMutation, now time.Time) (model.BrandCloudMemberInvitation, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.BrandCloudMemberInvitation{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockCloudInvitationMutationTx(ctx, tx, in); err != nil {
		return model.BrandCloudMemberInvitation{}, err
	}
	invitation, err := scanBrandCloudMemberInvitation(tx.QueryRow(ctx, `
		UPDATE brand_cloud_member_invitations
		SET token_hash = $4, expires_at = $5, updated_at = now()
		WHERE id = $1 AND brand_cloud_id = $2 AND status = 'pending' AND expires_at > $3
		RETURNING id::text, brand_cloud_id::text, invited_by_user_id::text, target_user_id::text,
		          target_email, role, status, expires_at, accepted_at, canceled_at, created_at, updated_at
	`, in.InvitationID, in.BrandCloudID, now, in.TokenHash, in.ExpiresAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.BrandCloudMemberInvitation{}, ErrConflict
	}
	if err != nil {
		return model.BrandCloudMemberInvitation{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_member_invitation_resent", ActorUserID: &in.ActorUserID, OrganizationID: &in.BrandCloudID, SubjectType: "brand_cloud_member_invitation", SubjectID: invitation.ID, Payload: map[string]any{"target_user_id": invitation.TargetUserID, "role": invitation.Role}}); err != nil {
		return model.BrandCloudMemberInvitation{}, err
	}
	if in.Email != nil {
		email := *in.Email
		email.Payload.RecipientEmail = invitation.TargetEmail
		if err := s.enqueueEmailTx(ctx, tx, email); err != nil {
			return model.BrandCloudMemberInvitation{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return model.BrandCloudMemberInvitation{}, err
	}
	return invitation, nil
}

func (s *Store) CancelBrandCloudMemberInvitation(ctx context.Context, in BrandCloudMemberInvitationMutation, now time.Time) (model.BrandCloudMemberInvitation, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.BrandCloudMemberInvitation{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockCloudInvitationMutationTx(ctx, tx, in); err != nil {
		return model.BrandCloudMemberInvitation{}, err
	}
	invitation, err := scanBrandCloudMemberInvitation(tx.QueryRow(ctx, `
		UPDATE brand_cloud_member_invitations
		SET status = 'canceled', canceled_at = $3, updated_at = now()
		WHERE id = $1 AND brand_cloud_id = $2 AND status = 'pending' AND expires_at > $3
		RETURNING id::text, brand_cloud_id::text, invited_by_user_id::text, target_user_id::text,
		          target_email, role, status, expires_at, accepted_at, canceled_at, created_at, updated_at
	`, in.InvitationID, in.BrandCloudID, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.BrandCloudMemberInvitation{}, ErrConflict
	}
	if err != nil {
		return model.BrandCloudMemberInvitation{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_member_invitation_canceled", ActorUserID: &in.ActorUserID, OrganizationID: &in.BrandCloudID, SubjectType: "brand_cloud_member_invitation", SubjectID: invitation.ID, Payload: map[string]any{"target_user_id": invitation.TargetUserID, "role": invitation.Role}}); err != nil {
		return model.BrandCloudMemberInvitation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.BrandCloudMemberInvitation{}, err
	}
	return invitation, nil
}

func (s *Store) AcceptBrandCloudMemberInvitation(ctx context.Context, targetUserID, tokenHash string, now time.Time) (model.BrandCloudMemberInvitation, model.Member, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.BrandCloudMemberInvitation{}, model.Member{}, err
	}
	defer tx.Rollback(ctx)
	var cloudID, inviterID string
	if err := tx.QueryRow(ctx, `SELECT brand_cloud_id::text,invited_by_user_id::text FROM brand_cloud_member_invitations WHERE token_hash=$1 AND target_user_id::text=$2 AND status='pending'`, tokenHash, targetUserID).Scan(&cloudID, &inviterID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return model.BrandCloudMemberInvitation{}, model.Member{}, err
	}
	if err := lockBrandCloudCollaborationTx(ctx, tx, cloudID, inviterID, targetUserID); err != nil {
		return model.BrandCloudMemberInvitation{}, model.Member{}, err
	}
	if err := requireBrandCloudOwnerTx(ctx, tx, cloudID, inviterID); err != nil {
		return model.BrandCloudMemberInvitation{}, model.Member{}, err
	}
	invitation, err := scanBrandCloudMemberInvitation(tx.QueryRow(ctx, `
		SELECT i.id::text, i.brand_cloud_id::text, i.invited_by_user_id::text, i.target_user_id::text,
		       i.target_email, i.role, i.status, i.expires_at, i.accepted_at, i.canceled_at, i.created_at, i.updated_at
		FROM brand_cloud_member_invitations i
		JOIN users u ON u.id = i.target_user_id
		WHERE i.token_hash = $1 AND i.target_user_id = $2 AND i.status = 'pending'
		  AND u.disabled_at IS NULL AND u.email_verified = true
		  AND u.signup_pending_verification = false AND u.email = i.target_email
		FOR UPDATE
	`, tokenHash, targetUserID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.BrandCloudMemberInvitation{}, model.Member{}, ErrNotFound
	}
	if err != nil {
		return model.BrandCloudMemberInvitation{}, model.Member{}, err
	}
	if !invitation.ExpiresAt.After(now) {
		if _, err := tx.Exec(ctx, `UPDATE brand_cloud_member_invitations SET status = 'expired', updated_at = now() WHERE id = $1`, invitation.ID); err != nil {
			return model.BrandCloudMemberInvitation{}, model.Member{}, err
		}
		if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_member_invitation_expired", ActorUserID: &targetUserID, OrganizationID: &invitation.BrandCloudID, SubjectType: "brand_cloud_member_invitation", SubjectID: invitation.ID, Payload: map[string]any{"target_user_id": targetUserID, "role": invitation.Role}}); err != nil {
			return model.BrandCloudMemberInvitation{}, model.Member{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return model.BrandCloudMemberInvitation{}, model.Member{}, err
		}
		return model.BrandCloudMemberInvitation{}, model.Member{}, ErrNotFound
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_members (organization_id, user_id, role) VALUES ($1, $2, $3)`, invitation.BrandCloudID, targetUserID, invitation.Role); err != nil {
		if isUniqueViolation(err) {
			return model.BrandCloudMemberInvitation{}, model.Member{}, ErrConflict
		}
		return model.BrandCloudMemberInvitation{}, model.Member{}, err
	}
	member, err := s.getMemberTx(ctx, tx, invitation.BrandCloudID, targetUserID)
	if err != nil {
		return model.BrandCloudMemberInvitation{}, model.Member{}, err
	}
	accepted, err := scanBrandCloudMemberInvitation(tx.QueryRow(ctx, `
		UPDATE brand_cloud_member_invitations SET status = 'accepted', accepted_at = $2, updated_at = now()
		WHERE id = $1
		RETURNING id::text, brand_cloud_id::text, invited_by_user_id::text, target_user_id::text,
		          target_email, role, status, expires_at, accepted_at, canceled_at, created_at, updated_at
	`, invitation.ID, now))
	if err != nil {
		return model.BrandCloudMemberInvitation{}, model.Member{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_member_invitation_accepted", ActorUserID: &targetUserID, OrganizationID: &invitation.BrandCloudID, SubjectType: "brand_cloud_member_invitation", SubjectID: invitation.ID, Payload: map[string]any{"target_user_id": targetUserID, "role": invitation.Role}}); err != nil {
		return model.BrandCloudMemberInvitation{}, model.Member{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.BrandCloudMemberInvitation{}, model.Member{}, err
	}
	return accepted, member, nil
}

func scanBrandCloudMemberInvitation(row scanner) (model.BrandCloudMemberInvitation, error) {
	var invitation model.BrandCloudMemberInvitation
	err := row.Scan(&invitation.ID, &invitation.BrandCloudID, &invitation.InvitedByUserID, &invitation.TargetUserID,
		&invitation.TargetEmail, &invitation.Role, &invitation.Status, &invitation.ExpiresAt, &invitation.AcceptedAt,
		&invitation.CanceledAt, &invitation.CreatedAt, &invitation.UpdatedAt)
	return invitation, err
}
