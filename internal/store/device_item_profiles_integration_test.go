package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func TestDeviceItemProfileCRUDAndAudit(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	admin, err := env.store.Register(ctx, RegisterInput{
		Email:            "profile-admin@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Profile Admin Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	brand, err := env.store.CreateBrandCloud(ctx, admin.User.ID, BrandCloudInput{Name: "Profile Brand"})
	if err != nil {
		t.Fatal(err)
	}

	profile, err := env.store.CreateDeviceItemProfile(ctx, DeviceItemProfileCreateInput{
		ActorUserID:      &admin.User.ID,
		BrandCloudID:     brand.ID,
		ProfileKey:       "indoor-cam-v1",
		DisplayName:      "Indoor Camera V1",
		Category:         model.DeviceCategoryIPCamera,
		Manufacturer:     stringPtr("Realtek"),
		Model:            stringPtr("RTC-100"),
		MetadataDefaults: map[string]any{"lens": "wide"},
		MetadataSchema:   map[string]any{"type": "object"},
		CAProfile:        "brand-root-2026",
		IssuerProfile:    "factory-line-a",
		ServiceOptions:   []string{"video_storage", "video_streaming"},
		ClaimPolicy:      map[string]any{"ttl_hours": float64(24)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if profile.Status != model.DeviceItemProfileStatusActive ||
		profile.BrandCloudID != brand.ID ||
		profile.ProfileKey != "indoor-cam-v1" ||
		!stringSlicesEqual(profile.ServiceOptions, []string{"video_storage", "video_streaming"}) {
		t.Fatalf("unexpected created profile: %+v", profile)
	}

	listed, err := env.store.ListDeviceItemProfiles(ctx, DeviceItemProfileListFilter{BrandCloudID: brand.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Page.Total != 1 || listed.Profiles[0].ID != profile.ID {
		t.Fatalf("expected profile in brand list, got %+v", listed)
	}

	updated, err := env.store.UpdateDeviceItemProfile(ctx, DeviceItemProfileUpdateInput{
		ActorUserID:  &admin.User.ID,
		BrandCloudID: brand.ID,
		ProfileID:    profile.ID,
		DisplayName:  stringPtr("Indoor Camera V1 Rev B"),
		Model:        stringPtr("RTC-100B"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.DisplayName != "Indoor Camera V1 Rev B" || updated.Model == nil || *updated.Model != "RTC-100B" {
		t.Fatalf("unexpected updated profile: %+v", updated)
	}

	disabled, err := env.store.DisableDeviceItemProfile(ctx, brand.ID, profile.ID, &admin.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Status != model.DeviceItemProfileStatusDisabled || disabled.DisabledAt == nil {
		t.Fatalf("expected disabled profile, got %+v", disabled)
	}

	events, err := env.store.ListAuditEvents(ctx, AuditEventListFilter{SubjectType: "device_item_profile", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if events.Page.Total != 3 {
		t.Fatalf("expected create/update/disable audit events, got %+v", events)
	}
}

func TestDeviceItemProfileBacksClaimTokenSnapshotAndResolve(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	admin, err := env.store.Register(ctx, RegisterInput{
		Email:            "profile-claim-admin@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Profile Claim Admin Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	brand, err := env.store.CreateBrandCloud(ctx, admin.User.ID, BrandCloudInput{Name: "Claim Profile Brand"})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := env.store.Register(ctx, RegisterInput{
		Email:            "profile-claim-owner@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Profile Claim Owner Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := env.store.CreateDeviceItemProfile(ctx, DeviceItemProfileCreateInput{
		ActorUserID:      &admin.User.ID,
		BrandCloudID:     brand.ID,
		ProfileKey:       "profile-claim-cam",
		DisplayName:      "Profile Claim Camera",
		Category:         model.DeviceCategoryIPCamera,
		Manufacturer:     stringPtr("Realtek"),
		Model:            stringPtr("RTC-200"),
		MetadataDefaults: map[string]any{"region": "tw", "tier": "pro"},
		CAProfile:        "brand-ca",
		IssuerProfile:    "issuer-a",
		ServiceOptions:   []string{"video_streaming", "video_storage"},
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	token, err := env.store.CreateDeviceClaimToken(ctx, DeviceClaimTokenCreateInput{
		TokenHash:           "hashed-profile-claim",
		OrganizationID:      &owner.Organization.ID,
		CreatedBy:           &admin.User.ID,
		DeviceItemProfileID: &profile.ID,
		VideoCloudDevid:     "profile-video-device",
		ActivityID:          "profile-activity",
		ClipPublicKey:       "profile-clip-key",
		ExpiresAt:           now.Add(time.Hour),
		Now:                 now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if token.DeviceItemProfileID == nil || *token.DeviceItemProfileID != profile.ID {
		t.Fatalf("expected token to reference profile, got %+v", token)
	}
	if token.Category != model.DeviceCategoryIPCamera ||
		!stringSlicesEqual(token.ServiceOptions, []string{"video_streaming", "video_storage"}) ||
		token.Metadata["profile_key"] != "profile-claim-cam" ||
		token.Metadata["ca_profile"] != "brand-ca" ||
		token.Metadata["issuer_profile"] != "issuer-a" {
		t.Fatalf("expected token profile snapshot, got %+v", token)
	}

	resolved, err := env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		TokenHash:      "hashed-profile-claim",
		OrganizationID: owner.Organization.ID,
		RequestedBy:    owner.User.ID,
		DeviceName:     "Profile Camera",
		Now:            now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Device.Manufacturer == nil || *resolved.Device.Manufacturer != "Realtek" ||
		resolved.Device.Model == nil || *resolved.Device.Model != "RTC-200" ||
		resolved.Device.Metadata["region"] != "tw" ||
		resolved.Device.Metadata["profile_key"] != "profile-claim-cam" {
		t.Fatalf("expected profile inventory snapshot on registry device, got %+v", resolved.Device)
	}
	if !stringSlicesEqual(resolved.ProvisionInput.ServiceOptions, []string{"video_streaming", "video_storage"}) {
		t.Fatalf("expected explicit provision service options, got %+v", resolved.ProvisionInput)
	}

	_, err = env.store.CreateDeviceClaimToken(ctx, DeviceClaimTokenCreateInput{
		TokenHash:           "hashed-profile-mismatch",
		DeviceItemProfileID: &profile.ID,
		VideoCloudDevid:     "profile-video-mismatch",
		ActivityID:          "profile-activity-mismatch",
		ClipPublicKey:       "profile-clip-key-mismatch",
		ServiceOptions:      []string{"mqtt"},
		ExpiresAt:           now.Add(time.Hour),
		Now:                 now,
	})
	if !errors.Is(err, ErrClaimServiceOptionsMismatch) {
		t.Fatalf("expected ErrClaimServiceOptionsMismatch, got %v", err)
	}

	if _, err := env.store.DisableDeviceItemProfile(ctx, brand.ID, profile.ID, &admin.User.ID); err != nil {
		t.Fatal(err)
	}
	_, err = env.store.CreateDeviceClaimToken(ctx, DeviceClaimTokenCreateInput{
		TokenHash:           "hashed-profile-disabled",
		DeviceItemProfileID: &profile.ID,
		VideoCloudDevid:     "profile-video-disabled",
		ActivityID:          "profile-activity-disabled",
		ClipPublicKey:       "profile-clip-key-disabled",
		ExpiresAt:           now.Add(time.Hour),
		Now:                 now,
	})
	if !errors.Is(err, ErrDeviceItemProfileDisabled) {
		t.Fatalf("expected ErrDeviceItemProfileDisabled, got %v", err)
	}
}
