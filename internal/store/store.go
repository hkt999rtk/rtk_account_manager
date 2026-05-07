package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rtk_account_manager/internal/model"
)

var (
	ErrNotFound                 = errors.New("not found")
	ErrLastOwner                = errors.New("last owner cannot be removed or downgraded")
	ErrDisabled                 = errors.New("resource is disabled")
	ErrConflict                 = errors.New("conflict")
	ErrNotProvisioned           = errors.New("device is not provisioned")
	ErrRateLimited              = errors.New("rate limited")
	ErrEvaluationQuotaExceeded  = errors.New("evaluation device quota exceeded")
	ErrClaimExpired             = errors.New("claim token expired")
	ErrClaimAlreadyClaimed      = errors.New("claim token already claimed")
	ErrClaimCrossOrganization   = errors.New("claim token belongs to another organization")
	ErrClaimUnsupportedCategory = errors.New("claim token category is unsupported")
)

type Store struct {
	db *pgxpool.Pool
}

func New(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

type Page struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}

type OrganizationPage struct {
	Organizations []model.Organization
	Page          Page
}

type MemberPage struct {
	Members []model.Member
	Page    Page
}

type DevicePage struct {
	Devices []model.Device
	Page    Page
}

type DeviceGroupPage struct {
	Groups []model.DeviceGroup
	Page   Page
}

type DeviceTagPage struct {
	Tags []model.DeviceTag
	Page Page
}

type RegisterInput struct {
	Email                     string
	PasswordHash              string
	DisplayName               *string
	OrganizationName          string
	OrganizationTier          model.OrganizationTier
	EvaluationDeviceQuota     int
	SignupPendingVerification bool
}

type RegisterResult struct {
	User         model.User         `json:"user"`
	Organization model.Organization `json:"organization"`
}

func (s *Store) Register(ctx context.Context, in RegisterInput) (RegisterResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return RegisterResult{}, err
	}
	defer tx.Rollback(ctx)

	var user model.User
	err = tx.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, display_name, signup_pending_verification)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text, email, display_name, email_verified, email_verified_at, signup_pending_verification, created_at, updated_at, disabled_at
	`, in.Email, in.PasswordHash, in.DisplayName, in.SignupPendingVerification).Scan(&user.ID, &user.Email, &user.DisplayName, &user.EmailVerified, &user.EmailVerifiedAt, &user.SignupPendingVerification, &user.CreatedAt, &user.UpdatedAt, &user.DisabledAt)
	if err != nil {
		return RegisterResult{}, err
	}

	tier := in.OrganizationTier
	if tier != model.OrganizationTierEvaluation {
		tier = model.OrganizationTierCommercial
	}
	quota := in.EvaluationDeviceQuota
	if quota <= 0 {
		quota = 5
	}
	var org model.Organization
	err = tx.QueryRow(ctx, `
		INSERT INTO organizations (name, tier, evaluation_device_quota)
		VALUES ($1, $2, $3)
		RETURNING id::text, name, tier, evaluation_device_quota, created_at, updated_at
	`, in.OrganizationName, tier, quota).Scan(&org.ID, &org.Name, &org.Tier, &org.EvaluationDeviceQuota, &org.CreatedAt, &org.UpdatedAt)
	if err != nil {
		return RegisterResult{}, err
	}
	org.Role = model.RoleOwner

	_, err = tx.Exec(ctx, `
		INSERT INTO organization_members (organization_id, user_id, role)
		VALUES ($1, $2, 'owner')
	`, org.ID, user.ID)
	if err != nil {
		return RegisterResult{}, err
	}

	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "signup_created",
		ActorUserID:    &user.ID,
		OrganizationID: &org.ID,
		SubjectType:    "organization",
		SubjectID:      org.ID,
		Payload: map[string]any{
			"user_id":                     user.ID,
			"email":                       user.Email,
			"organization_name":           org.Name,
			"organization_tier":           org.Tier,
			"evaluation_device_quota":     org.EvaluationDeviceQuota,
			"signup_pending_verification": in.SignupPendingVerification,
		},
	}); err != nil {
		return RegisterResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return RegisterResult{}, err
	}
	return RegisterResult{User: user, Organization: org}, nil
}

func (s *Store) GetUserPassword(ctx context.Context, email string) (model.User, string, error) {
	var user model.User
	var hash string
	err := s.db.QueryRow(ctx, `
		SELECT id::text, email, password_hash, display_name, email_verified, email_verified_at, signup_pending_verification, created_at, updated_at, disabled_at
		FROM users
		WHERE email = $1 AND disabled_at IS NULL
	`, email).Scan(&user.ID, &user.Email, &hash, &user.DisplayName, &user.EmailVerified, &user.EmailVerifiedAt, &user.SignupPendingVerification, &user.CreatedAt, &user.UpdatedAt, &user.DisabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, "", ErrNotFound
	}
	return user, hash, err
}

func (s *Store) GetUserPasswordByID(ctx context.Context, userID string) (model.User, string, error) {
	var user model.User
	var hash string
	err := s.db.QueryRow(ctx, `
		SELECT id::text, email, password_hash, display_name, email_verified, email_verified_at, signup_pending_verification, created_at, updated_at, disabled_at
		FROM users
		WHERE id = $1 AND disabled_at IS NULL
	`, userID).Scan(&user.ID, &user.Email, &hash, &user.DisplayName, &user.EmailVerified, &user.EmailVerifiedAt, &user.SignupPendingVerification, &user.CreatedAt, &user.UpdatedAt, &user.DisabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, "", ErrNotFound
	}
	return user, hash, err
}

func (s *Store) GetUser(ctx context.Context, userID string) (model.User, error) {
	var user model.User
	err := s.db.QueryRow(ctx, `
		SELECT id::text, email, display_name, email_verified, email_verified_at, signup_pending_verification, created_at, updated_at, disabled_at
		FROM users
		WHERE id = $1 AND disabled_at IS NULL
	`, userID).Scan(&user.ID, &user.Email, &user.DisplayName, &user.EmailVerified, &user.EmailVerifiedAt, &user.SignupPendingVerification, &user.CreatedAt, &user.UpdatedAt, &user.DisabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, ErrNotFound
	}
	return user, err
}

func (s *Store) CreateEmailVerificationToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error {
	return s.createAuthToken(ctx, userID, "email_verification", tokenHash, expiresAt)
}

func (s *Store) CreatePasswordResetToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error {
	return s.createAuthToken(ctx, userID, "password_reset", tokenHash, expiresAt)
}

func (s *Store) CreatePasswordResetTokenForEmail(ctx context.Context, email, tokenHash string, expiresAt time.Time) (bool, error) {
	var userID string
	err := s.db.QueryRow(ctx, `
		SELECT id::text
		FROM users
		WHERE email = $1 AND disabled_at IS NULL
	`, email).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, s.CreatePasswordResetToken(ctx, userID, tokenHash, expiresAt)
}

func (s *Store) CreateEmailVerificationTokenForEmail(ctx context.Context, email, tokenHash string, expiresAt time.Time) (bool, error) {
	var userID string
	var verified bool
	err := s.db.QueryRow(ctx, `
		SELECT id::text, email_verified
		FROM users
		WHERE email = $1 AND disabled_at IS NULL
	`, email).Scan(&userID, &verified)
	if errors.Is(err, pgx.ErrNoRows) || verified {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, s.CreateEmailVerificationToken(ctx, userID, tokenHash, expiresAt)
}

func (s *Store) VerifyEmailToken(ctx context.Context, tokenHash string) (model.User, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.User{}, err
	}
	defer tx.Rollback(ctx)

	userID, err := consumeAuthTokenTx(ctx, tx, tokenHash, "email_verification")
	if err != nil {
		return model.User{}, err
	}
	var signupPendingVerification bool
	if err := tx.QueryRow(ctx, `
		SELECT signup_pending_verification
		FROM users
		WHERE id = $1 AND disabled_at IS NULL
	`, userID).Scan(&signupPendingVerification); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.User{}, ErrNotFound
		}
		return model.User{}, err
	}
	var user model.User
	err = tx.QueryRow(ctx, `
		UPDATE users
		SET email_verified = true,
		    email_verified_at = COALESCE(email_verified_at, now()),
		    signup_pending_verification = false,
		    updated_at = now()
		WHERE id = $1 AND disabled_at IS NULL
		RETURNING id::text, email, display_name, email_verified, email_verified_at, signup_pending_verification, created_at, updated_at, disabled_at
	`, userID).Scan(&user.ID, &user.Email, &user.DisplayName, &user.EmailVerified, &user.EmailVerifiedAt, &user.SignupPendingVerification, &user.CreatedAt, &user.UpdatedAt, &user.DisabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, ErrNotFound
	}
	if err != nil {
		return model.User{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:   "email_verified",
		ActorUserID: &userID,
		SubjectType: "user",
		SubjectID:   userID,
		Payload: map[string]any{
			"token_purpose":               "email_verification",
			"signup_pending_verification": signupPendingVerification,
		},
	}); err != nil {
		return model.User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.User{}, err
	}
	return user, nil
}

func (s *Store) ResetPasswordWithToken(ctx context.Context, tokenHash, passwordHash string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	userID, err := consumeAuthTokenTx(ctx, tx, tokenHash, "password_reset")
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE users
		SET password_hash = $2, updated_at = now()
		WHERE id = $1 AND disabled_at IS NULL
	`, userID, passwordHash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL
	`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) createAuthToken(ctx context.Context, userID, purpose, tokenHash string, expiresAt time.Time) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var recent int
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM auth_tokens
		WHERE user_id = $1 AND purpose = $2 AND created_at > now() - interval '1 hour'
	`, userID, purpose).Scan(&recent); err != nil {
		return err
	}
	if recent >= 5 {
		return ErrRateLimited
	}
	if _, err := tx.Exec(ctx, `
		UPDATE auth_tokens
		SET consumed_at = now()
		WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL
	`, userID, purpose); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO auth_tokens (user_id, purpose, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)
	`, userID, purpose, tokenHash, expiresAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func consumeAuthTokenTx(ctx context.Context, tx pgx.Tx, tokenHash, purpose string) (string, error) {
	var userID string
	err := tx.QueryRow(ctx, `
		SELECT at.user_id::text
		FROM auth_tokens at
		JOIN users u ON u.id = at.user_id
		WHERE at.token_hash = $1
		  AND at.purpose = $2
		  AND at.consumed_at IS NULL
		  AND at.expires_at > now()
		  AND u.disabled_at IS NULL
		FOR UPDATE OF at
	`, tokenHash, purpose).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE auth_tokens
		SET consumed_at = now()
		WHERE token_hash = $1
	`, tokenHash); err != nil {
		return "", err
	}
	return userID, nil
}

func (s *Store) UpdateUserPassword(ctx context.Context, userID, passwordHash string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		UPDATE users
		SET password_hash = $2
		WHERE id = $1 AND disabled_at IS NULL
	`, userID, passwordHash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL
	`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) DisableCurrentUser(ctx context.Context, userID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var activeUserID string
	err = tx.QueryRow(ctx, `
		SELECT id::text
		FROM users
		WHERE id = $1 AND disabled_at IS NULL
		FOR UPDATE
	`, userID).Scan(&activeUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	rows, err := tx.Query(ctx, `
		SELECT organization_id::text
		FROM organization_members
		WHERE user_id = $1 AND role = 'owner'
		FOR UPDATE
	`, userID)
	if err != nil {
		return err
	}
	ownerOrgIDs := []string{}
	for rows.Next() {
		var orgID string
		if err := rows.Scan(&orgID); err != nil {
			rows.Close()
			return err
		}
		ownerOrgIDs = append(ownerOrgIDs, orgID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, orgID := range ownerOrgIDs {
		if err := ensureNotLastActiveOwnerTx(ctx, tx, orgID, userID); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE users SET disabled_at = now() WHERE id = $1
	`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL
	`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) SaveRefreshToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
	`, userID, tokenHash, expiresAt)
	return err
}

func (s *Store) RotateRefreshToken(ctx context.Context, oldTokenHash, newTokenHash, userID string, newExpiresAt time.Time) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var activeUserID string
	err = tx.QueryRow(ctx, `
		SELECT rt.user_id::text
		FROM refresh_tokens rt
		JOIN users u ON u.id = rt.user_id
		WHERE rt.token_hash = $1
		  AND rt.revoked_at IS NULL
		  AND rt.expires_at > now()
		  AND u.disabled_at IS NULL
		FOR UPDATE
	`, oldTokenHash).Scan(&activeUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if activeUserID != userID {
		return ErrNotFound
	}

	if _, err := tx.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL
	`, oldTokenHash); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
	`, userID, newTokenHash, newExpiresAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) RefreshTokenActive(ctx context.Context, tokenHash string) (string, error) {
	var userID string
	err := s.db.QueryRow(ctx, `
		SELECT rt.user_id::text
		FROM refresh_tokens rt
		JOIN users u ON u.id = rt.user_id
		WHERE rt.token_hash = $1
		  AND rt.revoked_at IS NULL
		  AND rt.expires_at > now()
		  AND u.disabled_at IS NULL
	`, tokenHash).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return userID, err
}

func (s *Store) RevokeRefreshToken(ctx context.Context, tokenHash string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL
	`, tokenHash)
	return err
}

func (s *Store) RevokeUserRefreshTokens(ctx context.Context, userID string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL
	`, userID)
	return err
}

func (s *Store) CleanupRefreshTokens(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.db.Exec(ctx, `
		DELETE FROM refresh_tokens
		WHERE expires_at < $1 OR (revoked_at IS NOT NULL AND revoked_at < $1)
	`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) ListOrganizations(ctx context.Context, userID string, limit, offset int) (OrganizationPage, error) {
	total, err := s.countOrganizations(ctx, userID)
	if err != nil {
		return OrganizationPage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT o.id::text, o.name, m.role, o.tier, o.evaluation_device_quota, o.created_at, o.updated_at
		FROM organizations o
		JOIN organization_members m ON m.organization_id = o.id
		JOIN users u ON u.id = m.user_id
		WHERE m.user_id = $1 AND u.disabled_at IS NULL
		ORDER BY o.created_at ASC
		LIMIT $2 OFFSET $3
	`, userID, limit, offset)
	if err != nil {
		return OrganizationPage{}, err
	}
	defer rows.Close()

	orgs := []model.Organization{}
	for rows.Next() {
		var org model.Organization
		if err := rows.Scan(&org.ID, &org.Name, &org.Role, &org.Tier, &org.EvaluationDeviceQuota, &org.CreatedAt, &org.UpdatedAt); err != nil {
			return OrganizationPage{}, err
		}
		orgs = append(orgs, org)
	}
	if err := rows.Err(); err != nil {
		return OrganizationPage{}, err
	}
	return OrganizationPage{Organizations: orgs, Page: Page{Limit: limit, Offset: offset, Total: total}}, nil
}

func (s *Store) CreateOrganization(ctx context.Context, userID, name string) (model.Organization, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.Organization{}, err
	}
	defer tx.Rollback(ctx)

	var org model.Organization
	err = tx.QueryRow(ctx, `
		INSERT INTO organizations (name, tier, evaluation_device_quota)
		VALUES ($1, 'commercial', 5)
		RETURNING id::text, name, tier, evaluation_device_quota, created_at, updated_at
	`, name).Scan(&org.ID, &org.Name, &org.Tier, &org.EvaluationDeviceQuota, &org.CreatedAt, &org.UpdatedAt)
	if err != nil {
		return model.Organization{}, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO organization_members (organization_id, user_id, role)
		VALUES ($1, $2, 'owner')
	`, org.ID, userID)
	if err != nil {
		return model.Organization{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Organization{}, err
	}
	org.Role = model.RoleOwner
	return org, nil
}

func (s *Store) GetOrganization(ctx context.Context, orgID, userID string) (model.Organization, error) {
	var org model.Organization
	err := s.db.QueryRow(ctx, `
		SELECT o.id::text, o.name, m.role, o.tier, o.evaluation_device_quota, o.created_at, o.updated_at
		FROM organizations o
		JOIN organization_members m ON m.organization_id = o.id
		WHERE o.id = $1 AND m.user_id = $2
	`, orgID, userID).Scan(&org.ID, &org.Name, &org.Role, &org.Tier, &org.EvaluationDeviceQuota, &org.CreatedAt, &org.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Organization{}, ErrNotFound
	}
	return org, err
}

func (s *Store) UpdateOrganization(ctx context.Context, orgID, userID, name string) (model.Organization, error) {
	var org model.Organization
	err := s.db.QueryRow(ctx, `
		UPDATE organizations o
		SET name = $3, updated_at = now()
		FROM organization_members m
		WHERE o.id = $1
		  AND m.organization_id = o.id
		  AND m.user_id = $2
		RETURNING o.id::text, o.name, m.role, o.tier, o.evaluation_device_quota, o.created_at, o.updated_at
	`, orgID, userID, name).Scan(&org.ID, &org.Name, &org.Role, &org.Tier, &org.EvaluationDeviceQuota, &org.CreatedAt, &org.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Organization{}, ErrNotFound
	}
	return org, err
}

func (s *Store) GetRole(ctx context.Context, orgID, userID string) (model.Role, error) {
	var role model.Role
	err := s.db.QueryRow(ctx, `
		SELECT m.role
		FROM organization_members m
		JOIN users u ON u.id = m.user_id
		WHERE m.organization_id = $1
		  AND m.user_id = $2
		  AND u.disabled_at IS NULL
	`, orgID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}

func (s *Store) ListMembers(ctx context.Context, orgID string, limit, offset int) (MemberPage, error) {
	total, err := s.countMembers(ctx, orgID)
	if err != nil {
		return MemberPage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT m.organization_id::text, m.user_id::text, u.email, u.display_name, m.role, m.created_at, m.updated_at, u.disabled_at
		FROM organization_members m
		JOIN users u ON u.id = m.user_id
		WHERE m.organization_id = $1
		ORDER BY m.created_at ASC
		LIMIT $2 OFFSET $3
	`, orgID, limit, offset)
	if err != nil {
		return MemberPage{}, err
	}
	defer rows.Close()

	members := []model.Member{}
	for rows.Next() {
		var member model.Member
		if err := rows.Scan(&member.OrganizationID, &member.UserID, &member.Email, &member.DisplayName, &member.Role, &member.CreatedAt, &member.UpdatedAt, &member.DisabledAt); err != nil {
			return MemberPage{}, err
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return MemberPage{}, err
	}
	return MemberPage{Members: members, Page: Page{Limit: limit, Offset: offset, Total: total}}, nil
}

func (s *Store) getMemberTx(ctx context.Context, tx pgx.Tx, orgID, userID string) (model.Member, error) {
	var member model.Member
	err := tx.QueryRow(ctx, `
		SELECT m.organization_id::text, m.user_id::text, u.email, u.display_name, m.role, m.created_at, m.updated_at, u.disabled_at
		FROM organization_members m
		JOIN users u ON u.id = m.user_id
		WHERE m.organization_id = $1 AND m.user_id = $2
	`, orgID, userID).Scan(&member.OrganizationID, &member.UserID, &member.Email, &member.DisplayName, &member.Role, &member.CreatedAt, &member.UpdatedAt, &member.DisabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Member{}, ErrNotFound
	}
	return member, err
}

func (s *Store) AddMember(ctx context.Context, orgID, email string, role model.Role) (model.Member, error) {
	var member model.Member
	err := s.db.QueryRow(ctx, `
		INSERT INTO organization_members (organization_id, user_id, role)
		SELECT $1, id, $3 FROM users WHERE email = $2 AND disabled_at IS NULL
		RETURNING organization_id::text, user_id::text, role, created_at, updated_at
	`, orgID, email, role).Scan(&member.OrganizationID, &member.UserID, &member.Role, &member.CreatedAt, &member.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Member{}, ErrNotFound
	}
	if err != nil {
		return model.Member{}, err
	}
	user, err := s.GetUser(ctx, member.UserID)
	if err != nil {
		return model.Member{}, err
	}
	member.Email = user.Email
	member.DisplayName = user.DisplayName
	member.DisabledAt = user.DisabledAt
	return member, nil
}

func (s *Store) UpdateMemberRole(ctx context.Context, orgID, userID string, role model.Role) (model.Member, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.Member{}, err
	}
	defer tx.Rollback(ctx)

	if role != model.RoleOwner {
		if err := ensureNotLastActiveOwnerTx(ctx, tx, orgID, userID); err != nil {
			return model.Member{}, err
		}
	}
	var member model.Member
	err = tx.QueryRow(ctx, `
		UPDATE organization_members
		SET role = $3, updated_at = now()
		WHERE organization_id = $1 AND user_id = $2
		RETURNING organization_id::text, user_id::text, role, created_at, updated_at
	`, orgID, userID, role).Scan(&member.OrganizationID, &member.UserID, &member.Role, &member.CreatedAt, &member.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Member{}, ErrNotFound
	}
	if err != nil {
		return model.Member{}, err
	}
	var email string
	var displayName *string
	err = tx.QueryRow(ctx, `
		SELECT email, display_name FROM users WHERE id = $1 AND disabled_at IS NULL
	`, userID).Scan(&email, &displayName)
	if err != nil {
		return model.Member{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Member{}, err
	}
	member.Email = email
	member.DisplayName = displayName
	return member, nil
}

func (s *Store) DisableMemberUser(ctx context.Context, orgID, userID string) (model.Member, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.Member{}, err
	}
	defer tx.Rollback(ctx)

	if err := ensureNotLastActiveOwnerTx(ctx, tx, orgID, userID); err != nil {
		return model.Member{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE users SET disabled_at = COALESCE(disabled_at, now()) WHERE id = $1
	`, userID); err != nil {
		return model.Member{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL
	`, userID); err != nil {
		return model.Member{}, err
	}
	member, err := s.getMemberTx(ctx, tx, orgID, userID)
	if err != nil {
		return model.Member{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Member{}, err
	}
	return member, nil
}

func (s *Store) EnableMemberUser(ctx context.Context, orgID, userID string) (model.Member, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.Member{}, err
	}
	defer tx.Rollback(ctx)

	if _, err := s.getMemberTx(ctx, tx, orgID, userID); err != nil {
		return model.Member{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE users SET disabled_at = NULL WHERE id = $1
	`, userID); err != nil {
		return model.Member{}, err
	}
	member, err := s.getMemberTx(ctx, tx, orgID, userID)
	if err != nil {
		return model.Member{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Member{}, err
	}
	return member, nil
}

func (s *Store) RemoveMember(ctx context.Context, orgID, userID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := ensureNotLastActiveOwnerTx(ctx, tx, orgID, userID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		DELETE FROM organization_members WHERE organization_id = $1 AND user_id = $2
	`, orgID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

func ensureNotLastActiveOwnerTx(ctx context.Context, tx pgx.Tx, orgID, userID string) error {
	var role model.Role
	err := tx.QueryRow(ctx, `
		SELECT role
		FROM organization_members
		WHERE organization_id = $1 AND user_id = $2
		FOR UPDATE
	`, orgID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if role != model.RoleOwner {
		return nil
	}

	rows, err := tx.Query(ctx, `
		SELECT m.user_id::text, u.disabled_at
		FROM organization_members m
		JOIN users u ON u.id = m.user_id
		WHERE m.organization_id = $1 AND m.role = 'owner'
		FOR UPDATE OF m, u
	`, orgID)
	if err != nil {
		return err
	}
	activeOtherOwners := 0
	for rows.Next() {
		var ownerID string
		var disabledAt *time.Time
		if err := rows.Scan(&ownerID, &disabledAt); err != nil {
			rows.Close()
			return err
		}
		if ownerID != userID && disabledAt == nil {
			activeOtherOwners++
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if activeOtherOwners == 0 {
		return ErrLastOwner
	}
	return nil
}

type DeviceInput struct {
	Name         string
	Category     model.DeviceCategory
	SerialNumber *string
	MACAddress   *string
	Manufacturer *string
	Model        *string
	Metadata     map[string]any
}

type DeviceGroupInput struct {
	Name        string
	Description *string
}

func (s *Store) CreateDevice(ctx context.Context, orgID string, in DeviceInput) (model.Device, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.Device{}, err
	}
	defer tx.Rollback(ctx)

	var tier model.OrganizationTier
	var quota int
	err = tx.QueryRow(ctx, `
		SELECT tier, evaluation_device_quota
		FROM organizations
		WHERE id = $1
		FOR UPDATE
	`, orgID).Scan(&tier, &quota)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Device{}, ErrNotFound
	}
	if err != nil {
		return model.Device{}, err
	}
	if tier == model.OrganizationTierEvaluation {
		var activeDevices int
		if err := tx.QueryRow(ctx, `
			SELECT count(*)
			FROM devices
			WHERE organization_id = $1 AND disabled_at IS NULL
		`, orgID).Scan(&activeDevices); err != nil {
			return model.Device{}, err
		}
		if activeDevices >= quota {
			return model.Device{}, ErrEvaluationQuotaExceeded
		}
	}
	metadata, err := json.Marshal(defaultMetadata(in.Metadata))
	if err != nil {
		return model.Device{}, err
	}
	device, err := s.scanDevice(tx.QueryRow(ctx, `
		INSERT INTO devices (organization_id, name, category, serial_number, mac_address, manufacturer, model, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at
	`, orgID, in.Name, in.Category, in.SerialNumber, in.MACAddress, in.Manufacturer, in.Model, metadata))
	if err != nil {
		return model.Device{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Device{}, err
	}
	return device, nil
}

func (s *Store) ListDevices(ctx context.Context, orgID string, limit, offset int) (DevicePage, error) {
	total, err := s.countDevices(ctx, orgID)
	if err != nil {
		return DevicePage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at
		FROM devices
		WHERE organization_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, orgID, limit, offset)
	if err != nil {
		return DevicePage{}, err
	}
	defer rows.Close()

	devices := []model.Device{}
	for rows.Next() {
		device, err := scanDeviceRows(rows)
		if err != nil {
			return DevicePage{}, err
		}
		devices = append(devices, device)
	}
	if err := rows.Err(); err != nil {
		return DevicePage{}, err
	}
	return DevicePage{Devices: devices, Page: Page{Limit: limit, Offset: offset, Total: total}}, nil
}

func (s *Store) countOrganizations(ctx context.Context, userID string) (int, error) {
	var total int
	err := s.db.QueryRow(ctx, `
		SELECT count(*)
		FROM organizations o
		JOIN organization_members m ON m.organization_id = o.id
		JOIN users u ON u.id = m.user_id
		WHERE m.user_id = $1 AND u.disabled_at IS NULL
	`, userID).Scan(&total)
	return total, err
}

func (s *Store) countMembers(ctx context.Context, orgID string) (int, error) {
	var total int
	err := s.db.QueryRow(ctx, `
		SELECT count(*)
		FROM organization_members m
		JOIN users u ON u.id = m.user_id
		WHERE m.organization_id = $1
	`, orgID).Scan(&total)
	return total, err
}

func (s *Store) countDevices(ctx context.Context, orgID string) (int, error) {
	var total int
	err := s.db.QueryRow(ctx, `
		SELECT count(*) FROM devices WHERE organization_id = $1
	`, orgID).Scan(&total)
	return total, err
}

func (s *Store) countActiveDevices(ctx context.Context, orgID string) (int, error) {
	var total int
	err := s.db.QueryRow(ctx, `
		SELECT count(*)
		FROM devices
		WHERE organization_id = $1 AND disabled_at IS NULL
	`, orgID).Scan(&total)
	return total, err
}

func (s *Store) countDeviceGroups(ctx context.Context, orgID string) (int, error) {
	var total int
	err := s.db.QueryRow(ctx, `
		SELECT count(*) FROM device_groups WHERE organization_id = $1
	`, orgID).Scan(&total)
	return total, err
}

func (s *Store) countDeviceGroupDevices(ctx context.Context, orgID, groupID string) (int, error) {
	var total int
	err := s.db.QueryRow(ctx, `
		SELECT count(*) FROM device_group_members WHERE organization_id = $1 AND group_id = $2
	`, orgID, groupID).Scan(&total)
	return total, err
}

func (s *Store) countDeviceTags(ctx context.Context, orgID, deviceID string) (int, error) {
	var total int
	err := s.db.QueryRow(ctx, `
		SELECT count(*) FROM device_tags WHERE organization_id = $1 AND device_id = $2
	`, orgID, deviceID).Scan(&total)
	return total, err
}

func (s *Store) GetDevice(ctx context.Context, orgID, deviceID string) (model.Device, error) {
	device, err := s.scanDevice(s.db.QueryRow(ctx, `
		SELECT id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at
		FROM devices
		WHERE organization_id = $1 AND id = $2
	`, orgID, deviceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Device{}, ErrNotFound
	}
	return device, err
}

func (s *Store) UpdateDevice(ctx context.Context, orgID, deviceID string, in DeviceInput) (model.Device, error) {
	metadata, err := json.Marshal(defaultMetadata(in.Metadata))
	if err != nil {
		return model.Device{}, err
	}
	device, err := s.scanDevice(s.db.QueryRow(ctx, `
		UPDATE devices
		SET name = $3, category = $4, serial_number = $5, mac_address = $6, manufacturer = $7, model = $8, metadata = $9, updated_at = now()
		WHERE organization_id = $1 AND id = $2 AND disabled_at IS NULL
		RETURNING id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at
	`, orgID, deviceID, in.Name, in.Category, in.SerialNumber, in.MACAddress, in.Manufacturer, in.Model, metadata))
	if errors.Is(err, pgx.ErrNoRows) {
		if existing, getErr := s.GetDevice(ctx, orgID, deviceID); getErr == nil && existing.DisabledAt != nil {
			return model.Device{}, ErrDisabled
		}
		return model.Device{}, ErrNotFound
	}
	return device, err
}

func (s *Store) DeleteDevice(ctx context.Context, orgID, deviceID string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE devices
		SET status = 'disabled', disabled_at = COALESCE(disabled_at, now()), updated_at = now()
		WHERE organization_id = $1 AND id = $2
	`, orgID, deviceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UpdateDeviceStatus(ctx context.Context, orgID, deviceID string, status model.DeviceStatus, lastSeenAt *time.Time) (model.Device, error) {
	device, err := s.scanDevice(s.db.QueryRow(ctx, `
		UPDATE devices
		SET status = $3, last_seen_at = COALESCE($4, last_seen_at), disabled_at = CASE WHEN $3 = 'disabled' THEN now() ELSE disabled_at END, updated_at = now()
		WHERE organization_id = $1 AND id = $2 AND (disabled_at IS NULL OR status <> 'disabled')
		RETURNING id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at
	`, orgID, deviceID, status, lastSeenAt))
	if errors.Is(err, pgx.ErrNoRows) {
		if existing, getErr := s.GetDevice(ctx, orgID, deviceID); getErr == nil && existing.DisabledAt != nil {
			return model.Device{}, ErrDisabled
		}
		return model.Device{}, ErrNotFound
	}
	return device, err
}

func (s *Store) CreateDeviceGroup(ctx context.Context, orgID string, in DeviceGroupInput) (model.DeviceGroup, error) {
	group, err := scanDeviceGroup(s.db.QueryRow(ctx, `
		INSERT INTO device_groups (organization_id, name, description)
		VALUES ($1, $2, $3)
		RETURNING id::text, organization_id::text, name, description, created_at, updated_at, 0
	`, orgID, in.Name, in.Description))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceGroup{}, ErrNotFound
	}
	return group, err
}

func (s *Store) ListDeviceGroups(ctx context.Context, orgID string, limit, offset int) (DeviceGroupPage, error) {
	total, err := s.countDeviceGroups(ctx, orgID)
	if err != nil {
		return DeviceGroupPage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT g.id::text, g.organization_id::text, g.name, g.description, g.created_at, g.updated_at, count(m.device_id)::int
		FROM device_groups g
		LEFT JOIN device_group_members m ON m.organization_id = g.organization_id AND m.group_id = g.id
		WHERE g.organization_id = $1
		GROUP BY g.id
		ORDER BY g.name ASC
		LIMIT $2 OFFSET $3
	`, orgID, limit, offset)
	if err != nil {
		return DeviceGroupPage{}, err
	}
	defer rows.Close()

	groups := []model.DeviceGroup{}
	for rows.Next() {
		group, err := scanDeviceGroup(rows)
		if err != nil {
			return DeviceGroupPage{}, err
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return DeviceGroupPage{}, err
	}
	return DeviceGroupPage{Groups: groups, Page: Page{Limit: limit, Offset: offset, Total: total}}, nil
}

func (s *Store) GetDeviceGroup(ctx context.Context, orgID, groupID string) (model.DeviceGroup, error) {
	group, err := scanDeviceGroup(s.db.QueryRow(ctx, `
		SELECT g.id::text, g.organization_id::text, g.name, g.description, g.created_at, g.updated_at, count(m.device_id)::int
		FROM device_groups g
		LEFT JOIN device_group_members m ON m.organization_id = g.organization_id AND m.group_id = g.id
		WHERE g.organization_id = $1 AND g.id = $2
		GROUP BY g.id
	`, orgID, groupID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceGroup{}, ErrNotFound
	}
	return group, err
}

func (s *Store) UpdateDeviceGroup(ctx context.Context, orgID, groupID string, in DeviceGroupInput) (model.DeviceGroup, error) {
	group, err := scanDeviceGroup(s.db.QueryRow(ctx, `
		WITH updated AS (
			UPDATE device_groups
			SET name = $3, description = $4
			WHERE organization_id = $1 AND id = $2
			RETURNING id, organization_id, name, description, created_at, updated_at
		)
		SELECT u.id::text, u.organization_id::text, u.name, u.description, u.created_at, u.updated_at, count(m.device_id)::int
		FROM updated u
		LEFT JOIN device_group_members m ON m.organization_id = u.organization_id AND m.group_id = u.id
		GROUP BY u.id, u.organization_id, u.name, u.description, u.created_at, u.updated_at
	`, orgID, groupID, in.Name, in.Description))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceGroup{}, ErrNotFound
	}
	return group, err
}

func (s *Store) DeleteDeviceGroup(ctx context.Context, orgID, groupID string) error {
	tag, err := s.db.Exec(ctx, `
		DELETE FROM device_groups
		WHERE organization_id = $1 AND id = $2
	`, orgID, groupID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) AddDeviceToGroup(ctx context.Context, orgID, groupID, deviceID string) error {
	if _, err := s.GetDeviceGroup(ctx, orgID, groupID); err != nil {
		return err
	}
	if _, err := s.GetDevice(ctx, orgID, deviceID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO device_group_members (organization_id, group_id, device_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (group_id, device_id) DO NOTHING
	`, orgID, groupID, deviceID)
	return err
}

func (s *Store) RemoveDeviceFromGroup(ctx context.Context, orgID, groupID, deviceID string) error {
	tag, err := s.db.Exec(ctx, `
		DELETE FROM device_group_members
		WHERE organization_id = $1 AND group_id = $2 AND device_id = $3
	`, orgID, groupID, deviceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, groupErr := s.GetDeviceGroup(ctx, orgID, groupID); groupErr != nil {
			return groupErr
		}
		if _, deviceErr := s.GetDevice(ctx, orgID, deviceID); deviceErr != nil {
			return deviceErr
		}
	}
	return nil
}

func (s *Store) ListDeviceGroupDevices(ctx context.Context, orgID, groupID string, limit, offset int) (DevicePage, error) {
	if _, err := s.GetDeviceGroup(ctx, orgID, groupID); err != nil {
		return DevicePage{}, err
	}
	total, err := s.countDeviceGroupDevices(ctx, orgID, groupID)
	if err != nil {
		return DevicePage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT d.id::text, d.organization_id::text, d.name, d.category, d.serial_number, d.mac_address, d.manufacturer, d.model, d.status, d.last_seen_at, d.metadata, d.created_at, d.updated_at, d.disabled_at
		FROM device_group_members m
		JOIN devices d ON d.organization_id = m.organization_id AND d.id = m.device_id
		WHERE m.organization_id = $1 AND m.group_id = $2
		ORDER BY d.created_at DESC
		LIMIT $3 OFFSET $4
	`, orgID, groupID, limit, offset)
	if err != nil {
		return DevicePage{}, err
	}
	defer rows.Close()

	devices := []model.Device{}
	for rows.Next() {
		device, err := scanDeviceRows(rows)
		if err != nil {
			return DevicePage{}, err
		}
		devices = append(devices, device)
	}
	if err := rows.Err(); err != nil {
		return DevicePage{}, err
	}
	return DevicePage{Devices: devices, Page: Page{Limit: limit, Offset: offset, Total: total}}, nil
}

func (s *Store) AddDeviceTag(ctx context.Context, orgID, deviceID, tag string) (model.DeviceTag, error) {
	if _, err := s.GetDevice(ctx, orgID, deviceID); err != nil {
		return model.DeviceTag{}, err
	}
	return scanDeviceTag(s.db.QueryRow(ctx, `
		INSERT INTO device_tags (organization_id, device_id, tag)
		VALUES ($1, $2, $3)
		ON CONFLICT (organization_id, device_id, tag)
		DO UPDATE SET tag = EXCLUDED.tag
		RETURNING organization_id::text, device_id::text, tag, created_at, updated_at
	`, orgID, deviceID, tag))
}

func (s *Store) DeleteDeviceTag(ctx context.Context, orgID, deviceID, tag string) error {
	result, err := s.db.Exec(ctx, `
		DELETE FROM device_tags
		WHERE organization_id = $1 AND device_id = $2 AND tag = $3
	`, orgID, deviceID, tag)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		if _, deviceErr := s.GetDevice(ctx, orgID, deviceID); deviceErr != nil {
			return deviceErr
		}
	}
	return nil
}

func (s *Store) ListDeviceTags(ctx context.Context, orgID, deviceID string, limit, offset int) (DeviceTagPage, error) {
	if _, err := s.GetDevice(ctx, orgID, deviceID); err != nil {
		return DeviceTagPage{}, err
	}
	total, err := s.countDeviceTags(ctx, orgID, deviceID)
	if err != nil {
		return DeviceTagPage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT organization_id::text, device_id::text, tag, created_at, updated_at
		FROM device_tags
		WHERE organization_id = $1 AND device_id = $2
		ORDER BY tag ASC
		LIMIT $3 OFFSET $4
	`, orgID, deviceID, limit, offset)
	if err != nil {
		return DeviceTagPage{}, err
	}
	defer rows.Close()

	tags := []model.DeviceTag{}
	for rows.Next() {
		deviceTag, err := scanDeviceTag(rows)
		if err != nil {
			return DeviceTagPage{}, err
		}
		tags = append(tags, deviceTag)
	}
	if err := rows.Err(); err != nil {
		return DeviceTagPage{}, err
	}
	return DeviceTagPage{Tags: tags, Page: Page{Limit: limit, Offset: offset, Total: total}}, nil
}

func defaultMetadata(metadata map[string]any) map[string]any {
	if metadata == nil {
		return map[string]any{}
	}
	return metadata
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanDevice(row rowScanner) (model.Device, error) {
	return scanDevice(row)
}

func scanDeviceRows(rows pgx.Rows) (model.Device, error) {
	return scanDevice(rows)
}

func scanDevice(row rowScanner) (model.Device, error) {
	var device model.Device
	var metadata []byte
	err := row.Scan(
		&device.ID,
		&device.OrganizationID,
		&device.Name,
		&device.Category,
		&device.SerialNumber,
		&device.MACAddress,
		&device.Manufacturer,
		&device.Model,
		&device.Status,
		&device.LastSeenAt,
		&metadata,
		&device.CreatedAt,
		&device.UpdatedAt,
		&device.DisabledAt,
	)
	if err != nil {
		return model.Device{}, err
	}
	if len(metadata) == 0 {
		device.Metadata = map[string]any{}
		return device, nil
	}
	if err := json.Unmarshal(metadata, &device.Metadata); err != nil {
		return model.Device{}, err
	}
	return device, nil
}

func scanDeviceGroup(row rowScanner) (model.DeviceGroup, error) {
	var group model.DeviceGroup
	var deviceCount int
	err := row.Scan(
		&group.ID,
		&group.OrganizationID,
		&group.Name,
		&group.Description,
		&group.CreatedAt,
		&group.UpdatedAt,
		&deviceCount,
	)
	if err != nil {
		return model.DeviceGroup{}, err
	}
	group.DeviceCount = &deviceCount
	return group, nil
}

func scanDeviceTag(row rowScanner) (model.DeviceTag, error) {
	var tag model.DeviceTag
	err := row.Scan(
		&tag.OrganizationID,
		&tag.DeviceID,
		&tag.Tag,
		&tag.CreatedAt,
		&tag.UpdatedAt,
	)
	if err != nil {
		return model.DeviceTag{}, err
	}
	return tag, nil
}
