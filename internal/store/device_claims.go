package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/model"
)

type DeviceClaimTokenCreateInput struct {
	OrganizationID      *string
	CreatedBy           *string
	DeviceItemProfileID *string
	TokenHash           string
	Category            model.DeviceCategory
	VideoCloudDevid     string
	ActivityID          string
	ClipPublicKey       string
	ServiceOptions      []string
	Metadata            map[string]any
	Notes               *string
	ExpiresAt           time.Time
	Now                 time.Time
}

type DeviceClaimTokenListFilter struct {
	Limit  int
	Offset int
}

type DeviceClaimResolveInput struct {
	TokenHash      string
	OrganizationID string
	RequestedBy    string
	DeviceName     string
	Now            time.Time
}

type DeviceProvisionInput struct {
	VideoCloudDevid string   `json:"video_cloud_devid"`
	ActivityID      string   `json:"activity_id"`
	ClipPublicKey   string   `json:"clip_public_key"`
	ServiceOptions  []string `json:"service_options"`
}

type DeviceClaimResolveResult struct {
	Claim          model.DeviceClaim    `json:"claim"`
	Device         model.Device         `json:"device"`
	ProvisionInput DeviceProvisionInput `json:"provision_input"`
}

type DeviceClaimTransferInput struct {
	ClaimID              string
	TargetOrganizationID string
	ActorUserID          string
	Reason               string
	Evidence             map[string]any
	Now                  time.Time
}

type DeviceClaimReclaimInput struct {
	TokenID              string
	TargetOrganizationID string
	ActorUserID          string
	Reason               string
	Evidence             map[string]any
	Now                  time.Time
}

type DeviceClaimOverrideResult struct {
	Claim  model.DeviceClaim      `json:"claim"`
	Token  model.DeviceClaimToken `json:"device_claim_token"`
	Device model.Device           `json:"device"`
}

// Low-level bootstrap/fixture persistence. Human HTTP callers must use the
// separately authorized CreateDeviceClaimTokenAsPlatform entrypoint.
func (s *Store) CreateDeviceClaimToken(ctx context.Context, in DeviceClaimTokenCreateInput) (model.DeviceClaimToken, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.DeviceClaimToken{}, err
	}
	defer tx.Rollback(ctx)
	token, err := createDeviceClaimTokenTx(ctx, tx, in)
	if err != nil {
		return model.DeviceClaimToken{}, err
	}
	return token, tx.Commit(ctx)
}

func createDeviceClaimTokenTx(ctx context.Context, tx pgx.Tx, in DeviceClaimTokenCreateInput) (model.DeviceClaimToken, error) {
	metadataValue := defaultMetadata(in.Metadata)
	category := in.Category
	serviceOptionValues := in.ServiceOptions
	if in.DeviceItemProfileID != nil && strings.TrimSpace(*in.DeviceItemProfileID) != "" {
		profile, err := getDeviceItemProfileByID(ctx, tx, *in.DeviceItemProfileID)
		if err != nil {
			return model.DeviceClaimToken{}, err
		}
		if profile.Status == model.DeviceItemProfileStatusDisabled {
			return model.DeviceClaimToken{}, ErrDeviceItemProfileDisabled
		}
		if category == "" {
			category = profile.Category
		}
		if category != profile.Category {
			return model.DeviceClaimToken{}, ErrConflict
		}
		if serviceOptionValues == nil || len(serviceOptionValues) == 0 {
			serviceOptionValues = profile.ServiceOptions
		} else if err := validateClaimServiceOptions(serviceOptionValues); err != nil {
			return model.DeviceClaimToken{}, err
		} else if !serviceOptionSetsEqual(serviceOptionValues, profile.ServiceOptions) {
			return model.DeviceClaimToken{}, ErrClaimServiceOptionsMismatch
		}
		metadataValue["device_item_profile_id"] = profile.ID
		metadataValue["profile_key"] = profile.ProfileKey
		metadataValue["ca_profile"] = profile.CAProfile
		metadataValue["issuer_profile"] = profile.IssuerProfile
		if profile.Manufacturer != nil {
			metadataValue["manufacturer"] = *profile.Manufacturer
		}
		if profile.Model != nil {
			metadataValue["model"] = *profile.Model
		}
		metadataValue["metadata_defaults"] = profile.MetadataDefaults
	}
	metadata, err := json.Marshal(metadataValue)
	if err != nil {
		return model.DeviceClaimToken{}, err
	}
	if serviceOptionValues == nil {
		serviceOptionValues = []string{}
	}
	if err := validateClaimServiceOptions(serviceOptionValues); err != nil {
		return model.DeviceClaimToken{}, err
	}
	serviceOptions, err := json.Marshal(serviceOptionValues)
	if err != nil {
		return model.DeviceClaimToken{}, err
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	token, err := scanDeviceClaimToken(tx.QueryRow(ctx, `
		INSERT INTO device_claim_tokens (
			organization_id,
			created_by,
			device_item_profile_id,
			token_hash,
			category,
			video_cloud_devid,
			activity_id,
			clip_public_key,
			service_options,
			metadata,
			notes,
			expires_at,
			created_at,
			updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13)
		RETURNING id::text, organization_id::text, created_by::text, device_item_profile_id::text, category, video_cloud_devid, activity_id, clip_public_key, service_options, metadata, notes, expires_at, claimed_at, revoked_at, created_at, updated_at
	`, in.OrganizationID, in.CreatedBy, in.DeviceItemProfileID, in.TokenHash, category, in.VideoCloudDevid, in.ActivityID, in.ClipPublicKey, serviceOptions, metadata, in.Notes, in.ExpiresAt, now))
	if err != nil {
		return model.DeviceClaimToken{}, err
	}
	return token, nil
}

func (s *Store) ListDeviceClaimTokens(ctx context.Context, in DeviceClaimTokenListFilter) (DeviceClaimTokenPage, error) {
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*)::int FROM device_claim_tokens`).Scan(&total); err != nil {
		return DeviceClaimTokenPage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT id::text, organization_id::text, created_by::text, device_item_profile_id::text, category, video_cloud_devid, activity_id, clip_public_key, service_options, metadata, notes, expires_at, claimed_at, revoked_at, created_at, updated_at
		FROM device_claim_tokens
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2
	`, in.Limit, in.Offset)
	if err != nil {
		return DeviceClaimTokenPage{}, err
	}
	defer rows.Close()

	tokens := []model.DeviceClaimToken{}
	for rows.Next() {
		token, err := scanDeviceClaimToken(rows)
		if err != nil {
			return DeviceClaimTokenPage{}, err
		}
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		return DeviceClaimTokenPage{}, err
	}
	return DeviceClaimTokenPage{Tokens: tokens, Page: Page{Limit: in.Limit, Offset: in.Offset, Total: total}}, nil
}

func (s *Store) GetDeviceClaimToken(ctx context.Context, tokenID string) (model.DeviceClaimToken, error) {
	token, err := scanDeviceClaimToken(s.db.QueryRow(ctx, `
		SELECT id::text, organization_id::text, created_by::text, device_item_profile_id::text, category, video_cloud_devid, activity_id, clip_public_key, service_options, metadata, notes, expires_at, claimed_at, revoked_at, created_at, updated_at
		FROM device_claim_tokens
		WHERE id = $1
	`, tokenID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceClaimToken{}, ErrNotFound
	}
	return token, err
}

// Low-level bootstrap/fixture persistence, not a human request authorization path.
func (s *Store) RevokeDeviceClaimToken(ctx context.Context, tokenID string, now time.Time) (model.DeviceClaimToken, error) {
	return revokeDeviceClaimToken(ctx, s.db, tokenID, now)
}

func revokeDeviceClaimToken(ctx context.Context, q handoffQuerier, tokenID string, now time.Time) (model.DeviceClaimToken, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	token, err := scanDeviceClaimToken(q.QueryRow(ctx, `
		UPDATE device_claim_tokens
		SET revoked_at = COALESCE(revoked_at, $2), updated_at = $2
		WHERE id = $1
		RETURNING id::text, organization_id::text, created_by::text, device_item_profile_id::text, category, video_cloud_devid, activity_id, clip_public_key, service_options, metadata, notes, expires_at, claimed_at, revoked_at, created_at, updated_at
	`, tokenID, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceClaimToken{}, ErrNotFound
	}
	return token, err
}

func (s *Store) ResolveDeviceClaimToken(ctx context.Context, in DeviceClaimResolveInput) (DeviceClaimResolveResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return DeviceClaimResolveResult{}, err
	}
	defer tx.Rollback(ctx)
	if err := authorizeDeviceUserMutationTx(ctx, tx, in.RequestedBy, in.OrganizationID, "", "claim.resolve"); err != nil {
		return DeviceClaimResolveResult{}, err
	}

	token, err := getClaimTokenByHashForUpdateTx(ctx, tx, in.TokenHash)
	if err != nil {
		return DeviceClaimResolveResult{}, err
	}
	var productAllowed bool
	if err := tx.QueryRow(ctx, `SELECT user_can_access_brand_cloud_product($1,$2,$3)`, in.RequestedBy, in.OrganizationID, token.DeviceItemProfileID).Scan(&productAllowed); err != nil {
		return DeviceClaimResolveResult{}, err
	}
	if !productAllowed {
		return DeviceClaimResolveResult{}, ErrNotFound
	}
	result, err := resolveLockedDeviceClaimTokenTx(ctx, tx, in, token)
	if err != nil {
		return DeviceClaimResolveResult{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType: "device_claim_resolved", ActorUserID: &in.RequestedBy,
		OrganizationID: &in.OrganizationID, SubjectType: "device_claim", SubjectID: result.Claim.ID,
		Payload: map[string]any{"device_id": result.Device.ID},
	}); err != nil {
		return DeviceClaimResolveResult{}, err
	}
	return result, tx.Commit(ctx)
}

func getClaimTokenByHashForUpdateTx(ctx context.Context, tx pgx.Tx, hash string) (model.DeviceClaimToken, error) {
	token, err := scanDeviceClaimToken(tx.QueryRow(ctx, `
		SELECT id::text, organization_id::text, created_by::text, device_item_profile_id::text, category, video_cloud_devid, activity_id, clip_public_key, service_options, metadata, notes, expires_at, claimed_at, revoked_at, created_at, updated_at
		FROM device_claim_tokens
		WHERE token_hash = $1
		FOR UPDATE
	`, hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceClaimToken{}, ErrNotFound
	}
	return token, err
}

// Callers authenticate their distinct human/end-user identity and lock its cloud
// before the token. They commit the claim with their own audit/binding writes.
func resolveLockedDeviceClaimTokenTx(ctx context.Context, tx pgx.Tx, in DeviceClaimResolveInput, token model.DeviceClaimToken) (DeviceClaimResolveResult, error) {
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if token.ClaimedAt != nil {
		return DeviceClaimResolveResult{}, ErrClaimAlreadyClaimed
	}
	if token.RevokedAt != nil {
		return DeviceClaimResolveResult{}, ErrClaimRevoked
	}
	if !token.ExpiresAt.After(now) {
		return DeviceClaimResolveResult{}, ErrClaimExpired
	}
	if token.OrganizationID != nil && *token.OrganizationID != in.OrganizationID {
		return DeviceClaimResolveResult{}, ErrClaimCrossOrganization
	}
	if token.DeviceItemProfileID != nil {
		var sameCloud bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organizations o WHERE o.id::text=$2 AND
			(o.organization_kind<>'brand_cloud' OR EXISTS(SELECT 1 FROM device_item_profiles p WHERE p.id::text=$1 AND p.brand_cloud_id=o.id)))`, *token.DeviceItemProfileID, in.OrganizationID).Scan(&sameCloud); err != nil {
			return DeviceClaimResolveResult{}, err
		}
		if !sameCloud {
			return DeviceClaimResolveResult{}, ErrNotFound
		}
	}
	if token.Category != model.DeviceCategoryIPCamera &&
		token.Category != model.DeviceCategoryMQTT &&
		token.Category != model.DeviceCategoryGeneric {
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
	if err == nil {
		return DeviceClaimResolveResult{}, ErrClaimAlreadyClaimed
	}
	if !errors.Is(err, ErrNotFound) {
		return DeviceClaimResolveResult{}, err
	}
	device, err = createClaimedDeviceTx(ctx, tx, in.OrganizationID, deviceName, token)
	if err != nil {
		return DeviceClaimResolveResult{}, err
	}

	provisionInput := map[string]any{
		"video_cloud_devid": token.VideoCloudDevid,
		"activity_id":       token.ActivityID,
		"clip_public_key":   token.ClipPublicKey,
		"service_options":   token.ServiceOptions,
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
	return DeviceClaimResolveResult{
		Claim:  claim,
		Device: device,
		ProvisionInput: DeviceProvisionInput{
			VideoCloudDevid: token.VideoCloudDevid,
			ActivityID:      token.ActivityID,
			ClipPublicKey:   token.ClipPublicKey,
			ServiceOptions:  token.ServiceOptions,
		},
	}, nil
}

func (s *Store) TransferDeviceClaim(ctx context.Context, in DeviceClaimTransferInput) (DeviceClaimOverrideResult, error) {
	if err := validateClaimOverrideInput(in.TargetOrganizationID, in.ActorUserID, in.Reason, in.Evidence); err != nil {
		return DeviceClaimOverrideResult{}, err
	}
	return s.overrideDeviceClaim(ctx, claimOverrideInput{
		ClaimID:              in.ClaimID,
		TargetOrganizationID: in.TargetOrganizationID,
		ActorUserID:          in.ActorUserID,
		Reason:               strings.TrimSpace(in.Reason),
		Evidence:             in.Evidence,
		Status:               "transferred",
		EventType:            "device_claim_transferred",
		Now:                  in.Now,
	})
}

func (s *Store) ReclaimDeviceClaimToken(ctx context.Context, in DeviceClaimReclaimInput) (DeviceClaimOverrideResult, error) {
	if err := validateClaimOverrideInput(in.TargetOrganizationID, in.ActorUserID, in.Reason, in.Evidence); err != nil {
		return DeviceClaimOverrideResult{}, err
	}
	return s.overrideDeviceClaim(ctx, claimOverrideInput{
		TokenID:              in.TokenID,
		TargetOrganizationID: in.TargetOrganizationID,
		ActorUserID:          in.ActorUserID,
		Reason:               strings.TrimSpace(in.Reason),
		Evidence:             in.Evidence,
		Status:               "reclaimed",
		EventType:            "device_claim_reclaimed",
		Now:                  in.Now,
	})
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

type claimOverrideInput struct {
	ClaimID              string
	TokenID              string
	TargetOrganizationID string
	ActorUserID          string
	Reason               string
	Evidence             map[string]any
	Status               string
	EventType            string
	Now                  time.Time
}

func validateClaimOverrideInput(targetOrgID, actorUserID, reason string, evidence map[string]any) error {
	if strings.TrimSpace(targetOrgID) == "" || strings.TrimSpace(actorUserID) == "" {
		return ErrNotFound
	}
	if strings.TrimSpace(reason) == "" || len(evidence) == 0 {
		return ErrClaimEvidenceRequired
	}
	return nil
}

func (s *Store) overrideDeviceClaim(ctx context.Context, in claimOverrideInput) (DeviceClaimOverrideResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return DeviceClaimOverrideResult{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockPlatformActorTx(ctx, tx, in.ActorUserID); err != nil {
		return DeviceClaimOverrideResult{}, err
	}
	observed, err := getClaimForOverrideTx(ctx, tx, in.ClaimID, in.TokenID, false)
	if err != nil {
		return DeviceClaimOverrideResult{}, err
	}
	clouds := []string{observed.OrganizationID, in.TargetOrganizationID}
	slices.Sort(clouds)
	for i, cloud := range clouds {
		if i > 0 && cloud == clouds[i-1] {
			continue
		}
		if err := lockOperationalCloudTx(ctx, tx, cloud); err != nil {
			return DeviceClaimOverrideResult{}, err
		}
	}
	device, err := getDeviceForUpdateTx(ctx, tx, observed.OrganizationID, observed.DeviceID)
	if err != nil {
		return DeviceClaimOverrideResult{}, err
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	claim, err := getClaimForOverrideTx(ctx, tx, in.ClaimID, in.TokenID, true)
	if err != nil {
		return DeviceClaimOverrideResult{}, err
	}
	if claim.Status != "resolved" {
		return DeviceClaimOverrideResult{}, ErrClaimInvalidState
	}
	if claim.ID != observed.ID || claim.OrganizationID != observed.OrganizationID || claim.DeviceID != device.ID || claim.TokenID != observed.TokenID {
		return DeviceClaimOverrideResult{}, ErrConflict
	}

	token, err := getClaimTokenForUpdateTx(ctx, tx, claim.TokenID)
	if err != nil {
		return DeviceClaimOverrideResult{}, err
	}
	if token.ClaimedAt == nil {
		return DeviceClaimOverrideResult{}, ErrClaimInvalidState
	}
	if token.RevokedAt != nil {
		return DeviceClaimOverrideResult{}, ErrClaimRevoked
	}
	if token.OrganizationID != nil && *token.OrganizationID != claim.OrganizationID || stringValue(token.DeviceItemProfileID) != stringValue(device.DeviceItemProfileID) {
		return DeviceClaimOverrideResult{}, ErrConflict
	}
	var productAllowed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organizations o WHERE o.id::text=$1 AND
		(o.organization_kind<>'brand_cloud' OR EXISTS(SELECT 1 FROM device_item_profiles p WHERE p.id::text=$2 AND p.brand_cloud_id=o.id)))`, in.TargetOrganizationID, device.DeviceItemProfileID).Scan(&productAllowed); err != nil {
		return DeviceClaimOverrideResult{}, err
	}
	if !productAllowed {
		return DeviceClaimOverrideResult{}, ErrConflict
	}

	updatedDevice, err := scanDevice(tx.QueryRow(ctx, `
		UPDATE devices
		SET organization_id = $2, updated_at = $3
		WHERE id = $1
		RETURNING id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at, device_item_profile_id::text
	`, claim.DeviceID, in.TargetOrganizationID, now))
	if err != nil {
		if isUniqueViolation(err) {
			return DeviceClaimOverrideResult{}, ErrConflict
		}
		return DeviceClaimOverrideResult{}, err
	}

	updatedToken, err := scanDeviceClaimToken(tx.QueryRow(ctx, `
		UPDATE device_claim_tokens
		SET organization_id = $2, updated_at = $3
		WHERE id = $1
		RETURNING id::text, organization_id::text, created_by::text, device_item_profile_id::text, category, video_cloud_devid, activity_id, clip_public_key, service_options, metadata, notes, expires_at, claimed_at, revoked_at, created_at, updated_at
	`, claim.TokenID, in.TargetOrganizationID, now))
	if err != nil {
		return DeviceClaimOverrideResult{}, err
	}

	evidence, err := json.Marshal(defaultMetadata(in.Evidence))
	if err != nil {
		return DeviceClaimOverrideResult{}, err
	}
	updatedClaim, err := scanDeviceClaim(tx.QueryRow(ctx, `
		UPDATE device_claims
		SET organization_id = $2,
			device_id = $3,
			claimed_by = $4,
			status = $5,
			overridden_by = $4,
			override_reason = $6,
			override_evidence = $7,
			overridden_at = $8,
			updated_at = $8
		WHERE id = $1
		RETURNING id::text, claim_token_id::text, organization_id::text, device_id::text, claimed_by::text, status, provision_input, created_at, updated_at
	`, claim.ID, in.TargetOrganizationID, updatedDevice.ID, in.ActorUserID, in.Status, in.Reason, evidence, now))
	if err != nil {
		return DeviceClaimOverrideResult{}, err
	}

	payload := map[string]any{
		"claim_id":               claim.ID,
		"claim_token_id":         claim.TokenID,
		"device_id":              claim.DeviceID,
		"source_organization_id": claim.OrganizationID,
		"target_organization_id": in.TargetOrganizationID,
		"previous_device_status": device.Status,
		"previous_claim_status":  claim.Status,
		"resulting_claim_status": in.Status,
		"reason":                 in.Reason,
		"evidence":               defaultMetadata(in.Evidence),
		"video_cloud_devid":      token.VideoCloudDevid,
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      in.EventType,
		ActorUserID:    &in.ActorUserID,
		OrganizationID: &in.TargetOrganizationID,
		SubjectType:    "device_claim",
		SubjectID:      claim.ID,
		Payload:        payload,
	}); err != nil {
		return DeviceClaimOverrideResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return DeviceClaimOverrideResult{}, err
	}
	return DeviceClaimOverrideResult{Claim: updatedClaim, Token: updatedToken, Device: updatedDevice}, nil
}

func getClaimForOverrideTx(ctx context.Context, tx pgx.Tx, claimID, tokenID string, lock bool) (model.DeviceClaim, error) {
	query := `
		SELECT id::text, claim_token_id::text, organization_id::text, device_id::text, claimed_by::text, status, provision_input, created_at, updated_at
		FROM device_claims
		WHERE id = $1
	`
	arg := claimID
	if strings.TrimSpace(claimID) == "" {
		query = `
			SELECT id::text, claim_token_id::text, organization_id::text, device_id::text, claimed_by::text, status, provision_input, created_at, updated_at
			FROM device_claims
			WHERE claim_token_id = $1
		`
		arg = tokenID
	}
	if lock {
		query += " FOR UPDATE"
	}
	claim, err := scanDeviceClaim(tx.QueryRow(ctx, query, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceClaim{}, ErrNotFound
	}
	return claim, err
}

func getClaimTokenForUpdateTx(ctx context.Context, tx pgx.Tx, tokenID string) (model.DeviceClaimToken, error) {
	token, err := scanDeviceClaimToken(tx.QueryRow(ctx, `
		SELECT id::text, organization_id::text, created_by::text, device_item_profile_id::text, category, video_cloud_devid, activity_id, clip_public_key, service_options, metadata, notes, expires_at, claimed_at, revoked_at, created_at, updated_at
		FROM device_claim_tokens
		WHERE id = $1
		FOR UPDATE
	`, tokenID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceClaimToken{}, ErrNotFound
	}
	return token, err
}

func getClaimedDeviceByVideoDevidTx(ctx context.Context, tx pgx.Tx, orgID, videoCloudDevid string) (model.Device, error) {
	device, err := scanDevice(tx.QueryRow(ctx, `
		SELECT id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at, device_item_profile_id::text
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
	metadata := metadataDefaultsFromToken(token.Metadata)
	for key, value := range token.Metadata {
		if key == "metadata_defaults" {
			continue
		}
		metadata[key] = value
	}
	metadata[model.DeviceMetadataVideoCloudDevid] = token.VideoCloudDevid
	metadata[model.DeviceMetadataVideoCloudActivityID] = token.ActivityID
	metadata[model.DeviceMetadataVideoCloudClipPublicKey] = token.ClipPublicKey
	metadata[model.DeviceMetadataServiceOptions] = token.ServiceOptions
	manufacturer := metadataString(token.Metadata, "manufacturer")
	deviceModel := metadataString(token.Metadata, "model")
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return model.Device{}, err
	}
	return scanDevice(tx.QueryRow(ctx, `
		INSERT INTO devices (organization_id, name, category, manufacturer, model, metadata, device_item_profile_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at, device_item_profile_id::text
	`, orgID, name, token.Category, manufacturer, deviceModel, metadataJSON, token.DeviceItemProfileID))
}

func validateClaimServiceOptions(options []string) error {
	seen := map[string]struct{}{}
	for _, option := range options {
		switch option {
		case "mqtt", "iot_shadow", "video_streaming", "video_storage":
		default:
			return ErrClaimUnsupportedService
		}
		if _, ok := seen[option]; ok {
			return ErrClaimUnsupportedService
		}
		seen[option] = struct{}{}
	}
	return nil
}

func scanDeviceClaimToken(row rowScanner) (model.DeviceClaimToken, error) {
	var token model.DeviceClaimToken
	var metadata []byte
	var serviceOptions []byte
	err := row.Scan(
		&token.ID,
		&token.OrganizationID,
		&token.CreatedBy,
		&token.DeviceItemProfileID,
		&token.Category,
		&token.VideoCloudDevid,
		&token.ActivityID,
		&token.ClipPublicKey,
		&serviceOptions,
		&metadata,
		&token.Notes,
		&token.ExpiresAt,
		&token.ClaimedAt,
		&token.RevokedAt,
		&token.CreatedAt,
		&token.UpdatedAt,
	)
	if err != nil {
		return model.DeviceClaimToken{}, err
	}
	if len(serviceOptions) > 0 {
		if err := json.Unmarshal(serviceOptions, &token.ServiceOptions); err != nil {
			return model.DeviceClaimToken{}, err
		}
	}
	if token.ServiceOptions == nil {
		token.ServiceOptions = []string{}
	}
	token.Metadata, err = unmarshalJSONMap(metadata)
	if err != nil {
		return model.DeviceClaimToken{}, err
	}
	return token, nil
}

func serviceOptionSetsEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := map[string]int{}
	for _, option := range left {
		seen[option]++
	}
	for _, option := range right {
		if seen[option] == 0 {
			return false
		}
		seen[option]--
	}
	return true
}

func metadataDefaultsFromToken(metadata map[string]any) map[string]any {
	defaults := map[string]any{}
	raw, ok := metadata["metadata_defaults"]
	if !ok {
		return defaults
	}
	switch typed := raw.(type) {
	case map[string]any:
		for key, value := range typed {
			defaults[key] = value
		}
	case map[string]string:
		for key, value := range typed {
			defaults[key] = value
		}
	}
	return defaults
}

func metadataString(metadata map[string]any, key string) *string {
	value, ok := metadata[key].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
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
