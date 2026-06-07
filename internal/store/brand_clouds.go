package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

var slugUnsafePattern = regexp.MustCompile(`[^a-z0-9]+`)

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

	name := strings.TrimSpace(in.Name)
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
	org, err := scanOrganization(tx.QueryRow(ctx, `
		INSERT INTO organizations (name, tenant_slug, organization_kind, status, tier, evaluation_device_quota, metadata)
		VALUES ($1, $2, 'brand_cloud', 'active', 'commercial', 5, $3)
		RETURNING id::text, name, tenant_slug, ''::text, organization_kind, status, tier, evaluation_device_quota, metadata, created_at, updated_at
	`, name, slug, metadata))
	if err != nil {
		if isUniqueViolation(err) {
			return model.Organization{}, ErrConflict
		}
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
		SELECT id::text, name, tenant_slug, ''::text, organization_kind, status, tier, evaluation_device_quota, metadata, created_at, updated_at
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
		SELECT id::text, name, tenant_slug, ''::text, organization_kind, status, tier, evaluation_device_quota, metadata, created_at, updated_at
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
		SELECT id::text, name, tenant_slug, ''::text, organization_kind, status, tier, evaluation_device_quota, metadata, created_at, updated_at
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
	tenantSlug := current.TenantSlug
	if strings.TrimSpace(in.TenantSlug) != "" {
		normalized := normalizeTenantSlug(in.TenantSlug)
		if normalized == "" {
			return model.Organization{}, ErrConflict
		}
		tenantSlug = &normalized
	}
	rawMetadata, err := json.Marshal(metadata)
	if err != nil {
		return model.Organization{}, err
	}
	org, err := scanOrganization(tx.QueryRow(ctx, `
		UPDATE organizations
		SET name = $2, status = $3, tenant_slug = $4, metadata = $5, updated_at = now()
		WHERE id = $1 AND organization_kind = 'brand_cloud'
		RETURNING id::text, name, tenant_slug, ''::text, organization_kind, status, tier, evaluation_device_quota, metadata, created_at, updated_at
	`, orgID, name, status, tenantSlug, rawMetadata))
	if err != nil {
		if isUniqueViolation(err) {
			return model.Organization{}, ErrConflict
		}
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
	var existingBrandCloudUserID string
	err = tx.QueryRow(ctx, `
		SELECT id::text
		FROM brand_cloud_users
		WHERE brand_cloud_id = $1 AND email = $2
	`, orgID, email).Scan(&existingBrandCloudUserID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return BrandCloudUserResult{}, err
	}
	action := "assigned"
	if errors.Is(err, pgx.ErrNoRows) {
		action = "created"
	}

	brandCloudUser, err := scanBrandCloudUser(tx.QueryRow(ctx, `
		INSERT INTO brand_cloud_users (brand_cloud_id, email, password_hash, display_name, email_verified, email_verified_at, signup_pending_verification, disabled_at)
		VALUES ($1, $2, $3, $4, true, now(), false, NULL)
		ON CONFLICT ON CONSTRAINT brand_cloud_users_brand_email_key
		DO UPDATE SET
			password_hash = CASE WHEN $5 THEN EXCLUDED.password_hash ELSE brand_cloud_users.password_hash END,
			display_name = COALESCE(EXCLUDED.display_name, brand_cloud_users.display_name),
			email_verified = true,
			email_verified_at = COALESCE(brand_cloud_users.email_verified_at, now()),
			signup_pending_verification = false,
			disabled_at = NULL,
			updated_at = now()
		RETURNING id::text, brand_cloud_id::text, email, display_name, email_verified, email_verified_at, signup_pending_verification, created_at, updated_at, disabled_at
	`, orgID, email, in.PasswordHash, in.DisplayName, in.RotatePassword))
	if err != nil {
		return BrandCloudUserResult{}, err
	}
	brandCloudMember, err := scanBrandCloudMember(tx.QueryRow(ctx, `
		INSERT INTO brand_cloud_memberships (brand_cloud_id, brand_cloud_user_id, role)
		VALUES ($1, $2, $3)
		ON CONFLICT ON CONSTRAINT brand_cloud_memberships_brand_user_key
		DO UPDATE SET role = EXCLUDED.role, updated_at = now()
		RETURNING brand_cloud_id::text, brand_cloud_user_id::text, role, created_at, updated_at
	`, orgID, brandCloudUser.ID, in.Role))
	if err != nil {
		return BrandCloudUserResult{}, err
	}
	brandCloudMember.Email = brandCloudUser.Email
	brandCloudMember.DisplayName = brandCloudUser.DisplayName

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
			"user_id":             user.ID,
			"brand_cloud_user_id": brandCloudUser.ID,
			"email":               brandCloudUser.Email,
			"role":                brandCloudMember.Role,
			"rotate_password":     in.RotatePassword,
		},
	}); err != nil {
		return BrandCloudUserResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BrandCloudUserResult{}, err
	}
	return BrandCloudUserResult{Action: action, User: user, Member: member, BrandCloudUser: brandCloudUser, BrandCloudMember: brandCloudMember}, nil
}

func (s *Store) GetBrandCloudByTenantSlug(ctx context.Context, tenantSlug string) (model.Organization, error) {
	org, err := scanOrganization(s.db.QueryRow(ctx, `
		SELECT id::text, name, tenant_slug, ''::text, organization_kind, status, tier, evaluation_device_quota, metadata, created_at, updated_at
		FROM organizations
		WHERE tenant_slug = $1 AND organization_kind = 'brand_cloud' AND status = 'active'
	`, normalizeTenantSlug(tenantSlug)))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Organization{}, ErrNotFound
	}
	return org, err
}

func (s *Store) GetBrandCloudUserPassword(ctx context.Context, tenantSlug, email string) (BrandCloudLoginResult, error) {
	var result BrandCloudLoginResult
	var rawMetadata []byte
	err := s.db.QueryRow(ctx, `
		SELECT
		    o.id::text, o.name, o.tenant_slug, ''::text, o.organization_kind, o.status, o.tier, o.evaluation_device_quota, o.metadata, o.created_at, o.updated_at,
		    bcu.id::text, bcu.brand_cloud_id::text, bcu.email, bcu.password_hash, bcu.display_name, bcu.email_verified, bcu.email_verified_at, bcu.signup_pending_verification, bcu.created_at, bcu.updated_at, bcu.disabled_at,
		    bcm.role, bcm.created_at, bcm.updated_at
		FROM organizations o
		JOIN brand_cloud_users bcu ON bcu.brand_cloud_id = o.id
		JOIN brand_cloud_memberships bcm ON bcm.brand_cloud_id = o.id AND bcm.brand_cloud_user_id = bcu.id
		WHERE o.organization_kind = 'brand_cloud'
		  AND o.status = 'active'
		  AND o.tenant_slug = $1
		  AND bcu.email = $2
		  AND bcu.disabled_at IS NULL
	`, normalizeTenantSlug(tenantSlug), strings.ToLower(strings.TrimSpace(email))).Scan(
		&result.BrandCloud.ID,
		&result.BrandCloud.Name,
		&result.BrandCloud.TenantSlug,
		&result.BrandCloud.Role,
		&result.BrandCloud.OrganizationKind,
		&result.BrandCloud.Status,
		&result.BrandCloud.Tier,
		&result.BrandCloud.EvaluationDeviceQuota,
		&rawMetadata,
		&result.BrandCloud.CreatedAt,
		&result.BrandCloud.UpdatedAt,
		&result.BrandCloudUser.ID,
		&result.BrandCloudUser.BrandCloudID,
		&result.BrandCloudUser.Email,
		&result.PasswordHash,
		&result.BrandCloudUser.DisplayName,
		&result.BrandCloudUser.EmailVerified,
		&result.BrandCloudUser.EmailVerifiedAt,
		&result.BrandCloudUser.SignupPendingVerification,
		&result.BrandCloudUser.CreatedAt,
		&result.BrandCloudUser.UpdatedAt,
		&result.BrandCloudUser.DisabledAt,
		&result.Member.Role,
		&result.Member.CreatedAt,
		&result.Member.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return BrandCloudLoginResult{}, ErrNotFound
	}
	if err != nil {
		return BrandCloudLoginResult{}, err
	}
	if err := json.Unmarshal(rawMetadata, &result.BrandCloud.Metadata); err != nil {
		return BrandCloudLoginResult{}, err
	}
	result.Member.BrandCloudID = result.BrandCloud.ID
	result.Member.BrandCloudUserID = result.BrandCloudUser.ID
	result.Member.Email = result.BrandCloudUser.Email
	result.Member.DisplayName = result.BrandCloudUser.DisplayName
	return result, nil
}

func (s *Store) GetBrandCloudUser(ctx context.Context, brandCloudUserID string) (model.BrandCloudUser, error) {
	user, err := scanBrandCloudUser(s.db.QueryRow(ctx, `
		SELECT id::text, brand_cloud_id::text, email, display_name, email_verified, email_verified_at, signup_pending_verification, created_at, updated_at, disabled_at
		FROM brand_cloud_users
		WHERE id = $1 AND disabled_at IS NULL
	`, brandCloudUserID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.BrandCloudUser{}, ErrNotFound
	}
	return user, err
}

func (s *Store) GetBrandCloudMember(ctx context.Context, brandCloudID, brandCloudUserID string) (model.BrandCloudMember, error) {
	member, err := scanBrandCloudMember(s.db.QueryRow(ctx, `
		SELECT bcm.brand_cloud_id::text, bcm.brand_cloud_user_id::text, bcm.role, bcm.created_at, bcm.updated_at
		FROM brand_cloud_memberships bcm
		JOIN brand_cloud_users bcu ON bcu.id = bcm.brand_cloud_user_id AND bcu.disabled_at IS NULL
		WHERE bcm.brand_cloud_id = $1 AND bcm.brand_cloud_user_id = $2
	`, brandCloudID, brandCloudUserID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.BrandCloudMember{}, ErrNotFound
	}
	return member, err
}

func (s *Store) SaveBrandCloudRefreshToken(ctx context.Context, brandCloudUserID, brandCloudID, tokenHash string, expiresAt time.Time) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO brand_cloud_refresh_tokens (brand_cloud_user_id, brand_cloud_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)
	`, brandCloudUserID, brandCloudID, tokenHash, expiresAt)
	return err
}

func (s *Store) RotateBrandCloudRefreshToken(ctx context.Context, oldTokenHash, newTokenHash, brandCloudUserID, brandCloudID string, newExpiresAt time.Time) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var activeUserID, activeBrandCloudID string
	err = tx.QueryRow(ctx, `
		SELECT rt.brand_cloud_user_id::text, rt.brand_cloud_id::text
		FROM brand_cloud_refresh_tokens rt
		JOIN brand_cloud_users bcu ON bcu.id = rt.brand_cloud_user_id
		WHERE rt.token_hash = $1
		  AND rt.revoked_at IS NULL
		  AND rt.expires_at > now()
		  AND bcu.disabled_at IS NULL
		FOR UPDATE
	`, oldTokenHash).Scan(&activeUserID, &activeBrandCloudID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if activeUserID != brandCloudUserID || activeBrandCloudID != brandCloudID {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `
		UPDATE brand_cloud_refresh_tokens
		SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL
	`, oldTokenHash); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO brand_cloud_refresh_tokens (brand_cloud_user_id, brand_cloud_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)
	`, brandCloudUserID, brandCloudID, newTokenHash, newExpiresAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) RevokeBrandCloudRefreshToken(ctx context.Context, tokenHash string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE brand_cloud_refresh_tokens
		SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL
	`, tokenHash)
	return err
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
	if err := row.Scan(&org.ID, &org.Name, &org.TenantSlug, &role, &org.OrganizationKind, &org.Status, &org.Tier, &org.EvaluationDeviceQuota, &rawMetadata, &org.CreatedAt, &org.UpdatedAt); err != nil {
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

func normalizeTenantSlug(slug string) string {
	slug = strings.ToLower(strings.TrimSpace(slug))
	slug = slugUnsafePattern.ReplaceAllString(slug, "-")
	return strings.Trim(slug, "-")
}

func generatedTenantSlug(name, id string) string {
	base := normalizeTenantSlug(name)
	if base == "" {
		base = "brand"
	}
	suffix := strings.ReplaceAll(id, "-", "")
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	return base + "-" + suffix
}

func randomTenantSlugSuffix() (string, error) {
	var data [4]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}

func scanMember(row scanner) (model.Member, error) {
	var member model.Member
	err := row.Scan(&member.OrganizationID, &member.UserID, &member.Role, &member.CreatedAt, &member.UpdatedAt)
	return member, err
}

func scanBrandCloudUser(row scanner) (model.BrandCloudUser, error) {
	var user model.BrandCloudUser
	err := row.Scan(
		&user.ID,
		&user.BrandCloudID,
		&user.Email,
		&user.DisplayName,
		&user.EmailVerified,
		&user.EmailVerifiedAt,
		&user.SignupPendingVerification,
		&user.CreatedAt,
		&user.UpdatedAt,
		&user.DisabledAt,
	)
	return user, err
}

func scanBrandCloudMember(row scanner) (model.BrandCloudMember, error) {
	var member model.BrandCloudMember
	err := row.Scan(&member.BrandCloudID, &member.BrandCloudUserID, &member.Role, &member.CreatedAt, &member.UpdatedAt)
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
