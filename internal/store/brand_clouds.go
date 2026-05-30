package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

func (s *Store) CreateBrandCloud(ctx context.Context, actorUserID string, in BrandCloudInput) (model.Organization, error) {
	metadata, err := json.Marshal(defaultMetadata(in.Metadata))
	if err != nil {
		return model.Organization{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.Organization{}, err
	}
	defer tx.Rollback(ctx)

	org, err := scanOrganization(tx.QueryRow(ctx, `
		INSERT INTO organizations (name, organization_kind, status, tier, evaluation_device_quota, metadata)
		VALUES ($1, 'brand_cloud', 'active', 'commercial', 5, $2)
		RETURNING id::text, name, ''::text, organization_kind, status, tier, evaluation_device_quota, metadata, created_at, updated_at
	`, strings.TrimSpace(in.Name), metadata))
	if err != nil {
		return model.Organization{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "brand_cloud_created",
		ActorUserID:    &actorUserID,
		OrganizationID: &org.ID,
		SubjectType:    "brand_cloud",
		SubjectID:      org.ID,
		Payload: map[string]any{
			"name":              org.Name,
			"organization_kind": org.OrganizationKind,
			"status":            org.Status,
		},
	}); err != nil {
		return model.Organization{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Organization{}, err
	}
	return org, nil
}

func (s *Store) ListBrandClouds(ctx context.Context, limit, offset int) (OrganizationPage, error) {
	total, err := s.countBrandClouds(ctx)
	if err != nil {
		return OrganizationPage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT id::text, name, ''::text, organization_kind, status, tier, evaluation_device_quota, metadata, created_at, updated_at
		FROM organizations
		WHERE organization_kind = 'brand_cloud'
		ORDER BY created_at ASC
		LIMIT $1 OFFSET $2
	`, limit, offset)
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

func (s *Store) GetBrandCloud(ctx context.Context, orgID string) (model.Organization, error) {
	org, err := scanOrganization(s.db.QueryRow(ctx, `
		SELECT id::text, name, ''::text, organization_kind, status, tier, evaluation_device_quota, metadata, created_at, updated_at
		FROM organizations
		WHERE id = $1 AND organization_kind = 'brand_cloud'
	`, orgID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Organization{}, ErrNotFound
	}
	return org, err
}

func (s *Store) UpdateBrandCloud(ctx context.Context, actorUserID, orgID string, in BrandCloudInput) (model.Organization, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.Organization{}, err
	}
	defer tx.Rollback(ctx)

	current, err := scanOrganization(tx.QueryRow(ctx, `
		SELECT id::text, name, ''::text, organization_kind, status, tier, evaluation_device_quota, metadata, created_at, updated_at
		FROM organizations
		WHERE id = $1 AND organization_kind = 'brand_cloud'
		FOR UPDATE
	`, orgID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Organization{}, ErrNotFound
	}
	if err != nil {
		return model.Organization{}, err
	}

	name := current.Name
	if strings.TrimSpace(in.Name) != "" {
		name = strings.TrimSpace(in.Name)
	}
	status := current.Status
	if in.Status != "" {
		status = in.Status
	}
	metadata := current.Metadata
	if in.Metadata != nil {
		metadata = defaultMetadata(in.Metadata)
	}
	rawMetadata, err := json.Marshal(metadata)
	if err != nil {
		return model.Organization{}, err
	}
	org, err := scanOrganization(tx.QueryRow(ctx, `
		UPDATE organizations
		SET name = $2, status = $3, metadata = $4, updated_at = now()
		WHERE id = $1 AND organization_kind = 'brand_cloud'
		RETURNING id::text, name, ''::text, organization_kind, status, tier, evaluation_device_quota, metadata, created_at, updated_at
	`, orgID, name, status, rawMetadata))
	if err != nil {
		return model.Organization{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "brand_cloud_updated",
		ActorUserID:    &actorUserID,
		OrganizationID: &org.ID,
		SubjectType:    "brand_cloud",
		SubjectID:      org.ID,
		Payload: map[string]any{
			"name":   org.Name,
			"status": org.Status,
		},
	}); err != nil {
		return model.Organization{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Organization{}, err
	}
	return org, nil
}

func (s *Store) AssignBrandCloudMember(ctx context.Context, actorUserID, orgID, userID string, role model.Role) (model.Member, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.Member{}, err
	}
	defer tx.Rollback(ctx)

	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM organizations
			WHERE id = $1 AND organization_kind = 'brand_cloud'
		)
	`, orgID).Scan(&exists); err != nil {
		return model.Member{}, err
	}
	if !exists {
		return model.Member{}, ErrNotFound
	}

	member, err := scanMember(tx.QueryRow(ctx, `
		INSERT INTO organization_members (organization_id, user_id, role)
		SELECT $1, id, $3 FROM users WHERE id = $2 AND disabled_at IS NULL
		ON CONFLICT (organization_id, user_id)
		DO UPDATE SET role = EXCLUDED.role, updated_at = now()
		RETURNING organization_id::text, user_id::text, role, created_at, updated_at
	`, orgID, userID, role))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Member{}, ErrNotFound
	}
	if err != nil {
		return model.Member{}, err
	}
	user, err := getUserTx(ctx, tx, member.UserID)
	if err != nil {
		return model.Member{}, err
	}
	member.Email = user.Email
	member.DisplayName = user.DisplayName
	member.DisabledAt = user.DisabledAt

	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "brand_cloud_member_assigned",
		ActorUserID:    &actorUserID,
		OrganizationID: &orgID,
		SubjectType:    "brand_cloud",
		SubjectID:      orgID,
		Payload: map[string]any{
			"user_id": member.UserID,
			"role":    member.Role,
		},
	}); err != nil {
		return model.Member{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Member{}, err
	}
	return member, nil
}

func (s *Store) CreateBrandCloudUser(ctx context.Context, actorUserID, orgID string, in BrandCloudUserInput) (BrandCloudUserResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return BrandCloudUserResult{}, err
	}
	defer tx.Rollback(ctx)

	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM organizations
			WHERE id = $1 AND organization_kind = 'brand_cloud'
		)
	`, orgID).Scan(&exists); err != nil {
		return BrandCloudUserResult{}, err
	}
	if !exists {
		return BrandCloudUserResult{}, ErrNotFound
	}

	email := strings.ToLower(strings.TrimSpace(in.Email))
	var existingID string
	err = tx.QueryRow(ctx, `SELECT id::text FROM users WHERE email = $1`, email).Scan(&existingID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return BrandCloudUserResult{}, err
	}
	action := "assigned"
	if errors.Is(err, pgx.ErrNoRows) {
		action = "created"
	}

	var user model.User
	err = tx.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, display_name, email_verified, email_verified_at, signup_pending_verification, disabled_at)
		VALUES ($1, $2, $3, true, now(), false, NULL)
		ON CONFLICT (email)
		DO UPDATE SET
			password_hash = CASE WHEN $4 THEN EXCLUDED.password_hash ELSE users.password_hash END,
			display_name = COALESCE(EXCLUDED.display_name, users.display_name),
			email_verified = true,
			email_verified_at = COALESCE(users.email_verified_at, now()),
			signup_pending_verification = false,
			disabled_at = NULL,
			updated_at = now()
		RETURNING id::text, email, display_name, email_verified, email_verified_at, signup_pending_verification, created_at, updated_at, disabled_at
	`, email, in.PasswordHash, in.DisplayName, in.RotatePassword).Scan(&user.ID, &user.Email, &user.DisplayName, &user.EmailVerified, &user.EmailVerifiedAt, &user.SignupPendingVerification, &user.CreatedAt, &user.UpdatedAt, &user.DisabledAt)
	if err != nil {
		return BrandCloudUserResult{}, err
	}

	member, err := scanMember(tx.QueryRow(ctx, `
		INSERT INTO organization_members (organization_id, user_id, role)
		VALUES ($1, $2, $3)
		ON CONFLICT (organization_id, user_id)
		DO UPDATE SET role = EXCLUDED.role, updated_at = now()
		RETURNING organization_id::text, user_id::text, role, created_at, updated_at
	`, orgID, user.ID, in.Role))
	if err != nil {
		return BrandCloudUserResult{}, err
	}
	member.Email = user.Email
	member.DisplayName = user.DisplayName
	member.DisabledAt = user.DisabledAt

	eventType := "brand_cloud_user_assigned"
	if action == "created" {
		eventType = "brand_cloud_user_created"
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      eventType,
		ActorUserID:    &actorUserID,
		OrganizationID: &orgID,
		SubjectType:    "brand_cloud",
		SubjectID:      orgID,
		Payload: map[string]any{
			"user_id":         user.ID,
			"email":           user.Email,
			"role":            member.Role,
			"rotate_password": in.RotatePassword,
		},
	}); err != nil {
		return BrandCloudUserResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BrandCloudUserResult{}, err
	}
	return BrandCloudUserResult{Action: action, User: user, Member: member}, nil
}

func (s *Store) countBrandClouds(ctx context.Context) (int, error) {
	var total int
	err := s.db.QueryRow(ctx, `
		SELECT count(*)::int
		FROM organizations
		WHERE organization_kind = 'brand_cloud'
	`).Scan(&total)
	return total, err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanOrganization(row scanner) (model.Organization, error) {
	var org model.Organization
	var role string
	var rawMetadata []byte
	if err := row.Scan(&org.ID, &org.Name, &role, &org.OrganizationKind, &org.Status, &org.Tier, &org.EvaluationDeviceQuota, &rawMetadata, &org.CreatedAt, &org.UpdatedAt); err != nil {
		return model.Organization{}, err
	}
	org.Role = model.Role(role)
	if len(rawMetadata) == 0 {
		org.Metadata = map[string]any{}
		return org, nil
	}
	if err := json.Unmarshal(rawMetadata, &org.Metadata); err != nil {
		return model.Organization{}, err
	}
	return org, nil
}

func scanMember(row scanner) (model.Member, error) {
	var member model.Member
	err := row.Scan(&member.OrganizationID, &member.UserID, &member.Role, &member.CreatedAt, &member.UpdatedAt)
	return member, err
}

func getUserTx(ctx context.Context, tx pgx.Tx, userID string) (model.User, error) {
	var user model.User
	err := tx.QueryRow(ctx, `
		SELECT id::text, email, display_name, email_verified, email_verified_at, signup_pending_verification, created_at, updated_at, disabled_at
		FROM users
		WHERE id = $1 AND disabled_at IS NULL
	`, userID).Scan(&user.ID, &user.Email, &user.DisplayName, &user.EmailVerified, &user.EmailVerifiedAt, &user.SignupPendingVerification, &user.CreatedAt, &user.UpdatedAt, &user.DisabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, ErrNotFound
	}
	return user, err
}
