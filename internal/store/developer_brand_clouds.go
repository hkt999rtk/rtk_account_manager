package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

const defaultDeveloperCloudLimit = 8

func (s *Store) SignupDeveloper(ctx context.Context, in DeveloperSignupInput) (DeveloperSignupResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return DeveloperSignupResult{}, err
	}
	defer tx.Rollback(ctx)

	email := strings.ToLower(strings.TrimSpace(in.Email))
	user, err := scanDeveloperUser(tx.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, display_name, signup_pending_verification, developer_cloud_limit)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id::text, email, display_name, email_verified, email_verified_at, signup_pending_verification, developer_cloud_limit, created_at, updated_at, disabled_at
	`, email, in.PasswordHash, in.DisplayName, in.SignupPendingVerification, defaultDeveloperCloudLimit))
	if err != nil {
		if isUniqueViolation(err) {
			return DeveloperSignupResult{}, ErrConflict
		}
		return DeveloperSignupResult{}, err
	}
	brandCloud, err := createDeveloperBrandCloudTx(ctx, tx, user.ID, BrandCloudInput{Name: email}, false)
	if err != nil {
		return DeveloperSignupResult{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "developer_signup_created",
		ActorUserID:    &user.ID,
		OrganizationID: &brandCloud.ID,
		SubjectType:    "brand_cloud",
		SubjectID:      brandCloud.ID,
		Payload: map[string]any{
			"user_id":                     user.ID,
			"email":                       user.Email,
			"brand_cloud_name":            brandCloud.Name,
			"developer_cloud_limit":       user.DeveloperCloudLimit,
			"signup_pending_verification": in.SignupPendingVerification,
		},
	}); err != nil {
		return DeveloperSignupResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DeveloperSignupResult{}, err
	}
	return DeveloperSignupResult{User: user, BrandCloud: brandCloud}, nil
}

func (s *Store) CreateDeveloperBrandCloud(ctx context.Context, userID string, in BrandCloudInput) (model.Organization, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.Organization{}, err
	}
	defer tx.Rollback(ctx)

	org, err := createDeveloperBrandCloudTx(ctx, tx, userID, in, true)
	if err != nil {
		return model.Organization{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "developer_brand_cloud_created",
		ActorUserID:    &userID,
		OrganizationID: &org.ID,
		SubjectType:    "brand_cloud",
		SubjectID:      org.ID,
		Payload: map[string]any{
			"name":              org.Name,
			"tenant_slug":       org.TenantSlug,
			"organization_kind": org.OrganizationKind,
		},
	}); err != nil {
		return model.Organization{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Organization{}, err
	}
	return org, nil
}

func (s *Store) ListDeveloperBrandClouds(ctx context.Context, userID string, limit, offset int) (OrganizationPage, error) {
	total, err := s.countDeveloperBrandClouds(ctx, userID)
	if err != nil {
		return OrganizationPage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT o.id::text, o.name, o.tenant_slug, m.role, o.organization_kind, o.status, o.tier, o.evaluation_device_quota, o.metadata, o.created_at, o.updated_at
		FROM organizations o
		JOIN organization_members m ON m.organization_id = o.id
		JOIN users u ON u.id = m.user_id
		WHERE m.user_id = $1
		  AND o.organization_kind = 'brand_cloud'
		  AND u.disabled_at IS NULL
		ORDER BY o.created_at ASC
		LIMIT $2 OFFSET $3
	`, userID, limit, offset)
	if err != nil {
		return OrganizationPage{}, err
	}
	defer rows.Close()

	orgs := []model.Organization{}
	for rows.Next() {
		org, err := scanOrganization(rows)
		if err != nil {
			return OrganizationPage{}, err
		}
		orgs = append(orgs, org)
	}
	if err := rows.Err(); err != nil {
		return OrganizationPage{}, err
	}
	return OrganizationPage{Organizations: orgs, Page: Page{Limit: limit, Offset: offset, Total: total}}, nil
}

func (s *Store) SetDeveloperCloudLimit(ctx context.Context, userID string, limit int) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE users
		SET developer_cloud_limit = $2, updated_at = now()
		WHERE id = $1 AND disabled_at IS NULL
	`, userID, limit)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetDeveloperBrandCloudMember(ctx context.Context, brandCloudID, userID string) (model.Member, error) {
	member, err := scanDeveloperMember(s.db.QueryRow(ctx, `
		SELECT m.organization_id::text, m.user_id::text, u.email, u.display_name, m.role, m.created_at, m.updated_at, m.disabled_at
		FROM organization_members m
		JOIN organizations o ON o.id = m.organization_id
		JOIN users u ON u.id = m.user_id
		WHERE m.organization_id = $1
		  AND m.user_id = $2
		  AND o.organization_kind = 'brand_cloud'
		  AND m.disabled_at IS NULL
		  AND u.disabled_at IS NULL
	`, brandCloudID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Member{}, ErrNotFound
	}
	return member, err
}

func (s *Store) ListDeveloperBrandCloudMembers(ctx context.Context, brandCloudID string, limit, offset int) (MemberPage, error) {
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM organization_members m JOIN organizations o ON o.id = m.organization_id WHERE m.organization_id = $1 AND o.organization_kind = 'brand_cloud'`, brandCloudID).Scan(&total); err != nil {
		return MemberPage{}, err
	}
	rows, err := s.db.Query(ctx, `SELECT m.organization_id::text, m.user_id::text, u.email, u.display_name, m.role, m.created_at, m.updated_at, COALESCE(m.disabled_at, u.disabled_at) FROM organization_members m JOIN organizations o ON o.id = m.organization_id JOIN users u ON u.id = m.user_id WHERE m.organization_id = $1 AND o.organization_kind = 'brand_cloud' ORDER BY m.created_at ASC LIMIT $2 OFFSET $3`, brandCloudID, limit, offset)
	if err != nil {
		return MemberPage{}, err
	}
	defer rows.Close()
	members := []model.Member{}
	for rows.Next() {
		member, scanErr := scanDeveloperMember(rows)
		if scanErr != nil {
			return MemberPage{}, scanErr
		}
		members = append(members, member)
	}
	return MemberPage{Members: members, Page: Page{Limit: limit, Offset: offset, Total: total}}, rows.Err()
}

func (s *Store) DisableDeveloperBrandCloudMember(ctx context.Context, brandCloudID, userID string) (model.Member, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.Member{}, err
	}
	defer tx.Rollback(ctx)
	if err := ensureNotLastActiveOwnerTx(ctx, tx, brandCloudID, userID); err != nil {
		return model.Member{}, err
	}
	member, err := scanDeveloperMember(tx.QueryRow(ctx, `UPDATE organization_members m SET disabled_at = COALESCE(m.disabled_at, now()), updated_at = now() FROM users u WHERE m.organization_id = $1 AND m.user_id = $2 AND u.id = m.user_id RETURNING m.organization_id::text, m.user_id::text, u.email, u.display_name, m.role, m.created_at, m.updated_at, m.disabled_at`, brandCloudID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Member{}, ErrNotFound
	}
	if err != nil {
		return model.Member{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Member{}, err
	}
	return member, nil
}

func (s *Store) EnableDeveloperBrandCloudMember(ctx context.Context, brandCloudID, userID string) (model.Member, error) {
	member, err := scanDeveloperMember(s.db.QueryRow(ctx, `UPDATE organization_members m SET disabled_at = NULL, updated_at = now() FROM users u WHERE m.organization_id = $1 AND m.user_id = $2 AND u.id = m.user_id RETURNING m.organization_id::text, m.user_id::text, u.email, u.display_name, m.role, m.created_at, m.updated_at, m.disabled_at`, brandCloudID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Member{}, ErrNotFound
	}
	return member, err
}

func scanDeveloperMember(row scanner) (model.Member, error) {
	var member model.Member
	err := row.Scan(&member.OrganizationID, &member.UserID, &member.Email, &member.DisplayName, &member.Role, &member.CreatedAt, &member.UpdatedAt, &member.DisabledAt)
	return member, err
}

func (s *Store) CreateBrandCloudOwnerTransfer(ctx context.Context, in BrandCloudOwnerTransferInput) (model.BrandCloudOwnerTransfer, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	defer tx.Rollback(ctx)

	target, err := getDeveloperUserByEmailTx(ctx, tx, in.TargetEmail)
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	var currentRole model.Role
	if err := tx.QueryRow(ctx, `
		SELECT m.role
		FROM organization_members m
		JOIN organizations o ON o.id = m.organization_id
		WHERE m.organization_id = $1
		  AND m.user_id = $2
		  AND o.organization_kind = 'brand_cloud'
		FOR UPDATE
	`, in.BrandCloudID, in.RequestedByUserID).Scan(&currentRole); errors.Is(err, pgx.ErrNoRows) {
		return model.BrandCloudOwnerTransfer{}, ErrNotFound
	} else if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	if currentRole != model.RoleOwner {
		return model.BrandCloudOwnerTransfer{}, ErrNotFound
	}
	if target.ID == in.RequestedByUserID {
		return model.BrandCloudOwnerTransfer{}, ErrConflict
	}

	transfer, err := scanBrandCloudOwnerTransfer(tx.QueryRow(ctx, `
		INSERT INTO brand_cloud_owner_transfers (
		    brand_cloud_id, requested_by_user_id, target_user_id, token_hash, status, expires_at
		)
		VALUES ($1, $2, $3, $4, 'pending', $5)
		RETURNING id::text, brand_cloud_id::text, requested_by_user_id::text, target_user_id::text,
		          status, expires_at, accepted_at, canceled_at, created_at, updated_at
	`, in.BrandCloudID, in.RequestedByUserID, target.ID, in.TokenHash, in.ExpiresAt))
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	transfer.TargetEmail = target.Email
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "brand_cloud_owner_transfer_requested",
		ActorUserID:    &in.RequestedByUserID,
		OrganizationID: &in.BrandCloudID,
		SubjectType:    "brand_cloud",
		SubjectID:      in.BrandCloudID,
		Payload: map[string]any{
			"target_user_id": target.ID,
			"target_email":   target.Email,
			"transfer_id":    transfer.ID,
		},
	}); err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	return transfer, nil
}

func (s *Store) AcceptBrandCloudOwnerTransfer(ctx context.Context, targetUserID, tokenHash string, now time.Time) (model.BrandCloudOwnerTransfer, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	defer tx.Rollback(ctx)

	transfer, err := scanBrandCloudOwnerTransfer(tx.QueryRow(ctx, `
		SELECT id::text, brand_cloud_id::text, requested_by_user_id::text, target_user_id::text,
		       status, expires_at, accepted_at, canceled_at, created_at, updated_at
		FROM brand_cloud_owner_transfers
		WHERE token_hash = $1
		  AND target_user_id = $2
		  AND status = 'pending'
		  AND expires_at > $3
		FOR UPDATE
	`, tokenHash, targetUserID, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.BrandCloudOwnerTransfer{}, ErrNotFound
	}
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO organization_members (organization_id, user_id, role)
		VALUES ($1, $2, 'owner')
		ON CONFLICT (organization_id, user_id)
		DO UPDATE SET role = 'owner', updated_at = now()
	`, transfer.BrandCloudID, targetUserID); err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE organization_members
		SET role = 'admin', updated_at = now()
		WHERE organization_id = $1 AND user_id = $2 AND role = 'owner'
	`, transfer.BrandCloudID, transfer.RequestedByUserID); err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	accepted, err := scanBrandCloudOwnerTransfer(tx.QueryRow(ctx, `
		UPDATE brand_cloud_owner_transfers
		SET status = 'accepted', accepted_at = $2, updated_at = now()
		WHERE id = $1
		RETURNING id::text, brand_cloud_id::text, requested_by_user_id::text, target_user_id::text,
		          status, expires_at, accepted_at, canceled_at, created_at, updated_at
	`, transfer.ID, now))
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "brand_cloud_owner_transfer_accepted",
		ActorUserID:    &targetUserID,
		OrganizationID: &transfer.BrandCloudID,
		SubjectType:    "brand_cloud",
		SubjectID:      transfer.BrandCloudID,
		Payload: map[string]any{
			"previous_owner_user_id": transfer.RequestedByUserID,
			"new_owner_user_id":      targetUserID,
			"transfer_id":            transfer.ID,
		},
	}); err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	return accepted, nil
}

func (s *Store) GetBrandCloudOwnerTransfer(ctx context.Context, in BrandCloudOwnerTransferQuery, now time.Time) (model.BrandCloudOwnerTransfer, error) {
	transfer, err := scanBrandCloudOwnerTransfer(s.db.QueryRow(ctx, `
		SELECT id::text, brand_cloud_id::text, requested_by_user_id::text, target_user_id::text,
		       status, expires_at, accepted_at, canceled_at, created_at, updated_at
		FROM brand_cloud_owner_transfers
		WHERE id = $1 AND brand_cloud_id = $2 AND requested_by_user_id = $3
	`, in.TransferID, in.BrandCloudID, in.RequesterID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.BrandCloudOwnerTransfer{}, ErrNotFound
	}
	if err != nil {
		return model.BrandCloudOwnerTransfer{}, err
	}
	if transfer.Status == "pending" && !transfer.ExpiresAt.After(now) {
		transfer, err = scanBrandCloudOwnerTransfer(s.db.QueryRow(ctx, `
			UPDATE brand_cloud_owner_transfers SET status = 'expired', updated_at = now()
			WHERE id = $1 AND status = 'pending'
			RETURNING id::text, brand_cloud_id::text, requested_by_user_id::text, target_user_id::text,
			          status, expires_at, accepted_at, canceled_at, created_at, updated_at
		`, transfer.ID))
		if err != nil {
			return model.BrandCloudOwnerTransfer{}, err
		}
	}
	return transfer, nil
}

func (s *Store) CancelBrandCloudOwnerTransfer(ctx context.Context, in BrandCloudOwnerTransferQuery, now time.Time) (model.BrandCloudOwnerTransfer, error) {
	transfer, err := scanBrandCloudOwnerTransfer(s.db.QueryRow(ctx, `
		UPDATE brand_cloud_owner_transfers
		SET status = 'canceled', canceled_at = $4, updated_at = now()
		WHERE id = $1 AND brand_cloud_id = $2 AND requested_by_user_id = $3 AND status = 'pending' AND expires_at > $4
		RETURNING id::text, brand_cloud_id::text, requested_by_user_id::text, target_user_id::text,
		          status, expires_at, accepted_at, canceled_at, created_at, updated_at
	`, in.TransferID, in.BrandCloudID, in.RequesterID, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.BrandCloudOwnerTransfer{}, ErrNotFound
	}
	return transfer, err
}

func createDeveloperBrandCloudTx(ctx context.Context, tx pgx.Tx, userID string, in BrandCloudInput, enforceLimit bool) (model.Organization, error) {
	user, err := getDeveloperUserTx(ctx, tx, userID)
	if err != nil {
		return model.Organization{}, err
	}
	if enforceLimit {
		count, err := countDeveloperBrandCloudsTx(ctx, tx, user.ID)
		if err != nil {
			return model.Organization{}, err
		}
		if count >= user.DeveloperCloudLimit {
			return model.Organization{}, ErrDeveloperCloudLimitExceeded
		}
	}

	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = user.Email
	}
	slug := normalizeTenantSlug(in.TenantSlug)
	if strings.TrimSpace(in.TenantSlug) != "" && slug == "" {
		return model.Organization{}, ErrConflict
	}
	if slug == "" {
		suffix, err := randomTenantSlugSuffix()
		if err != nil {
			return model.Organization{}, err
		}
		slug = generatedTenantSlug(name, suffix)
	}
	metadata, err := json.Marshal(defaultMetadata(in.Metadata))
	if err != nil {
		return model.Organization{}, err
	}
	org, err := scanOrganization(tx.QueryRow(ctx, `
		INSERT INTO organizations (name, tenant_slug, organization_kind, status, tier, evaluation_device_quota, metadata)
		VALUES ($1, $2, 'brand_cloud', 'active', 'commercial', 5, $3)
		RETURNING id::text, name, tenant_slug, 'owner'::text, organization_kind, status, tier, evaluation_device_quota, metadata, created_at, updated_at
	`, name, slug, metadata))
	if err != nil {
		if isUniqueViolation(err) {
			return model.Organization{}, ErrConflict
		}
		return model.Organization{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO organization_members (organization_id, user_id, role)
		VALUES ($1, $2, 'owner')
	`, org.ID, user.ID); err != nil {
		return model.Organization{}, err
	}
	return org, nil
}

func (s *Store) countDeveloperBrandClouds(ctx context.Context, userID string) (int, error) {
	return countDeveloperBrandCloudsTx(ctx, s.db, userID)
}

func countDeveloperBrandCloudsTx(ctx context.Context, q rowQuerier, userID string) (int, error) {
	var total int
	err := q.QueryRow(ctx, `
		SELECT count(*)::int
		FROM organization_members m
		JOIN organizations o ON o.id = m.organization_id
		WHERE m.user_id = $1
		  AND m.role = 'owner'
		  AND o.organization_kind = 'brand_cloud'
	`, userID).Scan(&total)
	return total, err
}

func getDeveloperUserTx(ctx context.Context, tx pgx.Tx, userID string) (model.User, error) {
	user, err := scanDeveloperUser(tx.QueryRow(ctx, `
		SELECT id::text, email, display_name, email_verified, email_verified_at, signup_pending_verification, developer_cloud_limit, created_at, updated_at, disabled_at
		FROM users
		WHERE id = $1 AND disabled_at IS NULL
		FOR UPDATE
	`, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, ErrNotFound
	}
	return user, err
}

func getDeveloperUserByEmailTx(ctx context.Context, tx pgx.Tx, email string) (model.User, error) {
	user, err := scanDeveloperUser(tx.QueryRow(ctx, `
		SELECT id::text, email, display_name, email_verified, email_verified_at, signup_pending_verification, developer_cloud_limit, created_at, updated_at, disabled_at
		FROM users
		WHERE email = $1 AND disabled_at IS NULL
		FOR UPDATE
	`, strings.ToLower(strings.TrimSpace(email))))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, ErrNotFound
	}
	return user, err
}

func scanDeveloperUser(row scanner) (model.User, error) {
	var user model.User
	err := row.Scan(
		&user.ID,
		&user.Email,
		&user.DisplayName,
		&user.EmailVerified,
		&user.EmailVerifiedAt,
		&user.SignupPendingVerification,
		&user.DeveloperCloudLimit,
		&user.CreatedAt,
		&user.UpdatedAt,
		&user.DisabledAt,
	)
	return user, err
}

func scanBrandCloudOwnerTransfer(row scanner) (model.BrandCloudOwnerTransfer, error) {
	var transfer model.BrandCloudOwnerTransfer
	err := row.Scan(
		&transfer.ID,
		&transfer.BrandCloudID,
		&transfer.RequestedByUserID,
		&transfer.TargetUserID,
		&transfer.Status,
		&transfer.ExpiresAt,
		&transfer.AcceptedAt,
		&transfer.CanceledAt,
		&transfer.CreatedAt,
		&transfer.UpdatedAt,
	)
	return transfer, err
}
