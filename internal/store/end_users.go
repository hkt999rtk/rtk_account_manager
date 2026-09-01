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

type EndUserCreateInput struct {
	Email        string
	PasswordHash string
	DisplayName  *string
}

type EndUserLoginResult struct {
	EndUser      model.EndUser `json:"end_user"`
	PasswordHash string        `json:"-"`
}

type EndUserDeviceClaimResolveInput struct {
	TokenHash  string
	EndUserID  string
	DeviceName string
	Now        time.Time
}

type EndUserDeviceClaimResolveResult struct {
	Claim          model.DeviceClaim       `json:"claim"`
	Device         model.Device            `json:"device"`
	BrandLink      model.BrandCloudEndUser `json:"brand_cloud_end_user"`
	DeviceBinding  model.DeviceUserBinding `json:"device_user_binding"`
	ProvisionInput DeviceProvisionInput    `json:"provision_input"`
}

func (s *Store) CreateEndUser(ctx context.Context, in EndUserCreateInput) (model.EndUser, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	user, err := scanEndUser(s.db.QueryRow(ctx, `
		INSERT INTO end_users (primary_email, password_hash, display_name, status)
		VALUES ($1, $2, $3, 'active')
		RETURNING id::text, primary_email, display_name, status, created_at, updated_at, disabled_at
	`, email, in.PasswordHash, in.DisplayName))
	if err != nil {
		if isUniqueViolation(err) {
			return model.EndUser{}, ErrConflict
		}
		return model.EndUser{}, err
	}
	if _, err := s.db.Exec(ctx, `
		INSERT INTO end_user_identities (end_user_id, identity_provider, provider_subject, email)
		VALUES ($1, 'email', $2, $2)
		ON CONFLICT (identity_provider, provider_subject) DO NOTHING
	`, user.ID, user.PrimaryEmail); err != nil {
		return model.EndUser{}, err
	}
	return user, nil
}

func (s *Store) GetEndUserPassword(ctx context.Context, email string) (EndUserLoginResult, error) {
	var result EndUserLoginResult
	err := s.db.QueryRow(ctx, `
		SELECT id::text, primary_email, display_name, status, created_at, updated_at, disabled_at, password_hash
		FROM end_users
		WHERE primary_email = $1
		  AND disabled_at IS NULL
	`, strings.ToLower(strings.TrimSpace(email))).Scan(
		&result.EndUser.ID,
		&result.EndUser.PrimaryEmail,
		&result.EndUser.DisplayName,
		&result.EndUser.Status,
		&result.EndUser.CreatedAt,
		&result.EndUser.UpdatedAt,
		&result.EndUser.DisabledAt,
		&result.PasswordHash,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return EndUserLoginResult{}, ErrNotFound
	}
	return result, err
}

func (s *Store) GetEndUser(ctx context.Context, endUserID string) (model.EndUser, error) {
	user, err := scanEndUser(s.db.QueryRow(ctx, `
		SELECT id::text, primary_email, display_name, status, created_at, updated_at, disabled_at
		FROM end_users
		WHERE id = $1 AND disabled_at IS NULL
	`, endUserID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.EndUser{}, ErrNotFound
	}
	return user, err
}

func (s *Store) GetBrandCloudEndUser(ctx context.Context, brandCloudID, endUserID string) (model.BrandCloudEndUser, error) {
	var link model.BrandCloudEndUser
	err := s.db.QueryRow(ctx, `
		SELECT brand_cloud_id::text, end_user_id::text, display_alias, status, consent,
		       first_seen_at, last_seen_at, created_at, updated_at
		FROM brand_cloud_end_users
		WHERE brand_cloud_id = $1 AND end_user_id = $2 AND status = 'active'
	`, brandCloudID, endUserID).Scan(&link.BrandCloudID, &link.EndUserID, &link.DisplayAlias, &link.Status, &link.Consent, &link.FirstSeenAt, &link.LastSeenAt, &link.CreatedAt, &link.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.BrandCloudEndUser{}, ErrNotFound
	}
	return link, err
}

func (s *Store) SaveEndUserRefreshToken(ctx context.Context, endUserID, tokenHash string, expiresAt time.Time) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO end_user_refresh_tokens (end_user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
	`, endUserID, tokenHash, expiresAt)
	return err
}

func (s *Store) RotateEndUserRefreshToken(ctx context.Context, oldTokenHash, newTokenHash, endUserID string, newExpiresAt time.Time) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var activeEndUserID string
	err = tx.QueryRow(ctx, `
		SELECT rt.end_user_id::text
		FROM end_user_refresh_tokens rt
		JOIN end_users eu ON eu.id = rt.end_user_id
		WHERE rt.token_hash = $1
		  AND rt.revoked_at IS NULL
		  AND rt.expires_at > now()
		  AND eu.disabled_at IS NULL
		FOR UPDATE
	`, oldTokenHash).Scan(&activeEndUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if activeEndUserID != endUserID {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `
		UPDATE end_user_refresh_tokens
		SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL
	`, oldTokenHash); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO end_user_refresh_tokens (end_user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
	`, endUserID, newTokenHash, newExpiresAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) RevokeEndUserRefreshToken(ctx context.Context, tokenHash string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE end_user_refresh_tokens
		SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL
	`, tokenHash)
	return err
}

func (s *Store) ResolveEndUserDeviceClaimToken(ctx context.Context, in EndUserDeviceClaimResolveInput) (EndUserDeviceClaimResolveResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return EndUserDeviceClaimResolveResult{}, err
	}
	defer tx.Rollback(ctx)
	var actor string
	err = tx.QueryRow(ctx, `SELECT id::text FROM end_users WHERE id::text=$1 AND disabled_at IS NULL AND status='active' FOR UPDATE`, in.EndUserID).Scan(&actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return EndUserDeviceClaimResolveResult{}, ErrNotFound
	}
	if err != nil {
		return EndUserDeviceClaimResolveResult{}, err
	}
	var brandCloudID string
	err = tx.QueryRow(ctx, `
		SELECT organization_id::text
		FROM device_claim_tokens
		WHERE token_hash = $1
	`, in.TokenHash).Scan(&brandCloudID)
	if errors.Is(err, pgx.ErrNoRows) {
		return EndUserDeviceClaimResolveResult{}, ErrNotFound
	}
	if err != nil {
		return EndUserDeviceClaimResolveResult{}, err
	}
	if strings.TrimSpace(brandCloudID) == "" {
		return EndUserDeviceClaimResolveResult{}, ErrNotFound
	}
	if err := lockOperationalCloudTx(ctx, tx, brandCloudID); err != nil {
		return EndUserDeviceClaimResolveResult{}, err
	}
	token, err := getClaimTokenByHashForUpdateTx(ctx, tx, in.TokenHash)
	if err != nil {
		return EndUserDeviceClaimResolveResult{}, err
	}
	result, err := resolveLockedDeviceClaimTokenTx(ctx, tx, DeviceClaimResolveInput{
		TokenHash:      in.TokenHash,
		OrganizationID: brandCloudID,
		RequestedBy:    in.EndUserID,
		DeviceName:     in.DeviceName,
		Now:            in.Now,
	}, token)
	if err != nil {
		return EndUserDeviceClaimResolveResult{}, err
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	link, err := scanBrandCloudEndUser(tx.QueryRow(ctx, `
		INSERT INTO brand_cloud_end_users (brand_cloud_id, end_user_id, status, last_seen_at)
		VALUES ($1, $2, 'active', $3)
		ON CONFLICT ON CONSTRAINT brand_cloud_end_users_key
		DO UPDATE SET status = 'active', last_seen_at = EXCLUDED.last_seen_at, updated_at = now()
		RETURNING brand_cloud_id::text, end_user_id::text, display_alias, status, consent, first_seen_at, last_seen_at, created_at, updated_at
	`, brandCloudID, in.EndUserID, now))
	if err != nil {
		return EndUserDeviceClaimResolveResult{}, err
	}
	binding, err := scanDeviceUserBinding(tx.QueryRow(ctx, `
		INSERT INTO device_user_bindings (device_id, brand_cloud_id, end_user_id, role, created_from_claim_id)
		VALUES ($1, $2, $3, 'owner', $4)
		ON CONFLICT ON CONSTRAINT device_user_bindings_device_end_user_key
		DO UPDATE SET disabled_at = NULL, updated_at = now()
		RETURNING id::text, device_id::text, brand_cloud_id::text, end_user_id::text, role, created_from_claim_id::text, created_at, updated_at, disabled_at
	`, result.Device.ID, brandCloudID, in.EndUserID, result.Claim.ID))
	if err != nil {
		return EndUserDeviceClaimResolveResult{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType: "app_device_claim_resolved", OrganizationID: &brandCloudID, SubjectType: "device_claim", SubjectID: result.Claim.ID,
		Payload: map[string]any{"end_user_id": in.EndUserID, "device_id": result.Device.ID},
	}); err != nil {
		return EndUserDeviceClaimResolveResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return EndUserDeviceClaimResolveResult{}, err
	}
	return EndUserDeviceClaimResolveResult{
		Claim:          result.Claim,
		Device:         result.Device,
		BrandLink:      link,
		DeviceBinding:  binding,
		ProvisionInput: result.ProvisionInput,
	}, nil
}

func (s *Store) AuthorizeEndUserForVideoDevice(ctx context.Context, endUserID, videoCloudDevid string) error {
	var ok bool
	err := s.db.QueryRow(ctx, `
		SELECT true
		FROM device_user_bindings b
		JOIN devices d ON d.id = b.device_id
		WHERE b.end_user_id = $1
		  AND b.disabled_at IS NULL
		  AND d.disabled_at IS NULL
		  AND d.metadata ->> $2 = $3
		LIMIT 1
	`, endUserID, model.DeviceMetadataVideoCloudDevid, videoCloudDevid).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func scanEndUser(row scanner) (model.EndUser, error) {
	var user model.EndUser
	err := row.Scan(&user.ID, &user.PrimaryEmail, &user.DisplayName, &user.Status, &user.CreatedAt, &user.UpdatedAt, &user.DisabledAt)
	return user, err
}

func scanBrandCloudEndUser(row scanner) (model.BrandCloudEndUser, error) {
	var link model.BrandCloudEndUser
	var rawConsent []byte
	err := row.Scan(&link.BrandCloudID, &link.EndUserID, &link.DisplayAlias, &link.Status, &rawConsent, &link.FirstSeenAt, &link.LastSeenAt, &link.CreatedAt, &link.UpdatedAt)
	if err != nil {
		return model.BrandCloudEndUser{}, err
	}
	if len(rawConsent) == 0 {
		link.Consent = map[string]any{}
		return link, nil
	}
	if err := json.Unmarshal(rawConsent, &link.Consent); err != nil {
		return model.BrandCloudEndUser{}, err
	}
	return link, nil
}

func scanDeviceUserBinding(row scanner) (model.DeviceUserBinding, error) {
	var binding model.DeviceUserBinding
	err := row.Scan(&binding.ID, &binding.DeviceID, &binding.BrandCloudID, &binding.EndUserID, &binding.Role, &binding.CreatedFromClaimID, &binding.CreatedAt, &binding.UpdatedAt, &binding.DisabledAt)
	return binding, err
}
