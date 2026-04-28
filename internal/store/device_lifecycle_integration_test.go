package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

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
