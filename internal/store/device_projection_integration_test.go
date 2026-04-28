package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/testutil"
)

type projectionIntegrationEnv struct {
	db    *pgxpool.Pool
	store *Store
}

func newProjectionIntegrationEnv(t *testing.T) projectionIntegrationEnv {
	t.Helper()

	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := database.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)

	testutil.LockIntegrationDatabase(t, db)

	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		TRUNCATE device_message_inbox, device_message_outbox, device_operations, refresh_tokens, devices, organization_members, organizations, users
		RESTART IDENTITY CASCADE
	`); err != nil {
		t.Fatal(err)
	}

	return projectionIntegrationEnv{db: db, store: New(db)}
}

func createProjectionDevice(t *testing.T, env projectionIntegrationEnv, metadata map[string]any) (RegisterResult, model.Device) {
	t.Helper()

	ctx := context.Background()
	registered, err := env.store.Register(ctx, RegisterInput{
		Email:            "owner@example.com",
		PasswordHash:     "hashed-password",
		OrganizationName: "Owner Org",
	})
	if err != nil {
		t.Fatal(err)
	}

	device, err := env.store.CreateDevice(ctx, registered.Organization.ID, DeviceInput{
		Name:         "camera-1",
		Category:     model.DeviceCategoryIPCamera,
		SerialNumber: strPtr("SERIAL-001"),
		Metadata:     metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	return registered, device
}

func TestMergeDeviceMetadataPreservesUnrelatedFields(t *testing.T) {
	env := newProjectionIntegrationEnv(t)
	registered, device := createProjectionDevice(t, env, map[string]any{
		"location": "lab",
		model.DeviceMetadataVideoCloudLastError: map[string]any{
			"code": "stale",
		},
	})

	merged, err := env.store.MergeDeviceMetadata(context.Background(), registered.Organization.ID, device.ID, map[string]any{
		model.DeviceMetadataVideoCloudDevid:            "video-001",
		model.DeviceMetadataVideoCloudActivationStatus: model.VideoCloudActivationStatusActivated,
		model.DeviceMetadataVideoCloudLastError:        nil,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := merged.Metadata["location"]; got != "lab" {
		t.Fatalf("expected unrelated metadata to remain, got %+v", got)
	}
	if got := merged.Metadata[model.DeviceMetadataVideoCloudDevid]; got != "video-001" {
		t.Fatalf("expected merged devid, got %+v", got)
	}
	if got := merged.Metadata[model.DeviceMetadataVideoCloudActivationStatus]; got != string(model.VideoCloudActivationStatusActivated) {
		t.Fatalf("expected activation metadata, got %+v", got)
	}
	if _, exists := merged.Metadata[model.DeviceMetadataVideoCloudLastError]; exists {
		t.Fatalf("expected last_error to be cleared, got %+v", merged.Metadata[model.DeviceMetadataVideoCloudLastError])
	}
	if merged.Status != model.DeviceStatusUnknown {
		t.Fatalf("expected status to remain unknown, got %s", merged.Status)
	}
}

func TestProjectDeviceProvisioningAndOnlineRules(t *testing.T) {
	env := newProjectionIntegrationEnv(t)
	registered, device := createProjectionDevice(t, env, map[string]any{"location": "rack-a"})

	failedAt := time.Date(2026, 4, 28, 9, 30, 0, 0, time.UTC)
	failed, err := env.store.ProjectDevice(context.Background(), registered.Organization.ID, device.ID, ProvisionFailedProjection(channel.DeviceProvisionFailedPayload{
		VideoCloudDevid: "video-002",
		ActivityID:      "activity-1",
		ErrorCode:       "upstream_timeout",
		ErrorMessage:    "worker timed out",
		Retryable:       true,
		FailedAt:        failedAt,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != model.DeviceStatusUnknown {
		t.Fatalf("expected failed projection to leave device status unchanged, got %s", failed.Status)
	}
	if got := failed.Metadata[model.DeviceMetadataVideoCloudActivationStatus]; got != string(model.VideoCloudActivationStatusFailed) {
		t.Fatalf("expected failed activation metadata, got %+v", got)
	}

	succeeded, err := env.store.ProjectDevice(context.Background(), registered.Organization.ID, device.ID, ProvisionSucceededProjection(channel.DeviceProvisionSucceededPayload{
		VideoCloudDevid: "video-002",
		ActivityID:      "activity-1",
		ActivatedAt:     time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if succeeded.Status != model.DeviceStatusUnknown {
		t.Fatalf("expected provision success not to set account status online, got %s", succeeded.Status)
	}
	if _, exists := succeeded.Metadata[model.DeviceMetadataVideoCloudLastError]; exists {
		t.Fatalf("expected success projection to clear last_error, got %+v", succeeded.Metadata[model.DeviceMetadataVideoCloudLastError])
	}
	if got := succeeded.Metadata["location"]; got != "rack-a" {
		t.Fatalf("expected unrelated metadata to remain, got %+v", got)
	}

	lastSeenAt := time.Date(2026, 4, 28, 11, 15, 0, 0, time.FixedZone("CST", 8*60*60))
	online, err := env.store.ProjectDevice(context.Background(), registered.Organization.ID, device.ID, OnlineChangedProjection(channel.DeviceOnlineChangedPayload{
		VideoCloudDevid: "video-002",
		Status:          channel.OnlineStatusOnline,
		LastSeenAt:      lastSeenAt,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if online.Status != model.DeviceStatusOnline {
		t.Fatalf("expected online projection to update status, got %s", online.Status)
	}
	if online.LastSeenAt == nil || !online.LastSeenAt.Equal(lastSeenAt.UTC()) {
		t.Fatalf("expected online projection to update last_seen_at, got %+v", online.LastSeenAt)
	}
}

func TestProjectDeviceRejectsDisabledDevicesExceptDeactivateResults(t *testing.T) {
	env := newProjectionIntegrationEnv(t)
	registered, device := createProjectionDevice(t, env, map[string]any{"location": "rack-b"})

	if err := env.store.DeleteDevice(context.Background(), registered.Organization.ID, device.ID); err != nil {
		t.Fatal(err)
	}

	_, err := env.store.ProjectDevice(context.Background(), registered.Organization.ID, device.ID, ProvisionSucceededProjection(channel.DeviceProvisionSucceededPayload{
		VideoCloudDevid: "video-disabled",
		ActivityID:      "activity-disabled",
		ActivatedAt:     time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC),
	}))
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected disabled error for non-deactivation projection, got %v", err)
	}

	projected, err := env.store.ProjectDevice(context.Background(), registered.Organization.ID, device.ID, DeactivateSucceededProjection(channel.DeviceDeactivateSucceededPayload{
		VideoCloudDevid: "video-disabled",
		DeactivatedAt:   time.Date(2026, 4, 28, 12, 30, 0, 0, time.UTC),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if projected.Status != model.DeviceStatusDisabled {
		t.Fatalf("expected disabled device status to remain disabled, got %s", projected.Status)
	}
	if got := projected.Metadata[model.DeviceMetadataVideoCloudActivationStatus]; got != string(model.VideoCloudActivationStatusDeactivated) {
		t.Fatalf("expected deactivated metadata, got %+v", got)
	}
	if got := projected.Metadata["location"]; got != "rack-b" {
		t.Fatalf("expected unrelated metadata to remain, got %+v", got)
	}
}

func strPtr(value string) *string {
	return &value
}
