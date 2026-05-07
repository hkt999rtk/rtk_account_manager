package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func TestResolveDeviceClaimTokenCreatesDeviceAndClaim(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	registered, err := env.store.Register(ctx, RegisterInput{
		Email:            "claim-owner@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Claim Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	token, err := env.store.CreateDeviceClaimToken(ctx, DeviceClaimTokenCreateInput{
		TokenHash:       "hashed-claim-token-1",
		Category:        model.DeviceCategoryIPCamera,
		VideoCloudDevid: "video-device-1",
		ActivityID:      "activity-1",
		ClipPublicKey:   "clip-key-1",
		ExpiresAt:       now.Add(time.Hour),
		Now:             now,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		TokenHash:      "hashed-claim-token-1",
		OrganizationID: registered.Organization.ID,
		RequestedBy:    registered.User.ID,
		DeviceName:     "Front Door Camera",
		Now:            now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.Claim.TokenID != token.ID {
		t.Fatalf("expected claim token id %s, got %+v", token.ID, result.Claim)
	}
	if result.Claim.OrganizationID != registered.Organization.ID || result.Claim.DeviceID != result.Device.ID {
		t.Fatalf("expected claim to bind org/device, got %+v device %+v", result.Claim, result.Device)
	}
	if result.Device.Name != "Front Door Camera" || result.Device.Category != model.DeviceCategoryIPCamera {
		t.Fatalf("unexpected claimed device: %+v", result.Device)
	}
	if got := result.Device.Metadata[model.DeviceMetadataVideoCloudDevid]; got != "video-device-1" {
		t.Fatalf("expected video devid metadata, got %+v", result.Device.Metadata)
	}
	if result.ProvisionInput.VideoCloudDevid != "video-device-1" ||
		result.ProvisionInput.ActivityID != "activity-1" ||
		result.ProvisionInput.ClipPublicKey != "clip-key-1" {
		t.Fatalf("unexpected provision input: %+v", result.ProvisionInput)
	}

	var rawTokenCount int
	if err := env.db.QueryRow(ctx, `
		SELECT count(*)::int
		FROM device_claim_tokens
		WHERE token_hash = 'claim-token-1' OR metadata::text LIKE '%claim-token-1%'
	`).Scan(&rawTokenCount); err != nil {
		t.Fatal(err)
	}
	if rawTokenCount != 0 {
		t.Fatal("raw claim token value was persisted")
	}

	var operations, outbox int
	if err := env.db.QueryRow(ctx, `SELECT count(*)::int FROM device_operations`).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*)::int FROM device_message_outbox`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if operations != 0 || outbox != 0 {
		t.Fatalf("claim resolve must not start provisioning, operations=%d outbox=%d", operations, outbox)
	}
}

func TestResolveDeviceClaimTokenMatchesExistingDevice(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	registered, err := env.store.Register(ctx, RegisterInput{
		Email:            "claim-match@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Claim Match Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	existing, err := env.store.CreateDevice(ctx, registered.Organization.ID, DeviceInput{
		Name:     "Existing Camera",
		Category: model.DeviceCategoryIPCamera,
		Metadata: map[string]any{
			model.DeviceMetadataVideoCloudDevid: "video-device-match",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := env.store.CreateDeviceClaimToken(ctx, DeviceClaimTokenCreateInput{
		TokenHash:       "hashed-claim-match",
		Category:        model.DeviceCategoryIPCamera,
		VideoCloudDevid: "video-device-match",
		ActivityID:      "activity-match",
		ClipPublicKey:   "clip-key-match",
		ExpiresAt:       now.Add(time.Hour),
		Now:             now,
	}); err != nil {
		t.Fatal(err)
	}

	result, err := env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		TokenHash:      "hashed-claim-match",
		OrganizationID: registered.Organization.ID,
		RequestedBy:    registered.User.ID,
		DeviceName:     "Ignored New Name",
		Now:            now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Device.ID != existing.ID {
		t.Fatalf("expected existing device %s to be matched, got %+v", existing.ID, result.Device)
	}

	var deviceCount int
	if err := env.db.QueryRow(ctx, `SELECT count(*)::int FROM devices`).Scan(&deviceCount); err != nil {
		t.Fatal(err)
	}
	if deviceCount != 1 {
		t.Fatalf("expected claim resolve to match instead of creating a device, got %d devices", deviceCount)
	}
}

func TestResolveDeviceClaimTokenRejectsInvalidToken(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	registered, err := env.store.Register(ctx, RegisterInput{
		Email:            "invalid-claim@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Invalid Claim Org",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		TokenHash:      "missing-token-hash",
		OrganizationID: registered.Organization.ID,
		RequestedBy:    registered.User.ID,
		DeviceName:     "Invalid Camera",
		Now:            time.Now().UTC(),
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for invalid token, got %v", err)
	}
}

func TestResolveDeviceClaimTokenRejectsExpiredToken(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	registered, err := env.store.Register(ctx, RegisterInput{
		Email:            "expired-claim@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Expired Claim Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := env.store.CreateDeviceClaimToken(ctx, DeviceClaimTokenCreateInput{
		TokenHash:       "hashed-expired-claim",
		Category:        model.DeviceCategoryIPCamera,
		VideoCloudDevid: "video-device-expired",
		ActivityID:      "activity-expired",
		ClipPublicKey:   "clip-key-expired",
		ExpiresAt:       now.Add(-time.Minute),
		Now:             now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		TokenHash:      "hashed-expired-claim",
		OrganizationID: registered.Organization.ID,
		RequestedBy:    registered.User.ID,
		DeviceName:     "Expired Camera",
		Now:            now,
	})
	if !errors.Is(err, ErrClaimExpired) {
		t.Fatalf("expected ErrClaimExpired, got %v", err)
	}
}

func TestDeviceClaimTokenAdminLifecycle(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	registered, err := env.store.Register(ctx, RegisterInput{
		Email:            "claim-admin-store@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Claim Admin Store Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	notes := "factory batch 7"
	token, err := env.store.CreateDeviceClaimToken(ctx, DeviceClaimTokenCreateInput{
		TokenHash:       "hashed-admin-token",
		OrganizationID:  &registered.Organization.ID,
		CreatedBy:       &registered.User.ID,
		Category:        model.DeviceCategoryIPCamera,
		VideoCloudDevid: "admin-video-device",
		ActivityID:      "admin-activity",
		ClipPublicKey:   "admin-clip-key",
		Metadata:        map[string]any{"batch": "7"},
		Notes:           &notes,
		ExpiresAt:       now.Add(time.Hour),
		Now:             now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if token.CreatedBy == nil || *token.CreatedBy != registered.User.ID {
		t.Fatalf("expected created_by to be tracked, got %+v", token)
	}
	if token.Notes == nil || *token.Notes != notes {
		t.Fatalf("expected notes to be tracked, got %+v", token)
	}

	listed, err := env.store.ListDeviceClaimTokens(ctx, DeviceClaimTokenListFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Page.Total != 1 || len(listed.Tokens) != 1 || listed.Tokens[0].ID != token.ID {
		t.Fatalf("expected token in list, got %+v", listed)
	}

	fetched, err := env.store.GetDeviceClaimToken(ctx, token.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fetched.ID != token.ID || fetched.VideoCloudDevid != "admin-video-device" {
		t.Fatalf("unexpected fetched token: %+v", fetched)
	}

	revoked, err := env.store.RevokeDeviceClaimToken(ctx, token.ID, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if revoked.RevokedAt == nil {
		t.Fatalf("expected revoked_at, got %+v", revoked)
	}

	_, err = env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		TokenHash:      "hashed-admin-token",
		OrganizationID: registered.Organization.ID,
		RequestedBy:    registered.User.ID,
		DeviceName:     "Revoked Camera",
		Now:            now.Add(2 * time.Minute),
	})
	if !errors.Is(err, ErrClaimRevoked) {
		t.Fatalf("expected ErrClaimRevoked, got %v", err)
	}
}

func TestResolveDeviceClaimTokenRejectsAlreadyClaimedToken(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	registered, err := env.store.Register(ctx, RegisterInput{
		Email:            "first-claim@example.com",
		PasswordHash:     "hash",
		OrganizationName: "First Claim Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := env.store.CreateDeviceClaimToken(ctx, DeviceClaimTokenCreateInput{
		TokenHash:       "hashed-already-claimed",
		Category:        model.DeviceCategoryIPCamera,
		VideoCloudDevid: "video-device-claimed",
		ActivityID:      "activity-claimed",
		ClipPublicKey:   "clip-key-claimed",
		ExpiresAt:       now.Add(time.Hour),
		Now:             now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		TokenHash:      "hashed-already-claimed",
		OrganizationID: registered.Organization.ID,
		RequestedBy:    registered.User.ID,
		DeviceName:     "Claimed Camera",
		Now:            now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		TokenHash:      "hashed-already-claimed",
		OrganizationID: registered.Organization.ID,
		RequestedBy:    registered.User.ID,
		DeviceName:     "Claimed Camera Again",
		Now:            now.Add(2 * time.Minute),
	})
	if !errors.Is(err, ErrClaimAlreadyClaimed) {
		t.Fatalf("expected ErrClaimAlreadyClaimed, got %v", err)
	}
}

func TestResolveDeviceClaimTokenRejectsCrossOrganizationToken(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	first, err := env.store.Register(ctx, RegisterInput{
		Email:            "claim-scope-owner@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Claim Scope Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := env.store.Register(ctx, RegisterInput{
		Email:            "claim-scope-other@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Other Claim Scope Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := env.store.CreateDeviceClaimToken(ctx, DeviceClaimTokenCreateInput{
		TokenHash:       "hashed-cross-org",
		OrganizationID:  &first.Organization.ID,
		Category:        model.DeviceCategoryIPCamera,
		VideoCloudDevid: "video-device-cross-org",
		ActivityID:      "activity-cross-org",
		ClipPublicKey:   "clip-key-cross-org",
		ExpiresAt:       now.Add(time.Hour),
		Now:             now,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		TokenHash:      "hashed-cross-org",
		OrganizationID: second.Organization.ID,
		RequestedBy:    second.User.ID,
		DeviceName:     "Cross Org Camera",
		Now:            now.Add(time.Minute),
	})
	if !errors.Is(err, ErrClaimCrossOrganization) {
		t.Fatalf("expected ErrClaimCrossOrganization, got %v", err)
	}
}

func TestResolveDeviceClaimTokenRejectsUnsupportedCategory(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	registered, err := env.store.Register(ctx, RegisterInput{
		Email:            "unsupported-claim@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Unsupported Claim Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := env.db.Exec(ctx, `
		INSERT INTO device_claim_tokens (
			token_hash, category, video_cloud_devid, activity_id, clip_public_key, expires_at, created_at, updated_at
		)
		VALUES ('hashed-unsupported-category', 'unsupported_camera', 'video-device-unsupported', 'activity-unsupported', 'clip-key-unsupported', $1, $2, $2)
	`, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}

	_, err = env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		TokenHash:      "hashed-unsupported-category",
		OrganizationID: registered.Organization.ID,
		RequestedBy:    registered.User.ID,
		DeviceName:     "Unsupported Camera",
		Now:            now.Add(time.Minute),
	})
	if !errors.Is(err, ErrClaimUnsupportedCategory) {
		t.Fatalf("expected ErrClaimUnsupportedCategory, got %v", err)
	}
}
