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

type DeviceClaimTokenCreateInput struct {
	OrganizationID  *string
	TokenHash       string
	Category        model.DeviceCategory
	VideoCloudDevid string
	ActivityID      string
	ClipPublicKey   string
	Metadata        map[string]any
	ExpiresAt       time.Time
	Now             time.Time
}

type DeviceClaimResolveInput struct {
	TokenHash      string
	OrganizationID string
	RequestedBy    string
	DeviceName     string
	Now            time.Time
}

type DeviceProvisionInput struct {
	VideoCloudDevid string `json:"video_cloud_devid"`
	ActivityID      string `json:"activity_id"`
	ClipPublicKey   string `json:"clip_public_key"`
}

type DeviceClaimResolveResult struct {
	Claim          model.DeviceClaim    `json:"claim"`
	Device         model.Device         `json:"device"`
	ProvisionInput DeviceProvisionInput `json:"provision_input"`
}

func (s *Store) CreateDeviceClaimToken(ctx context.Context, in DeviceClaimTokenCreateInput) (model.DeviceClaimToken, error) {
	metadata, err := json.Marshal(defaultMetadata(in.Metadata))
	if err != nil {
		return model.DeviceClaimToken{}, err
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	token, err := scanDeviceClaimToken(s.db.QueryRow(ctx, `
		INSERT INTO device_claim_tokens (
			organization_id,
			token_hash,
			category,
			video_cloud_devid,
			activity_id,
			clip_public_key,
			metadata,
			expires_at,
			created_at,
			updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)
		RETURNING id::text, organization_id::text, category, video_cloud_devid, activity_id, clip_public_key, metadata, expires_at, claimed_at, created_at, updated_at
	`, in.OrganizationID, in.TokenHash, in.Category, in.VideoCloudDevid, in.ActivityID, in.ClipPublicKey, metadata, in.ExpiresAt, now))
	if err != nil {
		return model.DeviceClaimToken{}, err
	}
	return token, nil
}

func (s *Store) ResolveDeviceClaimToken(ctx context.Context, in DeviceClaimResolveInput) (DeviceClaimResolveResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return DeviceClaimResolveResult{}, err
	}
	defer tx.Rollback(ctx)

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	token, err := scanDeviceClaimToken(tx.QueryRow(ctx, `
		SELECT id::text, organization_id::text, category, video_cloud_devid, activity_id, clip_public_key, metadata, expires_at, claimed_at, created_at, updated_at
		FROM device_claim_tokens
		WHERE token_hash = $1
		FOR UPDATE
	`, in.TokenHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceClaimResolveResult{}, ErrNotFound
	}
	if err != nil {
		return DeviceClaimResolveResult{}, err
	}
	if token.ClaimedAt != nil {
		return DeviceClaimResolveResult{}, ErrClaimAlreadyClaimed
	}
	if !token.ExpiresAt.After(now) {
		return DeviceClaimResolveResult{}, ErrClaimExpired
	}
	if token.OrganizationID != nil && *token.OrganizationID != in.OrganizationID {
		return DeviceClaimResolveResult{}, ErrClaimCrossOrganization
	}
	if token.Category != model.DeviceCategoryIPCamera {
		return DeviceClaimResolveResult{}, ErrClaimUnsupportedCategory
	}

	if err := lockOrganizationAndCheckQuota(ctx, tx, in.OrganizationID); err != nil {
		return DeviceClaimResolveResult{}, err
	}

	deviceName := strings.TrimSpace(in.DeviceName)
	if deviceName == "" {
		deviceName = "Claimed Device"
	}
	device, err := getClaimedDeviceByVideoDevidTx(ctx, tx, in.OrganizationID, token.VideoCloudDevid)
	if errors.Is(err, ErrNotFound) {
		device, err = createClaimedDeviceTx(ctx, tx, in.OrganizationID, deviceName, token)
	}
	if err != nil {
		return DeviceClaimResolveResult{}, err
	}

	provisionInput := map[string]any{
		"video_cloud_devid": token.VideoCloudDevid,
		"activity_id":       token.ActivityID,
		"clip_public_key":   token.ClipPublicKey,
	}
	provisionInputJSON, err := json.Marshal(provisionInput)
	if err != nil {
		return DeviceClaimResolveResult{}, err
	}

	claim, err := scanDeviceClaim(tx.QueryRow(ctx, `
		INSERT INTO device_claims (
			claim_token_id,
			organization_id,
			device_id,
			claimed_by,
			status,
			provision_input,
			created_at,
			updated_at
		)
		VALUES ($1, $2, $3, $4, 'resolved', $5, $6, $6)
		RETURNING id::text, claim_token_id::text, organization_id::text, device_id::text, claimed_by::text, status, provision_input, created_at, updated_at
	`, token.ID, in.OrganizationID, device.ID, in.RequestedBy, provisionInputJSON, now))
	if err != nil {
		return DeviceClaimResolveResult{}, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE device_claim_tokens
		SET claimed_at = $2, updated_at = $2
		WHERE id = $1
	`, token.ID, now); err != nil {
		return DeviceClaimResolveResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return DeviceClaimResolveResult{}, err
	}

	return DeviceClaimResolveResult{
		Claim:  claim,
		Device: device,
		ProvisionInput: DeviceProvisionInput{
			VideoCloudDevid: token.VideoCloudDevid,
			ActivityID:      token.ActivityID,
			ClipPublicKey:   token.ClipPublicKey,
		},
	}, nil
}

func lockOrganizationAndCheckQuota(ctx context.Context, tx pgx.Tx, orgID string) error {
	var tier model.OrganizationTier
	var quota int
	err := tx.QueryRow(ctx, `
		SELECT tier, evaluation_device_quota
		FROM organizations
		WHERE id = $1
		FOR UPDATE
	`, orgID).Scan(&tier, &quota)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if tier != model.OrganizationTierEvaluation {
		return nil
	}
	var activeDevices int
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM devices
		WHERE organization_id = $1 AND disabled_at IS NULL
	`, orgID).Scan(&activeDevices); err != nil {
		return err
	}
	if activeDevices >= quota {
		return ErrEvaluationQuotaExceeded
	}
	return nil
}

func getClaimedDeviceByVideoDevidTx(ctx context.Context, tx pgx.Tx, orgID, videoCloudDevid string) (model.Device, error) {
	device, err := scanDevice(tx.QueryRow(ctx, `
		SELECT id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at
		FROM devices
		WHERE organization_id = $1
			AND disabled_at IS NULL
			AND metadata->>$2 = $3
		ORDER BY created_at ASC
		LIMIT 1
	`, orgID, model.DeviceMetadataVideoCloudDevid, videoCloudDevid))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Device{}, ErrNotFound
	}
	return device, err
}

func createClaimedDeviceTx(ctx context.Context, tx pgx.Tx, orgID, name string, token model.DeviceClaimToken) (model.Device, error) {
	metadata := map[string]any{
		model.DeviceMetadataVideoCloudDevid:         token.VideoCloudDevid,
		model.DeviceMetadataVideoCloudActivityID:    token.ActivityID,
		model.DeviceMetadataVideoCloudClipPublicKey: token.ClipPublicKey,
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return model.Device{}, err
	}
	return scanDevice(tx.QueryRow(ctx, `
		INSERT INTO devices (organization_id, name, category, metadata)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at
	`, orgID, name, token.Category, metadataJSON))
}

func scanDeviceClaimToken(row rowScanner) (model.DeviceClaimToken, error) {
	var token model.DeviceClaimToken
	var metadata []byte
	err := row.Scan(
		&token.ID,
		&token.OrganizationID,
		&token.Category,
		&token.VideoCloudDevid,
		&token.ActivityID,
		&token.ClipPublicKey,
		&metadata,
		&token.ExpiresAt,
		&token.ClaimedAt,
		&token.CreatedAt,
		&token.UpdatedAt,
	)
	if err != nil {
		return model.DeviceClaimToken{}, err
	}
	token.Metadata, err = unmarshalJSONMap(metadata)
	if err != nil {
		return model.DeviceClaimToken{}, err
	}
	return token, nil
}

func scanDeviceClaim(row rowScanner) (model.DeviceClaim, error) {
	var claim model.DeviceClaim
	var provisionInput []byte
	err := row.Scan(
		&claim.ID,
		&claim.TokenID,
		&claim.OrganizationID,
		&claim.DeviceID,
		&claim.ClaimedBy,
		&claim.Status,
		&provisionInput,
		&claim.CreatedAt,
		&claim.UpdatedAt,
	)
	if err != nil {
		return model.DeviceClaim{}, err
	}
	claim.ProvisionInput, err = unmarshalJSONMap(provisionInput)
	if err != nil {
		return model.DeviceClaim{}, err
	}
	return claim, nil
}
