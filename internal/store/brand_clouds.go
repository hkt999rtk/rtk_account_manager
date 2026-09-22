package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

var slugUnsafePattern = regexp.MustCompile(`[^a-z0-9]+`)

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Store) CreateBrandCloud(ctx context.Context, actorUserID string, in BrandCloudInput) (model.Organization, error) {
	ownerID := strings.TrimSpace(in.OwnerUserID)
	if ownerID == "" {
		return model.Organization{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.Organization{}, err
	}
	defer tx.Rollback(ctx)

	// Platform privilege does not make the operator the billing owner and does
	// not bypass the designated owner's activation or ownership quota.
	org, err := createDeveloperBrandCloudTx(ctx, tx, ownerID, in, true)
	if err != nil {
		return model.Organization{}, err
	}
	org.Role = "" // The platform actor has not been granted a cloud membership.
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "brand_cloud_created",
		ActorUserID:    &actorUserID,
		OrganizationID: &org.ID,
		SubjectType:    "brand_cloud",
		SubjectID:      org.ID,
		Payload: map[string]any{
			"owner_user_id":     ownerID,
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
		SELECT id::text, name, tenant_slug, ''::text, organization_kind, status, tier, evaluation_device_quota, metadata, created_at, updated_at, pki_status, pki_operation_id::text, COALESCE(pki_issuer_id::text,'')
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
		SELECT id::text, name, tenant_slug, ''::text, organization_kind, status, tier, evaluation_device_quota, metadata, created_at, updated_at, pki_status, pki_operation_id::text, COALESCE(pki_issuer_id::text,'')
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
		SELECT id::text, name, tenant_slug, ''::text, organization_kind, status, tier, evaluation_device_quota, metadata, created_at, updated_at, pki_status, pki_operation_id::text, COALESCE(pki_issuer_id::text,'')
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
		RETURNING id::text, name, tenant_slug, ''::text, organization_kind, status, tier, evaluation_device_quota, metadata, created_at, updated_at, pki_status, pki_operation_id::text, COALESCE(pki_issuer_id::text,'')
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

func (s *Store) ProvisionBrandCloudAccount(ctx context.Context, actorUserID, orgID string, in BrandCloudAccountInput) (BrandCloudAccountResult, error) {
	if in.Role != model.RoleAdmin && in.Role != model.RoleMember {
		return BrandCloudAccountResult{}, ErrConflict
	}
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
	if !newUser {
		var isOwner bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM organization_members WHERE organization_id=$1 AND user_id=$2 AND role='owner')`, orgID, user.ID).Scan(&isOwner); err != nil {
			return BrandCloudAccountResult{}, err
		}
		if isOwner {
			return BrandCloudAccountResult{}, ErrLastOwner
		}
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
	member, err := scanDeveloperMember(tx.QueryRow(ctx, `INSERT INTO organization_members (organization_id,user_id,role,disabled_at)
		VALUES ($1,$2,$3,CASE WHEN $4 THEN now() ELSE NULL END) ON CONFLICT DO NOTHING
		RETURNING organization_id::text,user_id::text,$5::text,$6::text,role,created_at,updated_at,disabled_at,access_scope`, orgID, user.ID, in.Role, memberDisabled, user.Email, user.DisplayName))
	holdEligible := err == nil && memberDisabled
	if errors.Is(err, pgx.ErrNoRows) {
		// Lock the existing row before judging provenance. An earlier
		// administrative disable must never be converted into an email hold.
		member, err = scanDeveloperMember(tx.QueryRow(ctx, `SELECT organization_id::text,user_id::text,$3::text,$4::text,role,created_at,updated_at,disabled_at,access_scope
			FROM organization_members WHERE organization_id=$1 AND user_id=$2 FOR UPDATE`, orgID, user.ID, user.Email, user.DisplayName))
		if err != nil {
			return BrandCloudAccountResult{}, err
		}
		if memberDisabled && member.Role == in.Role {
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_member_activation_holds h
				JOIN organization_members m USING(organization_id,user_id)
				WHERE h.organization_id=$1 AND h.user_id=$2 AND h.disabled_at=m.disabled_at AND h.updated_at=m.updated_at)`, orgID, user.ID).Scan(&holdEligible); err != nil {
				return BrandCloudAccountResult{}, err
			}
		}
		member, err = scanDeveloperMember(tx.QueryRow(ctx, `UPDATE organization_members SET role=$3,
			disabled_at=CASE WHEN $4 THEN disabled_at ELSE NULL END,updated_at=now()
			WHERE organization_id=$1 AND user_id=$2
			RETURNING organization_id::text,user_id::text,$5::text,$6::text,role,created_at,updated_at,disabled_at,access_scope`, orgID, user.ID, in.Role, memberDisabled, user.Email, user.DisplayName))
	}
	if err != nil {
		return BrandCloudAccountResult{}, err
	}
	if holdEligible {
		if _, err := tx.Exec(ctx, `INSERT INTO organization_member_activation_holds(organization_id,user_id,disabled_at,updated_at,source)
			SELECT organization_id,user_id,disabled_at,updated_at,'provisioning' FROM organization_members
			WHERE organization_id=$1 AND user_id=$2 AND disabled_at IS NOT NULL
			ON CONFLICT(organization_id,user_id) DO UPDATE SET disabled_at=EXCLUDED.disabled_at,updated_at=EXCLUDED.updated_at,source=EXCLUDED.source`, orgID, user.ID); err != nil {
			return BrandCloudAccountResult{}, err
		}
	}
	if in.ActivationMode == "immediate" {
		if _, err := tx.Exec(ctx, `INSERT INTO role_assignments (role_id,actor_type,actor_id,scope_type,scope_id,organization_id,created_by) SELECT id,'user',$1,'organization',$2,$3,$4 FROM roles WHERE name=$5 AND disabled_at IS NULL ON CONFLICT DO NOTHING`, user.ID, orgID, orgID, actorUserID, in.Role); err != nil {
			return BrandCloudAccountResult{}, err
		}
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
	filter := `m.organization_id=$1 AND ($2='' OR ($2='active' AND m.disabled_at IS NULL AND u.disabled_at IS NULL AND u.email_verified=true AND u.signup_pending_verification=false) OR ($2='pending_verification' AND (u.signup_pending_verification=true OR u.email_verified=false)) OR ($2='disabled' AND (m.disabled_at IS NOT NULL OR u.disabled_at IS NOT NULL))) AND ($3='' OR lower(u.email) LIKE '%'||$3||'%' OR lower(coalesce(u.display_name,'')) LIKE '%'||$3||'%' OR u.id::text=$3)`
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
	if err := row.Scan(&org.ID, &org.Name, &org.TenantSlug, &role, &org.OrganizationKind, &org.Status, &org.Tier, &org.EvaluationDeviceQuota, &rawMetadata, &org.CreatedAt, &org.UpdatedAt, &org.PKIStatus, &org.PKIOperationID, &org.PKIIssuerID); err != nil {
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
