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

type DeviceItemProfileCreateInput struct {
	ActorUserID        *string
	PlatformOverride   bool
	BrandCloudID       string
	ProfileKey         string
	DisplayName        string
	Category           model.DeviceCategory
	Manufacturer       *string
	Model              *string
	MetadataDefaults   map[string]any
	MetadataSchema     map[string]any
	CAProfile          string
	IssuerProfile      string
	ServiceOptions     []string
	ClaimPolicy        map[string]any
	ProvisioningPolicy map[string]any
	Now                time.Time
}

type DeviceItemProfileUpdateInput struct {
	ActorUserID        *string
	PlatformOverride   bool
	BrandCloudID       string
	ProfileID          string
	DisplayName        *string
	Status             *model.DeviceItemProfileStatus
	Category           *model.DeviceCategory
	Manufacturer       *string
	Model              *string
	MetadataDefaults   map[string]any
	MetadataSchema     map[string]any
	CAProfile          *string
	IssuerProfile      *string
	ServiceOptions     []string
	ClaimPolicy        map[string]any
	ProvisioningPolicy map[string]any
	Now                time.Time
}

type DeviceItemProfileListFilter struct {
	BrandCloudID     string
	BrandCloudUserID string
	UserID           string
	Status           model.DeviceItemProfileStatus
	Limit            int
	Offset           int
}

// Low-level bootstrap/fixture persistence; HTTP uses the AsUser entrypoints.
func (s *Store) CreateDeviceItemProfile(ctx context.Context, in DeviceItemProfileCreateInput) (model.DeviceItemProfile, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	defer tx.Rollback(ctx)
	profile, err := createDeviceItemProfileTx(ctx, tx, in)
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	return profile, tx.Commit(ctx)
}

func createDeviceItemProfileTx(ctx context.Context, tx pgx.Tx, in DeviceItemProfileCreateInput) (model.DeviceItemProfile, error) {
	if err := validateDeviceItemProfileCreate(in); err != nil {
		return model.DeviceItemProfile{}, err
	}
	if err := ensureBrandCloud(ctx, tx, in.BrandCloudID); err != nil {
		return model.DeviceItemProfile{}, err
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	metadataDefaults, err := json.Marshal(defaultMetadata(in.MetadataDefaults))
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	metadataSchema, err := json.Marshal(defaultMetadata(in.MetadataSchema))
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	claimPolicy, err := json.Marshal(defaultMetadata(in.ClaimPolicy))
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	provisioningPolicy, err := json.Marshal(defaultMetadata(in.ProvisioningPolicy))
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	serviceOptions, err := json.Marshal(in.ServiceOptions)
	if err != nil {
		return model.DeviceItemProfile{}, err
	}

	profile, err := scanDeviceItemProfile(tx.QueryRow(ctx, `
		INSERT INTO device_item_profiles (
			brand_cloud_id, profile_key, display_name, status, category, manufacturer, model,
			metadata_defaults, metadata_schema, ca_profile, issuer_profile, service_options,
			claim_policy, provisioning_policy, created_at, updated_at
		)
		VALUES ($1, $2, $3, 'active', $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)
		RETURNING id::text, brand_cloud_id::text, profile_key, display_name, status, category,
			manufacturer, model, metadata_defaults, metadata_schema, ca_profile, issuer_profile,
			service_options, claim_policy, provisioning_policy, disabled_at, created_at, updated_at
	`, in.BrandCloudID, strings.TrimSpace(in.ProfileKey), strings.TrimSpace(in.DisplayName), in.Category, in.Manufacturer, in.Model,
		metadataDefaults, metadataSchema, strings.TrimSpace(in.CAProfile), strings.TrimSpace(in.IssuerProfile), serviceOptions,
		claimPolicy, provisioningPolicy, now))
	if err != nil {
		if strings.Contains(err.Error(), "device_item_profiles_brand_key_unique") {
			return model.DeviceItemProfile{}, ErrConflict
		}
		return model.DeviceItemProfile{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "device_item_profile_created",
		ActorUserID:    in.ActorUserID,
		OrganizationID: &in.BrandCloudID,
		SubjectType:    "device_item_profile",
		SubjectID:      profile.ID,
		Payload: map[string]any{
			"brand_cloud_id":  profile.BrandCloudID,
			"profile_key":     profile.ProfileKey,
			"service_options": profile.ServiceOptions,
		},
	}); err != nil {
		return model.DeviceItemProfile{}, err
	}
	if in.ActorUserID != nil && strings.TrimSpace(*in.ActorUserID) != "" {
		_, err := tx.Exec(ctx, `INSERT INTO role_assignments (role_id,actor_type,actor_id,scope_type,scope_id,organization_id)
			SELECT r.id,'user',u.id::text,'product',$2,$1::uuid FROM users u
			JOIN roles r ON r.name='product_owner' AND r.disabled_at IS NULL WHERE u.id::text=$3 ON CONFLICT DO NOTHING`, in.BrandCloudID, profile.ID, strings.TrimSpace(*in.ActorUserID))
		if err != nil {
			return model.DeviceItemProfile{}, err
		}
	}
	return profile, nil
}

func (s *Store) ListDeviceItemProfiles(ctx context.Context, in DeviceItemProfileListFilter) (DeviceItemProfilePage, error) {
	if err := ensureBrandCloud(ctx, s.db, in.BrandCloudID); err != nil {
		return DeviceItemProfilePage{}, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	status := string(in.Status)
	var total int
	actorType, actorID := "brand_cloud_user", strings.TrimSpace(in.BrandCloudUserID)
	if strings.TrimSpace(in.UserID) != "" {
		actorType, actorID = "user", strings.TrimSpace(in.UserID)
	}
	if err := s.db.QueryRow(ctx, `
		SELECT count(*)::int
		FROM device_item_profiles dip
		WHERE dip.brand_cloud_id = $1
			AND ($2 = '' OR status = $2)
			AND ($3 = '' OR EXISTS (
				SELECT 1 FROM role_assignments ra
				JOIN roles r ON r.id=ra.role_id AND r.disabled_at IS NULL
				WHERE ra.actor_type=$4 AND ra.actor_id=$3 AND ra.disabled_at IS NULL
				  AND ra.organization_id=dip.brand_cloud_id
				  AND ($4 <> 'user' OR user_can_access_brand_cloud_product($3,$1::text,dip.id::text))
				  AND (ra.scope_type='organization' OR (ra.scope_type='product' AND ra.scope_id=dip.id::text))
			))
	`, in.BrandCloudID, status, actorID, actorType).Scan(&total); err != nil {
		return DeviceItemProfilePage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT id::text, brand_cloud_id::text, profile_key, display_name, status, category,
			manufacturer, model, metadata_defaults, metadata_schema, ca_profile, issuer_profile,
			service_options, claim_policy, provisioning_policy, disabled_at, created_at, updated_at
		FROM device_item_profiles dip
		WHERE dip.brand_cloud_id = $1
			AND ($2 = '' OR status = $2)
			AND ($3 = '' OR EXISTS (
				SELECT 1 FROM role_assignments ra
				JOIN roles r ON r.id=ra.role_id AND r.disabled_at IS NULL
				WHERE ra.actor_type=$4 AND ra.actor_id=$3 AND ra.disabled_at IS NULL
				  AND ra.organization_id=dip.brand_cloud_id
				  AND ($4 <> 'user' OR user_can_access_brand_cloud_product($3,$1::text,dip.id::text))
				  AND (ra.scope_type='organization' OR (ra.scope_type='product' AND ra.scope_id=dip.id::text))
			))
		ORDER BY dip.created_at DESC
		LIMIT $5 OFFSET $6
	`, in.BrandCloudID, status, actorID, actorType, limit, in.Offset)
	if err != nil {
		return DeviceItemProfilePage{}, err
	}
	defer rows.Close()

	profiles := []model.DeviceItemProfile{}
	for rows.Next() {
		profile, err := scanDeviceItemProfile(rows)
		if err != nil {
			return DeviceItemProfilePage{}, err
		}
		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		return DeviceItemProfilePage{}, err
	}
	return DeviceItemProfilePage{Profiles: profiles, Page: Page{Limit: limit, Offset: in.Offset, Total: total}}, nil
}

func (s *Store) GetDeviceItemProfile(ctx context.Context, brandCloudID, profileID string) (model.DeviceItemProfile, error) {
	return getDeviceItemProfile(ctx, s.db, brandCloudID, profileID, false)
}

func getDeviceItemProfile(ctx context.Context, q rowQuerier, brandCloudID, profileID string, lock bool) (model.DeviceItemProfile, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	profile, err := scanDeviceItemProfile(q.QueryRow(ctx, `
		SELECT id::text, brand_cloud_id::text, profile_key, display_name, status, category,
			manufacturer, model, metadata_defaults, metadata_schema, ca_profile, issuer_profile,
			service_options, claim_policy, provisioning_policy, disabled_at, created_at, updated_at
		FROM device_item_profiles
		WHERE brand_cloud_id = $1 AND id = $2
	`+suffix, brandCloudID, profileID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceItemProfile{}, ErrNotFound
	}
	return profile, err
}

// Low-level bootstrap/fixture persistence; HTTP uses the AsUser entrypoints.
func (s *Store) UpdateDeviceItemProfile(ctx context.Context, in DeviceItemProfileUpdateInput) (model.DeviceItemProfile, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	defer tx.Rollback(ctx)
	profile, err := updateDeviceItemProfileTx(ctx, tx, in)
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	return profile, tx.Commit(ctx)
}

func updateDeviceItemProfileTx(ctx context.Context, tx pgx.Tx, in DeviceItemProfileUpdateInput) (model.DeviceItemProfile, error) {
	current, err := getDeviceItemProfile(ctx, tx, in.BrandCloudID, in.ProfileID, true)
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	if in.DisplayName != nil {
		current.DisplayName = strings.TrimSpace(*in.DisplayName)
	}
	if in.Status != nil {
		current.Status = *in.Status
	}
	if in.Category != nil {
		current.Category = *in.Category
	}
	if in.Manufacturer != nil {
		current.Manufacturer = in.Manufacturer
	}
	if in.Model != nil {
		current.Model = in.Model
	}
	if in.MetadataDefaults != nil {
		current.MetadataDefaults = in.MetadataDefaults
	}
	if in.MetadataSchema != nil {
		current.MetadataSchema = in.MetadataSchema
	}
	if in.CAProfile != nil {
		current.CAProfile = strings.TrimSpace(*in.CAProfile)
	}
	if in.IssuerProfile != nil {
		current.IssuerProfile = strings.TrimSpace(*in.IssuerProfile)
	}
	if in.ServiceOptions != nil {
		current.ServiceOptions = in.ServiceOptions
	}
	if in.ClaimPolicy != nil {
		current.ClaimPolicy = in.ClaimPolicy
	}
	if in.ProvisioningPolicy != nil {
		current.ProvisioningPolicy = in.ProvisioningPolicy
	}
	if err := validateDeviceItemProfile(current); err != nil {
		return model.DeviceItemProfile{}, err
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	metadataDefaults, err := json.Marshal(defaultMetadata(current.MetadataDefaults))
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	metadataSchema, err := json.Marshal(defaultMetadata(current.MetadataSchema))
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	claimPolicy, err := json.Marshal(defaultMetadata(current.ClaimPolicy))
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	provisioningPolicy, err := json.Marshal(defaultMetadata(current.ProvisioningPolicy))
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	serviceOptions, err := json.Marshal(current.ServiceOptions)
	if err != nil {
		return model.DeviceItemProfile{}, err
	}

	disabledAt := current.DisabledAt
	if current.Status == model.DeviceItemProfileStatusDisabled && disabledAt == nil {
		disabledAt = &now
	}
	if current.Status == model.DeviceItemProfileStatusActive {
		disabledAt = nil
	}
	updated, err := scanDeviceItemProfile(tx.QueryRow(ctx, `
		UPDATE device_item_profiles
		SET display_name = $3,
			status = $4,
			category = $5,
			manufacturer = $6,
			model = $7,
			metadata_defaults = $8,
			metadata_schema = $9,
			ca_profile = $10,
			issuer_profile = $11,
			service_options = $12,
			claim_policy = $13,
			provisioning_policy = $14,
			disabled_at = $15,
			updated_at = $16
		WHERE brand_cloud_id = $1 AND id = $2
		RETURNING id::text, brand_cloud_id::text, profile_key, display_name, status, category,
			manufacturer, model, metadata_defaults, metadata_schema, ca_profile, issuer_profile,
			service_options, claim_policy, provisioning_policy, disabled_at, created_at, updated_at
	`, in.BrandCloudID, in.ProfileID, current.DisplayName, current.Status, current.Category, current.Manufacturer, current.Model,
		metadataDefaults, metadataSchema, current.CAProfile, current.IssuerProfile, serviceOptions, claimPolicy, provisioningPolicy, disabledAt, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceItemProfile{}, ErrNotFound
	}
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "device_item_profile_updated",
		ActorUserID:    in.ActorUserID,
		OrganizationID: &in.BrandCloudID,
		SubjectType:    "device_item_profile",
		SubjectID:      updated.ID,
		Payload: map[string]any{
			"brand_cloud_id":  updated.BrandCloudID,
			"profile_key":     updated.ProfileKey,
			"status":          updated.Status,
			"service_options": updated.ServiceOptions,
		},
	}); err != nil {
		return model.DeviceItemProfile{}, err
	}
	return updated, nil
}

// Low-level bootstrap/fixture persistence; HTTP uses the AsUser entrypoints.
func (s *Store) DisableDeviceItemProfile(ctx context.Context, brandCloudID, profileID string, actorUserID *string) (model.DeviceItemProfile, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	defer tx.Rollback(ctx)
	profile, err := disableDeviceItemProfileTx(ctx, tx, brandCloudID, profileID, actorUserID)
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	return profile, tx.Commit(ctx)
}

func disableDeviceItemProfileTx(ctx context.Context, tx pgx.Tx, brandCloudID, profileID string, actorUserID *string) (model.DeviceItemProfile, error) {
	now := time.Now().UTC()
	disabled, err := scanDeviceItemProfile(tx.QueryRow(ctx, `
		UPDATE device_item_profiles
		SET status = 'disabled',
			disabled_at = COALESCE(disabled_at, $3),
			updated_at = $3
		WHERE brand_cloud_id = $1 AND id = $2
		RETURNING id::text, brand_cloud_id::text, profile_key, display_name, status, category,
			manufacturer, model, metadata_defaults, metadata_schema, ca_profile, issuer_profile,
			service_options, claim_policy, provisioning_policy, disabled_at, created_at, updated_at
	`, brandCloudID, profileID, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceItemProfile{}, ErrNotFound
	}
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{
		EventType:      "device_item_profile_disabled",
		ActorUserID:    actorUserID,
		OrganizationID: &brandCloudID,
		SubjectType:    "device_item_profile",
		SubjectID:      disabled.ID,
		Payload: map[string]any{
			"brand_cloud_id": disabled.BrandCloudID,
			"profile_key":    disabled.ProfileKey,
			"status":         disabled.Status,
		},
	}); err != nil {
		return model.DeviceItemProfile{}, err
	}
	return disabled, nil
}

func validateDeviceItemProfileCreate(in DeviceItemProfileCreateInput) error {
	profile := model.DeviceItemProfile{
		BrandCloudID:       in.BrandCloudID,
		ProfileKey:         strings.TrimSpace(in.ProfileKey),
		DisplayName:        strings.TrimSpace(in.DisplayName),
		Status:             model.DeviceItemProfileStatusActive,
		Category:           in.Category,
		MetadataDefaults:   in.MetadataDefaults,
		MetadataSchema:     in.MetadataSchema,
		CAProfile:          strings.TrimSpace(in.CAProfile),
		IssuerProfile:      strings.TrimSpace(in.IssuerProfile),
		ServiceOptions:     in.ServiceOptions,
		ClaimPolicy:        in.ClaimPolicy,
		ProvisioningPolicy: in.ProvisioningPolicy,
	}
	return validateDeviceItemProfile(profile)
}

func validateDeviceItemProfile(profile model.DeviceItemProfile) error {
	if strings.TrimSpace(profile.BrandCloudID) == "" ||
		strings.TrimSpace(profile.ProfileKey) == "" ||
		strings.TrimSpace(profile.DisplayName) == "" ||
		strings.TrimSpace(profile.CAProfile) == "" ||
		strings.TrimSpace(profile.IssuerProfile) == "" {
		return ErrNotFound
	}
	if profile.Status != model.DeviceItemProfileStatusActive && profile.Status != model.DeviceItemProfileStatusDisabled {
		return ErrConflict
	}
	if profile.Category != model.DeviceCategoryIPCamera &&
		profile.Category != model.DeviceCategoryMQTT &&
		profile.Category != model.DeviceCategoryGeneric {
		return ErrClaimUnsupportedCategory
	}
	if err := validateClaimServiceOptions(profile.ServiceOptions); err != nil {
		return err
	}
	if len(profile.ServiceOptions) == 0 {
		return ErrClaimUnsupportedService
	}
	return nil
}

func getDeviceItemProfileByID(ctx context.Context, tx pgx.Tx, profileID string) (model.DeviceItemProfile, error) {
	profile, err := scanDeviceItemProfile(tx.QueryRow(ctx, `
		SELECT id::text, brand_cloud_id::text, profile_key, display_name, status, category,
			manufacturer, model, metadata_defaults, metadata_schema, ca_profile, issuer_profile,
			service_options, claim_policy, provisioning_policy, disabled_at, created_at, updated_at
		FROM device_item_profiles
		WHERE id = $1
	`, profileID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeviceItemProfile{}, ErrNotFound
	}
	return profile, err
}

func ensureBrandCloud(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, brandCloudID string) error {
	var exists bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM organizations
			WHERE id = $1 AND organization_kind = 'brand_cloud'
		)
	`, brandCloudID).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

func scanDeviceItemProfile(row rowScanner) (model.DeviceItemProfile, error) {
	var profile model.DeviceItemProfile
	var metadataDefaults, metadataSchema, serviceOptions, claimPolicy, provisioningPolicy []byte
	err := row.Scan(
		&profile.ID,
		&profile.BrandCloudID,
		&profile.ProfileKey,
		&profile.DisplayName,
		&profile.Status,
		&profile.Category,
		&profile.Manufacturer,
		&profile.Model,
		&metadataDefaults,
		&metadataSchema,
		&profile.CAProfile,
		&profile.IssuerProfile,
		&serviceOptions,
		&claimPolicy,
		&provisioningPolicy,
		&profile.DisabledAt,
		&profile.CreatedAt,
		&profile.UpdatedAt,
	)
	if err != nil {
		return model.DeviceItemProfile{}, err
	}
	var unmarshalErr error
	profile.MetadataDefaults, unmarshalErr = unmarshalJSONMap(metadataDefaults)
	if unmarshalErr != nil {
		return model.DeviceItemProfile{}, unmarshalErr
	}
	profile.MetadataSchema, unmarshalErr = unmarshalJSONMap(metadataSchema)
	if unmarshalErr != nil {
		return model.DeviceItemProfile{}, unmarshalErr
	}
	profile.ClaimPolicy, unmarshalErr = unmarshalJSONMap(claimPolicy)
	if unmarshalErr != nil {
		return model.DeviceItemProfile{}, unmarshalErr
	}
	profile.ProvisioningPolicy, unmarshalErr = unmarshalJSONMap(provisioningPolicy)
	if unmarshalErr != nil {
		return model.DeviceItemProfile{}, unmarshalErr
	}
	if len(serviceOptions) > 0 {
		if err := json.Unmarshal(serviceOptions, &profile.ServiceOptions); err != nil {
			return model.DeviceItemProfile{}, err
		}
	}
	if profile.ServiceOptions == nil {
		profile.ServiceOptions = []string{}
	}
	return profile, nil
}
