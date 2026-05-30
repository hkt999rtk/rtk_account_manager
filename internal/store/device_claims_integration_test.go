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
		ServiceOptions:  []string{"video_streaming", "video_storage"},
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
	if !stringSlicesEqual(result.ProvisionInput.ServiceOptions, []string{"video_streaming", "video_storage"}) {
		t.Fatalf("unexpected provision service options: %+v", result.ProvisionInput.ServiceOptions)
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

func TestResolveDeviceClaimTokenRejectsDuplicateDeviceClaim(t *testing.T) {
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
		ServiceOptions:  []string{"video_streaming", "video_storage"},
		ExpiresAt:       now.Add(time.Hour),
		Now:             now,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		TokenHash:      "hashed-claim-match",
		OrganizationID: registered.Organization.ID,
		RequestedBy:    registered.User.ID,
		DeviceName:     "Ignored New Name",
		Now:            now.Add(time.Minute),
	})
	if !errors.Is(err, ErrClaimAlreadyClaimed) {
		t.Fatalf("expected ErrClaimAlreadyClaimed for existing device %s, got %v", existing.ID, err)
	}

	var deviceCount int
	if err := env.db.QueryRow(ctx, `SELECT count(*)::int FROM devices`).Scan(&deviceCount); err != nil {
		t.Fatal(err)
	}
	if deviceCount != 1 {
		t.Fatalf("expected duplicate claim resolve to leave existing device unchanged, got %d devices", deviceCount)
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
		ServiceOptions:  []string{"video_streaming", "video_storage"},
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
		ServiceOptions:  []string{"video_streaming", "video_storage"},
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
	if !stringSlicesEqual(fetched.ServiceOptions, []string{"video_streaming", "video_storage"}) {
		t.Fatalf("unexpected fetched service options: %+v", fetched.ServiceOptions)
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

func TestCreateDeviceClaimTokenRejectsUnsupportedServiceOptions(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	if _, err := env.store.CreateDeviceClaimToken(ctx, DeviceClaimTokenCreateInput{
		TokenHash:       "hashed-admin-token-unsupported-service",
		Category:        model.DeviceCategoryIPCamera,
		VideoCloudDevid: "unsupported-service-video-device",
		ActivityID:      "unsupported-service-activity",
		ClipPublicKey:   "unsupported-service-clip-key",
		ServiceOptions:  []string{"mqtt", "admin"},
		ExpiresAt:       now.Add(time.Hour),
		Now:             now,
	}); !errors.Is(err, ErrClaimUnsupportedService) {
		t.Fatalf("expected ErrClaimUnsupportedService, got %v", err)
	}

	if _, err := env.store.CreateDeviceClaimToken(ctx, DeviceClaimTokenCreateInput{
		TokenHash:       "hashed-admin-token-duplicate-service",
		Category:        model.DeviceCategoryIPCamera,
		VideoCloudDevid: "duplicate-service-video-device",
		ActivityID:      "duplicate-service-activity",
		ClipPublicKey:   "duplicate-service-clip-key",
		ServiceOptions:  []string{"mqtt", "mqtt"},
		ExpiresAt:       now.Add(time.Hour),
		Now:             now,
	}); !errors.Is(err, ErrClaimUnsupportedService) {
		t.Fatalf("expected ErrClaimUnsupportedService for duplicate service option, got %v", err)
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
		ServiceOptions:  []string{"video_streaming", "video_storage"},
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
		ServiceOptions:  []string{"video_streaming", "video_storage"},
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

func TestDeviceClaimTransferMovesOwnershipAndAudits(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	source, err := env.store.Register(ctx, RegisterInput{
		Email:            "claim-transfer-source@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Claim Transfer Source",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := env.store.Register(ctx, RegisterInput{
		Email:            "claim-transfer-target@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Claim Transfer Target",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	token, err := env.store.CreateDeviceClaimToken(ctx, DeviceClaimTokenCreateInput{
		OrganizationID:  &source.Organization.ID,
		TokenHash:       "hashed-transfer-token",
		Category:        model.DeviceCategoryIPCamera,
		VideoCloudDevid: "transfer-video-device",
		ActivityID:      "transfer-activity",
		ClipPublicKey:   "transfer-clip-key",
		ServiceOptions:  []string{"video_streaming", "video_storage"},
		ExpiresAt:       now.Add(time.Hour),
		Now:             now,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		TokenHash:      "hashed-transfer-token",
		OrganizationID: source.Organization.ID,
		RequestedBy:    source.User.ID,
		DeviceName:     "Transfer Camera",
		Now:            now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	transferred, err := env.store.TransferDeviceClaim(ctx, DeviceClaimTransferInput{
		ClaimID:              resolved.Claim.ID,
		TargetOrganizationID: target.Organization.ID,
		ActorUserID:          target.User.ID,
		Reason:               "support verified ownership transfer",
		Evidence:             map[string]any{"ticket": "SUP-131"},
		Now:                  now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if transferred.Claim.OrganizationID != target.Organization.ID || transferred.Claim.Status != "transferred" {
		t.Fatalf("expected transferred claim in target org, got %+v", transferred.Claim)
	}
	if transferred.Device.OrganizationID != target.Organization.ID {
		t.Fatalf("expected transferred device in target org, got %+v", transferred.Device)
	}
	if transferred.Token.OrganizationID == nil || *transferred.Token.OrganizationID != target.Organization.ID || transferred.Token.ID != token.ID {
		t.Fatalf("expected transferred token target org, got %+v", transferred.Token)
	}

	_, err = env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		TokenHash:      "hashed-transfer-token",
		OrganizationID: target.Organization.ID,
		RequestedBy:    target.User.ID,
		DeviceName:     "Transfer Camera Again",
		Now:            now.Add(3 * time.Minute),
	})
	if !errors.Is(err, ErrClaimAlreadyClaimed) {
		t.Fatalf("normal resolve must still reject transferred claim token, got %v", err)
	}

	events, err := env.store.ListAuditEvents(ctx, AuditEventListFilter{EventType: "device_claim_transferred", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if events.Page.Total != 1 || events.Events[0].SubjectID != resolved.Claim.ID {
		t.Fatalf("expected transfer audit event, got %+v", events)
	}
	if events.Events[0].Payload["source_organization_id"] != source.Organization.ID ||
		events.Events[0].Payload["target_organization_id"] != target.Organization.ID {
		t.Fatalf("expected transfer audit ownership facts, got %+v", events.Events[0].Payload)
	}
}

func TestDeviceClaimReclaimRequiresEvidenceAndRejectsInvalidTransitions(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	source, err := env.store.Register(ctx, RegisterInput{
		Email:            "claim-reclaim-source@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Claim Reclaim Source",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := env.store.Register(ctx, RegisterInput{
		Email:            "claim-reclaim-target@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Claim Reclaim Target",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	token, err := env.store.CreateDeviceClaimToken(ctx, DeviceClaimTokenCreateInput{
		OrganizationID:  &source.Organization.ID,
		TokenHash:       "hashed-reclaim-token",
		Category:        model.DeviceCategoryIPCamera,
		VideoCloudDevid: "reclaim-video-device",
		ActivityID:      "reclaim-activity",
		ClipPublicKey:   "reclaim-clip-key",
		ServiceOptions:  []string{"video_streaming", "video_storage"},
		ExpiresAt:       now.Add(time.Hour),
		Now:             now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ReclaimDeviceClaimToken(ctx, DeviceClaimReclaimInput{
		TokenID:              token.ID,
		TargetOrganizationID: target.Organization.ID,
		ActorUserID:          target.User.ID,
		Reason:               "factory reset evidence",
		Evidence:             map[string]any{"factory_reset": true},
		Now:                  now.Add(time.Minute),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unclaimed token must not be reclaimed, got %v", err)
	}
	resolved, err := env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		TokenHash:      "hashed-reclaim-token",
		OrganizationID: source.Organization.ID,
		RequestedBy:    source.User.ID,
		DeviceName:     "Reclaim Camera",
		Now:            now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ReclaimDeviceClaimToken(ctx, DeviceClaimReclaimInput{
		TokenID:              token.ID,
		TargetOrganizationID: target.Organization.ID,
		ActorUserID:          target.User.ID,
		Reason:               "factory reset evidence missing",
		Now:                  now.Add(3 * time.Minute),
	}); !errors.Is(err, ErrClaimEvidenceRequired) {
		t.Fatalf("expected ErrClaimEvidenceRequired, got %v", err)
	}

	reclaimed, err := env.store.ReclaimDeviceClaimToken(ctx, DeviceClaimReclaimInput{
		TokenID:              token.ID,
		TargetOrganizationID: target.Organization.ID,
		ActorUserID:          target.User.ID,
		Reason:               "factory reset and support verified",
		Evidence:             map[string]any{"factory_reset": true, "ticket": "SUP-132"},
		Now:                  now.Add(4 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.Claim.ID != resolved.Claim.ID || reclaimed.Claim.Status != "reclaimed" || reclaimed.Claim.OrganizationID != target.Organization.ID {
		t.Fatalf("expected reclaimed claim in target org, got %+v", reclaimed.Claim)
	}
	if reclaimed.Device.OrganizationID != target.Organization.ID {
		t.Fatalf("expected reclaimed device in target org, got %+v", reclaimed.Device)
	}
	if _, err := env.store.TransferDeviceClaim(ctx, DeviceClaimTransferInput{
		ClaimID:              resolved.Claim.ID,
		TargetOrganizationID: source.Organization.ID,
		ActorUserID:          source.User.ID,
		Reason:               "second transfer should fail",
		Evidence:             map[string]any{"ticket": "SUP-133"},
		Now:                  now.Add(5 * time.Minute),
	}); !errors.Is(err, ErrClaimInvalidState) {
		t.Fatalf("expected ErrClaimInvalidState after reclaim, got %v", err)
	}

	events, err := env.store.ListAuditEvents(ctx, AuditEventListFilter{EventType: "device_claim_reclaimed", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if events.Page.Total != 1 || events.Events[0].SubjectID != resolved.Claim.ID {
		t.Fatalf("expected reclaim audit event, got %+v", events)
	}
}

func stringSlicesEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
