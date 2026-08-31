package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func TestUnprovisionDeviceRetainsClaimHistoryAndAllowsReplacementClaim(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	registered, err := env.store.Register(ctx, RegisterInput{
		Email:            "unprovision-store-owner@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Unprovision Store Org",
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := env.store.CreateDeviceClaimToken(ctx, DeviceClaimTokenCreateInput{
		OrganizationID:  &registered.Organization.ID,
		TokenHash:       "hashed-unprovision-store-token",
		Category:        model.DeviceCategoryMQTT,
		VideoCloudDevid: "unprovision-store-video-device",
		ActivityID:      "unprovision-store-activity",
		ClipPublicKey:   "unprovision-store-clip-key",
		ServiceOptions:  []string{"mqtt"},
		ExpiresAt:       now.Add(time.Hour),
		Now:             now,
	}); err != nil {
		t.Fatal(err)
	}

	resolved, err := env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		TokenHash:      "hashed-unprovision-store-token",
		OrganizationID: registered.Organization.ID,
		RequestedBy:    registered.User.ID,
		DeviceName:     "Store Unprovision MQTT Device",
		Now:            now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	var tenantActorID string
	if err := env.db.QueryRow(ctx, `
		INSERT INTO brand_cloud_users (
			brand_cloud_id, email, password_hash, email_verified,
			email_verified_at, signup_pending_verification
		)
		VALUES ($1, 'tenant-unprovision-actor@example.com', 'hash', true, $2, false)
		RETURNING id::text
	`, registered.Organization.ID, now).Scan(&tenantActorID); err != nil {
		t.Fatal(err)
	}

	if _, err := env.store.UnprovisionDevice(ctx, DeviceUnprovisionInput{
		OrganizationID: registered.Organization.ID,
		DeviceID:       resolved.Device.ID,
		ActorUserID:    tenantActorID,
		Reason:         "owner resale",
		Now:            now.Add(2 * time.Minute),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retired tenant identity authorized unprovision: %v", err)
	}
	result, err := env.store.UnprovisionDevice(ctx, DeviceUnprovisionInput{
		OrganizationID: registered.Organization.ID,
		DeviceID:       resolved.Device.ID,
		ActorUserID:    registered.User.ID,
		Reason:         "owner resale", Now: now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.DeviceID != resolved.Device.ID || result.OrganizationID != registered.Organization.ID {
		t.Fatalf("unexpected unprovision result: %+v", result)
	}
	if result.Operation.OperationType != model.DeviceOperationTypeUnprovision || result.Message.MessageType != "DeviceUnprovisionRequested" {
		t.Fatalf("expected unprovision lifecycle records, got operation=%+v message=%+v", result.Operation, result.Message)
	}

	var devices, claims, operations, outbox, audits int
	if err := env.db.QueryRow(ctx, `SELECT count(*)::int FROM devices WHERE id = $1`, resolved.Device.ID).Scan(&devices); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*)::int FROM device_claims WHERE id = $1 AND device_id = $2`, resolved.Claim.ID, resolved.Device.ID).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*)::int FROM device_operations WHERE operation_id = $1`, result.Operation.OperationID).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*)::int FROM device_message_outbox WHERE operation_id = $1`, result.Operation.OperationID).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*)::int FROM audit_events WHERE event_type = 'device_unprovisioned' AND subject_id = $1 AND actor_user_id = $2`, resolved.Device.ID, registered.User.ID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if devices != 0 || claims != 1 || operations != 1 || outbox != 1 || audits != 1 {
		t.Fatalf("unexpected unprovision retention counts: devices=%d claims=%d operations=%d outbox=%d audits=%d", devices, claims, operations, outbox, audits)
	}

	if _, err := env.store.CreateDeviceClaimToken(ctx, DeviceClaimTokenCreateInput{
		OrganizationID:  &registered.Organization.ID,
		TokenHash:       "hashed-unprovision-store-replacement",
		Category:        model.DeviceCategoryMQTT,
		VideoCloudDevid: "unprovision-store-video-device",
		ActivityID:      "unprovision-store-activity-2",
		ClipPublicKey:   "unprovision-store-clip-key-2",
		ServiceOptions:  []string{"mqtt"},
		ExpiresAt:       now.Add(time.Hour),
		Now:             now.Add(3 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	replacement, err := env.store.ResolveDeviceClaimToken(ctx, DeviceClaimResolveInput{
		TokenHash:      "hashed-unprovision-store-replacement",
		OrganizationID: registered.Organization.ID,
		RequestedBy:    registered.User.ID,
		DeviceName:     "Replacement Store Unprovision MQTT Device",
		Now:            now.Add(4 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Device.ID == resolved.Device.ID || replacement.ProvisionInput.VideoCloudDevid != "unprovision-store-video-device" {
		t.Fatalf("expected replacement claim for same factory identity, got %+v", replacement)
	}
}
