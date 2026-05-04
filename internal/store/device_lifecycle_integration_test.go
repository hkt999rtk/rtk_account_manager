package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
)

func TestStartDeviceLifecycleOperationPersistsPendingProvisionMetadata(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	orgID, userID, deviceID := createDeviceFixture(t, env)

	ctx := context.Background()
	if _, err := env.db.Exec(ctx, `
		UPDATE devices
		SET metadata = metadata
			|| jsonb_build_object(
				'video_cloud_activation_status', 'deactivated',
				'video_cloud_activated_at', '2026-04-28T09:00:00Z',
				'video_cloud_deactivated_at', '2026-04-28T10:00:00Z',
				'video_cloud_last_error', jsonb_build_object('code', 'stale_error')
			)
		WHERE id = $1
	`, deviceID); err != nil {
		t.Fatal(err)
	}

	result, err := env.store.StartDeviceLifecycleOperation(ctx, DeviceLifecycleOperationInput{
		OperationID:       "provision-op-1",
		CorrelationID:     "provision-op-1",
		MessageID:         "provision-message-1",
		OrganizationID:    orgID,
		DeviceID:          deviceID,
		OperationType:     model.DeviceOperationTypeProvision,
		RequestedBy:       &userID,
		RequestPayload:    map[string]any{"video_cloud_devid": "video-device-1", "activity_id": "activity-1", "clip_public_key": "clip-key-1"},
		OutboxMessageType: string(channel.MessageTypeDeviceProvisionRequested),
		OutboxPayload: map[string]any{
			"org_id":            orgID,
			"account_device_id": deviceID,
			"video_cloud_devid": "video-device-1",
			"activity_id":       "activity-1",
			"clip_public_key":   "clip-key-1",
			"requested_by":      userID,
		},
		MetadataPatch: PendingProvisionMetadata("video-device-1", "activity-1", "clip-key-1"),
		Now:           time.Now().UTC().Truncate(time.Microsecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created {
		t.Fatal("expected provisioning operation to be created")
	}
	if got := result.Device.Metadata["location"]; got != "lab" {
		t.Fatalf("expected unrelated metadata to remain, got %+v", got)
	}
	if got := result.Device.Metadata[model.DeviceMetadataVideoCloudDevid]; got != "video-device-1" {
		t.Fatalf("expected pending metadata to expose requested devid, got %+v", got)
	}
	if got := result.Device.Metadata[model.DeviceMetadataVideoCloudActivityID]; got != "activity-1" {
		t.Fatalf("expected pending metadata to expose requested activity id, got %+v", got)
	}
	if got := result.Device.Metadata[model.DeviceMetadataVideoCloudClipPublicKey]; got != "clip-key-1" {
		t.Fatalf("expected pending metadata to expose requested clip public key, got %+v", got)
	}
	if got := result.Device.Metadata[model.DeviceMetadataVideoCloudActivationStatus]; got != string(model.VideoCloudActivationStatusPending) {
		t.Fatalf("expected pending activation status, got %+v", got)
	}
	if _, exists := result.Device.Metadata[model.DeviceMetadataVideoCloudActivatedAt]; exists {
		t.Fatalf("expected pending metadata to clear activated_at, got %+v", result.Device.Metadata[model.DeviceMetadataVideoCloudActivatedAt])
	}
	if _, exists := result.Device.Metadata[model.DeviceMetadataVideoCloudDeactivatedAt]; exists {
		t.Fatalf("expected pending metadata to clear deactivated_at, got %+v", result.Device.Metadata[model.DeviceMetadataVideoCloudDeactivatedAt])
	}
	if _, exists := result.Device.Metadata[model.DeviceMetadataVideoCloudLastError]; exists {
		t.Fatalf("expected pending metadata to clear last_error, got %+v", result.Device.Metadata[model.DeviceMetadataVideoCloudLastError])
	}

	persisted, err := env.store.GetDevice(ctx, orgID, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.Metadata[model.DeviceMetadataVideoCloudActivationStatus]; got != string(model.VideoCloudActivationStatusPending) {
		t.Fatalf("expected persisted pending activation status, got %+v", got)
	}
}

func TestStartDeviceDeactivationOperationUsesProjectedMetadata(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	orgID, userID, deviceID := createDeviceFixture(t, env)

	ctx := context.Background()
	if _, err := env.db.Exec(ctx, `
		UPDATE devices
		SET metadata = metadata || jsonb_build_object('video_cloud_devid', 'video-device-1')
		WHERE id = $1
	`, deviceID); err != nil {
		t.Fatal(err)
	}
	if err := env.store.DeleteDevice(ctx, orgID, deviceID); err != nil {
		t.Fatal(err)
	}

	result, err := env.store.StartDeviceDeactivationOperation(ctx, DeviceDeactivationOperationInput{
		OperationID:    "deactivate-op-1",
		CorrelationID:  "deactivate-op-1",
		MessageID:      "deactivate-message-1",
		OrganizationID: orgID,
		DeviceID:       deviceID,
		RequestedBy:    &userID,
		Reason:         "user_request",
		Now:            time.Now().UTC().Truncate(time.Microsecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created {
		t.Fatal("expected deactivation operation to be created")
	}
	if result.Operation.OperationType != model.DeviceOperationTypeDeactivate {
		t.Fatalf("expected deactivate operation type, got %s", result.Operation.OperationType)
	}
	if got := result.Operation.RequestPayload["video_cloud_devid"]; got != "video-device-1" {
		t.Fatalf("expected request payload to use projected video metadata, got %+v", result.Operation.RequestPayload)
	}
	if got := result.Operation.RequestPayload["reason"]; got != "user_request" {
		t.Fatalf("expected deactivation reason in request payload, got %+v", result.Operation.RequestPayload)
	}
	if got := result.Message.Payload["video_cloud_devid"]; got != "video-device-1" {
		t.Fatalf("expected outbox payload to use projected video metadata, got %+v", result.Message.Payload)
	}
	if got := result.Message.Payload["reason"]; got != "user_request" {
		t.Fatalf("expected outbox payload to keep deactivation reason, got %+v", result.Message.Payload)
	}
	if got := result.Message.Payload["requested_by"]; got != userID {
		t.Fatalf("expected outbox payload to keep requester, got %+v", result.Message.Payload)
	}
}

func TestStartDeviceDeactivationOperationRejectsMissingProjectedMetadata(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	orgID, userID, deviceID := createDeviceFixture(t, env)

	_, err := env.store.StartDeviceDeactivationOperation(context.Background(), DeviceDeactivationOperationInput{
		OperationID:    "deactivate-op-missing-metadata",
		CorrelationID:  "deactivate-op-missing-metadata",
		MessageID:      "deactivate-message-missing-metadata",
		OrganizationID: orgID,
		DeviceID:       deviceID,
		RequestedBy:    &userID,
		Reason:         "user_request",
		Now:            time.Now().UTC().Truncate(time.Microsecond),
	})
	if !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("expected ErrNotProvisioned, got %v", err)
	}
}
