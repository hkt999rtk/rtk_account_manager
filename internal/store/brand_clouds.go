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

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

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

func (s *Store) AssignBrandCloudMember(ctx context.Context, actorUserID, orgID, brandCloudUserID string, role model.Role) (model.BrandCloudMember, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.BrandCloudMember{}, err
	}
	defer tx.Rollback(ctx)

	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM organizations
			WHERE id = $1 AND organization_kind = 'brand_cloud'
		)
	`, orgID).Scan(&exists); err != nil {
		return model.BrandCloudMember{}, err
	}
	if !exists {
		return model.BrandCloudMember{}, ErrNotFound
	}

	member, err := scanBrandCloudMember(tx.QueryRow(ctx, `
		INSERT INTO brand_cloud_memberships (brand_cloud_id, brand_cloud_user_id, role)
		SELECT $1, id, $3
		FROM brand_cloud_users
		WHERE brand_cloud_id = $1 AND id = $2 AND disabled_at IS NULL
		ON CONFLICT ON CONSTRAINT brand_cloud_memberships_brand_user_key
		DO UPDATE SET role = EXCLUDED.role, updated_at = now()
		RETURNING brand_cloud_id::text, brand_cloud_user_id::text, role, created_at, updated_at
	`, orgID, brandCloudUserID, role))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.BrandCloudMember{}, ErrNotFound
	}
	if err != nil {
		return model.BrandCloudMember{}, err
	}
	user, err := scanBrandCloudUser(tx.QueryRow(ctx, `
		SELECT id::text, brand_cloud_id::text, email, display_name, email_verified, email_verified_at, signup_pending_verification, created_at, updated_at, disabled_at
		FROM brand_cloud_users
		WHERE brand_cloud_id = $1 AND id = $2
	`, orgID, member.BrandCloudUserID))
	if err != nil {
		return model.BrandCloudMember{}, err
	}
	member.Email = user.Email
	member.DisplayName = user.DisplayName

	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "brand_cloud_member_assigned",
		ActorUserID:    &actorUserID,
		OrganizationID: &orgID,
		SubjectType:    "brand_cloud",
		SubjectID:      orgID,
		Payload: map[string]any{
			"brand_cloud_user_id": member.BrandCloudUserID,
			"email":               member.Email,
			"role":                member.Role,
		},
	}); err != nil {
		return model.BrandCloudMember{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.BrandCloudMember{}, err
	}
	return member, nil
}

func (s *Store) CreateBrandCloudUser(ctx context.Context, actorUserID, orgID string, in BrandCloudUserInput) (BrandCloudUserResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return BrandCloudUserResult{}, err
	}
	defer tx.Rollback(ctx)

	var tenantSlug, brandCloudName string
	if err := tx.QueryRow(ctx, `
		SELECT tenant_slug, name
		FROM organizations
		WHERE id = $1 AND organization_kind = 'brand_cloud'
	`, orgID).Scan(&tenantSlug, &brandCloudName); errors.Is(err, pgx.ErrNoRows) {
		return BrandCloudUserResult{}, ErrNotFound
	} else if err != nil {
		return BrandCloudUserResult{}, err
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
	if in.ActivationMode == "email" && err == nil {
		return BrandCloudUserResult{}, ErrConflict
	}
	action := "assigned"
	if errors.Is(err, pgx.ErrNoRows) {
		action = "created"
	}

	emailVerified := in.ActivationMode != "email"
	brandCloudUser, err := scanBrandCloudUser(tx.QueryRow(ctx, `
		INSERT INTO brand_cloud_users (brand_cloud_id, email, password_hash, display_name, email_verified, email_verified_at, signup_pending_verification, disabled_at)
		VALUES ($1, $2, $3, $4, $6, CASE WHEN $6 THEN now() ELSE NULL END, NOT $6, NULL)
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
	`, orgID, email, in.PasswordHash, in.DisplayName, in.RotatePassword, emailVerified))
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
	if in.ActivationMode == "email" {
		if in.ActivationTokenHash == "" || in.ActivationExpiresAt.IsZero() || in.ActivationEmail == nil {
			return BrandCloudUserResult{}, errors.New("email activation token and outbox are required")
		}
		outbox := *in.ActivationEmail
		outbox.Payload.TenantSlug = normalizeTenantSlug(tenantSlug)
		outbox.Payload.OrganizationID = orgID
		outbox.Payload.OrganizationName = brandCloudName
		if err := s.createAuthTokenForSubjectWithEmailTx(
			ctx, tx, "brand_cloud_user", brandCloudUser.ID, "",
			"brand_cloud_user_activation", brandCloudAuthTokenScope(tenantSlug),
			in.ActivationTokenHash, in.ActivationExpiresAt, &outbox,
		); err != nil {
			return BrandCloudUserResult{}, err
		}
	}

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
			"brand_cloud_user_id": brandCloudUser.ID,
			"email":               brandCloudUser.Email,
			"role":                brandCloudMember.Role,
			"rotate_password":     in.RotatePassword,
			"activation_mode":     in.ActivationMode,
		},
	}); err != nil {
		return BrandCloudUserResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BrandCloudUserResult{}, err
	}
	return BrandCloudUserResult{Action: action, BrandCloudUser: brandCloudUser, BrandCloudMember: brandCloudMember}, nil
}

func (s *Store) ProvisionBrandCloudAccount(ctx context.Context, actorUserID, orgID string, in BrandCloudAccountInput) (BrandCloudAccountResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return BrandCloudAccountResult{}, err
	}
	defer tx.Rollback(ctx)

	var brandCloudName string
	if err := tx.QueryRow(ctx, `SELECT name FROM organizations WHERE id = $1 AND organization_kind = 'brand_cloud' AND status = 'active' FOR UPDATE`, orgID).Scan(&brandCloudName); errors.Is(err, pgx.ErrNoRows) {
		return BrandCloudAccountResult{}, ErrNotFound
	} else if err != nil {
		return BrandCloudAccountResult{}, err
	}

	email := strings.ToLower(strings.TrimSpace(in.Email))
	var user model.User
	var existingPasswordHash string
	err = tx.QueryRow(ctx, `SELECT id::text,email,password_hash,display_name,email_verified,email_verified_at,signup_pending_verification,developer_cloud_limit,created_at,updated_at,disabled_at FROM users WHERE email=$1 FOR UPDATE`, email).
		Scan(&user.ID, &user.Email, &existingPasswordHash, &user.DisplayName, &user.EmailVerified, &user.EmailVerifiedAt, &user.SignupPendingVerification, &user.DeveloperCloudLimit, &user.CreatedAt, &user.UpdatedAt, &user.DisabledAt)
	newUser := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !newUser {
		return BrandCloudAccountResult{}, err
	}
	if !newUser && user.DisabledAt != nil {
		return BrandCloudAccountResult{}, ErrConflict
	}
	action := "assigned"
	if newUser {
		action = "created"
		verified := in.ActivationMode == "immediate"
		user, err = scanDeveloperUser(tx.QueryRow(ctx, `INSERT INTO users (email,password_hash,display_name,email_verified,email_verified_at,signup_pending_verification) VALUES ($1,$2,$3,$4,CASE WHEN $4 THEN now() ELSE NULL END,NOT $4) RETURNING id::text,email,display_name,email_verified,email_verified_at,signup_pending_verification,developer_cloud_limit,created_at,updated_at,disabled_at`, email, in.PasswordHash, in.DisplayName, verified))
		if err != nil {
			if isUniqueViolation(err) {
				return BrandCloudAccountResult{}, ErrConflict
			}
			return BrandCloudAccountResult{}, err
		}
	} else if in.ActivationMode == "immediate" && in.RotatePassword {
		user, err = scanDeveloperUser(tx.QueryRow(ctx, `UPDATE users SET password_hash=$2,display_name=COALESCE($3,display_name),email_verified=true,email_verified_at=COALESCE(email_verified_at,now()),signup_pending_verification=false,updated_at=now() WHERE id=$1 RETURNING id::text,email,display_name,email_verified,email_verified_at,signup_pending_verification,developer_cloud_limit,created_at,updated_at,disabled_at`, user.ID, in.PasswordHash, in.DisplayName))
		if err != nil {
			return BrandCloudAccountResult{}, err
		}
	}

	memberDisabled := in.ActivationMode == "email" && !user.EmailVerified
	member, err := scanDeveloperMember(tx.QueryRow(ctx, `INSERT INTO organization_members (organization_id,user_id,role,disabled_at) VALUES ($1,$2,$3,CASE WHEN $4 THEN now() ELSE NULL END) ON CONFLICT (organization_id,user_id) DO UPDATE SET role=EXCLUDED.role,disabled_at=CASE WHEN $4 THEN organization_members.disabled_at ELSE NULL END,updated_at=now() RETURNING organization_id::text,user_id::text,$5::text,$6::text,role,created_at,updated_at,disabled_at`, orgID, user.ID, in.Role, memberDisabled, user.Email, user.DisplayName))
	if err != nil {
		return BrandCloudAccountResult{}, err
	}

	if in.ActivationMode == "email" {
		if in.ActivationTokenHash == "" || in.ActivationExpiresAt.IsZero() || in.ActivationEmail == nil {
			return BrandCloudAccountResult{}, errors.New("email activation token and outbox are required")
		}
		purpose := "login_activation"
		if !user.EmailVerified {
			purpose = "email_verification"
		}
		outbox := *in.ActivationEmail
		outbox.MessageType = purpose
		outbox.Payload.OrganizationID = orgID
		outbox.Payload.OrganizationName = brandCloudName
		if err := s.createAuthTokenForSubjectWithEmailTx(ctx, tx, "user", user.ID, user.ID, purpose, "", in.ActivationTokenHash, in.ActivationExpiresAt, &outbox); err != nil {
			return BrandCloudAccountResult{}, err
		}
	}

	if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "brand_cloud_account_" + action, ActorUserID: &actorUserID, OrganizationID: &orgID, SubjectType: "user", SubjectID: user.ID, Payload: map[string]any{"user_id": user.ID, "email": user.Email, "role": member.Role, "activation_mode": in.ActivationMode}}); err != nil {
		return BrandCloudAccountResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BrandCloudAccountResult{}, err
	}
	return BrandCloudAccountResult{Action: action, User: user, Member: member}, nil
}

func (s *Store) ListBrandCloudAccounts(ctx context.Context, in BrandCloudAccountListFilter) (BrandCloudAccountPage, error) {
	if exists, err := s.brandCloudExists(ctx, in.BrandCloudID); err != nil {
		return BrandCloudAccountPage{}, err
	} else if !exists {
		return BrandCloudAccountPage{}, ErrNotFound
	}
	status, query := strings.TrimSpace(in.Status), strings.ToLower(strings.TrimSpace(in.Query))
	filter := `m.organization_id=$1 AND ($2='' OR ($2='active' AND m.disabled_at IS NULL AND u.disabled_at IS NULL AND u.signup_pending_verification=false) OR ($2='pending_verification' AND m.disabled_at IS NOT NULL AND u.signup_pending_verification=true) OR ($2='disabled' AND (m.disabled_at IS NOT NULL OR u.disabled_at IS NOT NULL))) AND ($3='' OR lower(u.email) LIKE '%'||$3||'%' OR lower(coalesce(u.display_name,'')) LIKE '%'||$3||'%' OR u.id::text=$3)`
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM organization_members m JOIN users u ON u.id=m.user_id WHERE `+filter, in.BrandCloudID, status, query).Scan(&total); err != nil {
		return BrandCloudAccountPage{}, err
	}
	rows, err := s.db.Query(ctx, `SELECT m.organization_id::text,m.user_id::text,u.email,u.display_name,m.role,m.created_at,m.updated_at,COALESCE(m.disabled_at,u.disabled_at),u.email_verified,u.signup_pending_verification FROM organization_members m JOIN users u ON u.id=m.user_id WHERE `+filter+` ORDER BY m.created_at LIMIT $4 OFFSET $5`, in.BrandCloudID, status, query, in.Limit, in.Offset)
	if err != nil {
		return BrandCloudAccountPage{}, err
	}
	defer rows.Close()
	accounts := []model.BrandCloudAccountListItem{}
	for rows.Next() {
		var account model.BrandCloudAccountListItem
		scanErr := rows.Scan(&account.OrganizationID, &account.UserID, &account.Email, &account.DisplayName, &account.Role, &account.CreatedAt, &account.UpdatedAt, &account.DisabledAt, &account.EmailVerified, &account.SignupPendingVerification)
		if scanErr != nil {
			return BrandCloudAccountPage{}, scanErr
		}
		accounts = append(accounts, account)
	}
	return BrandCloudAccountPage{Accounts: accounts, Page: Page{Limit: in.Limit, Offset: in.Offset, Total: total}}, rows.Err()
}

func (s *Store) ActivateBrandCloudUser(ctx context.Context, tenantSlug, tokenHash, passwordHash string) (BrandCloudLoginResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return BrandCloudLoginResult{}, err
	}
	defer tx.Rollback(ctx)

	var brandCloudUserID string
	err = tx.QueryRow(ctx, `
		SELECT bcu.id::text
		FROM auth_tokens at
		JOIN brand_cloud_users bcu ON bcu.id = at.subject_id
		JOIN organizations o ON o.id = bcu.brand_cloud_id
		WHERE at.token_hash = $1
		  AND at.subject_type = 'brand_cloud_user'
		  AND at.purpose = 'brand_cloud_user_activation'
		  AND at.scope = $2
		  AND at.consumed_at IS NULL
		  AND at.expires_at > now()
		  AND o.organization_kind = 'brand_cloud'
		  AND o.status = 'active'
		  AND o.tenant_slug = $3
		  AND bcu.disabled_at IS NULL
		  AND bcu.signup_pending_verification = true
		  AND bcu.email_verified = false
		FOR UPDATE OF at, bcu
	`, tokenHash, brandCloudAuthTokenScope(tenantSlug), normalizeTenantSlug(tenantSlug)).Scan(&brandCloudUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return BrandCloudLoginResult{}, ErrNotFound
	}
	if err != nil {
		return BrandCloudLoginResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE brand_cloud_users
		SET password_hash = $2,
		    email_verified = true,
		    email_verified_at = now(),
		    signup_pending_verification = false,
		    updated_at = now()
		WHERE id = $1
	`, brandCloudUserID, passwordHash); err != nil {
		return BrandCloudLoginResult{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE auth_tokens SET consumed_at = now() WHERE token_hash = $1`, tokenHash); err != nil {
		return BrandCloudLoginResult{}, err
	}
	result, err := getBrandCloudLoginResultByID(ctx, tx, tenantSlug, brandCloudUserID)
	if err != nil {
		return BrandCloudLoginResult{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "brand_cloud_user_activated",
		OrganizationID: &result.BrandCloud.ID,
		SubjectType:    "brand_cloud_user",
		SubjectID:      result.BrandCloudUser.ID,
		Payload: map[string]any{
			"token_purpose":       "brand_cloud_user_activation",
			"tenant_slug":         normalizeTenantSlug(tenantSlug),
			"brand_cloud_user_id": result.BrandCloudUser.ID,
		},
	}); err != nil {
		return BrandCloudLoginResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BrandCloudLoginResult{}, err
	}
	return result, nil
}

func (s *Store) GetBrandCloudUserPassword(ctx context.Context, tenantSlug, email string) (BrandCloudLoginResult, error) {
	return getBrandCloudLoginResultByEmail(ctx, s.db, tenantSlug, email)
}

func (s *Store) CreateBrandCloudLoginActivationTokenForEmail(ctx context.Context, tenantSlug, email, tokenHash string, expiresAt time.Time) (bool, error) {
	result, err := s.GetBrandCloudUserPassword(ctx, tenantSlug, email)
	if errors.Is(err, ErrNotFound) || result.BrandCloudUser.SignupPendingVerification {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, s.createAuthTokenForSubject(ctx, "brand_cloud_user", result.BrandCloudUser.ID, "", "login_activation", brandCloudAuthTokenScope(tenantSlug), tokenHash, expiresAt)
}

func (s *Store) CreateBrandCloudLoginActivationTokenForEmailAndEmail(ctx context.Context, tenantSlug, email, tokenHash string, expiresAt time.Time, outbox EmailOutboxInput) (bool, error) {
	result, err := s.GetBrandCloudUserPassword(ctx, tenantSlug, email)
	if errors.Is(err, ErrNotFound) || result.BrandCloudUser.SignupPendingVerification {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = s.createAuthTokenForSubjectWithEmail(
		ctx,
		"brand_cloud_user",
		result.BrandCloudUser.ID,
		"",
		"login_activation",
		brandCloudAuthTokenScope(tenantSlug),
		tokenHash,
		expiresAt,
		&outbox,
	)
	return true, err
}

func (s *Store) ActivateBrandCloudLoginToken(ctx context.Context, tenantSlug, tokenHash string) (BrandCloudLoginResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return BrandCloudLoginResult{}, err
	}
	defer tx.Rollback(ctx)

	var brandCloudUserID string
	err = tx.QueryRow(ctx, `
		SELECT bcu.id::text
		FROM auth_tokens at
		JOIN brand_cloud_users bcu ON bcu.id = at.subject_id
		JOIN organizations o ON o.id = bcu.brand_cloud_id
		WHERE at.token_hash = $1
		  AND at.subject_type = 'brand_cloud_user'
		  AND at.purpose = 'login_activation'
		  AND at.scope = $2
		  AND at.consumed_at IS NULL
		  AND at.expires_at > now()
		  AND o.organization_kind = 'brand_cloud'
		  AND o.status = 'active'
		  AND o.tenant_slug = $3
		  AND bcu.disabled_at IS NULL
		  AND bcu.signup_pending_verification = false
		FOR UPDATE OF at
	`, tokenHash, brandCloudAuthTokenScope(tenantSlug), normalizeTenantSlug(tenantSlug)).Scan(&brandCloudUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return BrandCloudLoginResult{}, ErrNotFound
	}
	if err != nil {
		return BrandCloudLoginResult{}, err
	}

	result, err := getBrandCloudLoginResultByID(ctx, tx, tenantSlug, brandCloudUserID)
	if err != nil {
		return BrandCloudLoginResult{}, err
	}
	if result.BrandCloudUser.SignupPendingVerification {
		return BrandCloudLoginResult{}, ErrNotFound
	}
	if _, err := tx.Exec(ctx, `
		UPDATE auth_tokens
		SET consumed_at = now()
		WHERE token_hash = $1
	`, tokenHash); err != nil {
		return BrandCloudLoginResult{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "brand_cloud_login_activation_consumed",
		OrganizationID: &result.BrandCloud.ID,
		SubjectType:    "brand_cloud_user",
		SubjectID:      result.BrandCloudUser.ID,
		Payload: map[string]any{
			"token_purpose":       "login_activation",
			"tenant_slug":         normalizeTenantSlug(tenantSlug),
			"email":               result.BrandCloudUser.Email,
			"brand_cloud_user_id": result.BrandCloudUser.ID,
		},
	}); err != nil {
		return BrandCloudLoginResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BrandCloudLoginResult{}, err
	}
	return result, nil
}

func brandCloudAuthTokenScope(tenantSlug string) string {
	return "brand_cloud:" + normalizeTenantSlug(tenantSlug)
}

func getBrandCloudLoginResultByEmail(ctx context.Context, q rowQuerier, tenantSlug, email string) (BrandCloudLoginResult, error) {
	return getBrandCloudLoginResult(ctx, q, tenantSlug, strings.ToLower(strings.TrimSpace(email)), "")
}

func getBrandCloudLoginResultByID(ctx context.Context, q rowQuerier, tenantSlug, brandCloudUserID string) (BrandCloudLoginResult, error) {
	return getBrandCloudLoginResult(ctx, q, tenantSlug, "", brandCloudUserID)
}

func getBrandCloudLoginResult(ctx context.Context, q rowQuerier, tenantSlug, email, brandCloudUserID string) (BrandCloudLoginResult, error) {
	var result BrandCloudLoginResult
	var rawMetadata []byte
	err := q.QueryRow(ctx, `
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
		  AND ($2 = '' OR bcu.email = $2)
		  AND ($3 = '' OR bcu.id = NULLIF($3, '')::uuid)
		  AND bcu.disabled_at IS NULL
	`, normalizeTenantSlug(tenantSlug), email, brandCloudUserID).Scan(
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
	result.User.ID = result.BrandCloudUser.ID
	result.User.Email = result.BrandCloudUser.Email
	result.User.DisplayName = result.BrandCloudUser.DisplayName
	result.User.EmailVerified = result.BrandCloudUser.EmailVerified
	result.User.EmailVerifiedAt = result.BrandCloudUser.EmailVerifiedAt
	result.User.SignupPendingVerification = result.BrandCloudUser.SignupPendingVerification
	result.User.CreatedAt = result.BrandCloudUser.CreatedAt
	result.User.UpdatedAt = result.BrandCloudUser.UpdatedAt
	result.User.DisabledAt = result.BrandCloudUser.DisabledAt
	return result, nil
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

func (s *Store) brandCloudExists(ctx context.Context, brandCloudID string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM organizations
			WHERE id = $1 AND organization_kind = 'brand_cloud'
		)
	`, brandCloudID).Scan(&exists)
	return exists, err
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
