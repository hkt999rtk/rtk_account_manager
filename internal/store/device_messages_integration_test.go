package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/model"
)

type storeIntegrationEnv struct {
	store *Store
	db    *pgxpool.Pool
}

func newStoreIntegrationEnv(t *testing.T) storeIntegrationEnv {
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

	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		TRUNCATE device_message_inbox, device_message_outbox, device_operations, refresh_tokens, devices, organization_members, organizations, users
		RESTART IDENTITY CASCADE
	`); err != nil {
		t.Fatal(err)
	}

	return storeIntegrationEnv{store: New(db), db: db}
}

func createDeviceFixture(t *testing.T, env storeIntegrationEnv) (string, string, string) {
	t.Helper()

	ctx := context.Background()
	registered, err := env.store.Register(ctx, RegisterInput{
		Email:            "owner@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Owner Org",
	})
	if err != nil {
		t.Fatal(err)
	}

	device, err := env.store.CreateDevice(ctx, registered.Organization.ID, DeviceInput{
		Name:     "Camera 1",
		Category: model.DeviceCategoryIPCamera,
		Metadata: map[string]any{"location": "lab"},
	})
	if err != nil {
		t.Fatal(err)
	}

	return registered.Organization.ID, registered.User.ID, device.ID
}

func TestCreateOrGetDeviceOperationIsIdempotent(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	orgID, userID, deviceID := createDeviceFixture(t, env)

	ctx := context.Background()
	createdAt := time.Now().UTC().Truncate(time.Microsecond)
	op, created, err := env.store.CreateOrGetDeviceOperation(ctx, DeviceOperationCreateInput{
		OperationID:    "op-1",
		CorrelationID:  "corr-1",
		OrganizationID: orgID,
		DeviceID:       deviceID,
		OperationType:  model.DeviceOperationTypeProvision,
		Status:         model.DeviceOperationStatusPending,
		RequestedBy:    &userID,
		RequestPayload: map[string]any{"video_cloud_devid": "device-1"},
		ResultPayload:  map[string]any{},
		CompletedAt:    nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected first operation insert to create a row")
	}
	if op.OperationID != "op-1" || op.Status != model.DeviceOperationStatusPending {
		t.Fatalf("unexpected operation: %+v", op)
	}

	same, created, err := env.store.CreateOrGetDeviceOperation(ctx, DeviceOperationCreateInput{
		OperationID:    "op-1",
		CorrelationID:  "corr-1",
		OrganizationID: orgID,
		DeviceID:       deviceID,
		OperationType:  model.DeviceOperationTypeProvision,
		Status:         model.DeviceOperationStatusPending,
		RequestedBy:    &userID,
		RequestPayload: map[string]any{"video_cloud_devid": "device-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("expected duplicate operation_id to reuse the existing row")
	}
	if same.ID != op.ID {
		t.Fatalf("expected same operation row, got %q want %q", same.ID, op.ID)
	}

	conflicting, created, err := env.store.CreateOrGetDeviceOperation(ctx, DeviceOperationCreateInput{
		OperationID:    "op-1",
		CorrelationID:  "corr-1",
		OrganizationID: orgID,
		DeviceID:       deviceID,
		OperationType:  model.DeviceOperationTypeProvision,
		Status:         model.DeviceOperationStatusPending,
		RequestedBy:    &userID,
		RequestPayload: map[string]any{"video_cloud_devid": "device-2"},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict error, got row=%+v created=%v err=%v", conflicting, created, err)
	}

	completedAt := createdAt.Add(5 * time.Minute)
	result, err := env.store.UpdateDeviceOperation(ctx, op.OperationID, DeviceOperationUpdateInput{
		Status:        model.DeviceOperationStatusSucceeded,
		ResultPayload: map[string]any{"video_cloud_devid": "device-1"},
		CompletedAt:   &completedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != model.DeviceOperationStatusSucceeded {
		t.Fatalf("expected succeeded status, got %s", result.Status)
	}
	if result.CompletedAt == nil || !result.CompletedAt.Equal(completedAt) {
		t.Fatalf("expected completed_at to be set, got %+v", result.CompletedAt)
	}
	if result.UpdatedAt.Before(op.UpdatedAt) {
		t.Fatalf("expected updated_at to stay monotonic, before=%s after=%s", op.UpdatedAt, result.UpdatedAt)
	}
}

func TestOutboxMessagePersistenceAndReadyList(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	orgID, userID, deviceID := createDeviceFixture(t, env)

	ctx := context.Background()
	op, _, err := env.store.CreateOrGetDeviceOperation(ctx, DeviceOperationCreateInput{
		OperationID:    "op-outbox",
		CorrelationID:  "corr-outbox",
		OrganizationID: orgID,
		DeviceID:       deviceID,
		OperationType:  model.DeviceOperationTypeProvision,
		Status:         model.DeviceOperationStatusPending,
		RequestedBy:    &userID,
		RequestPayload: map[string]any{"video_cloud_devid": "device-1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	first, err := env.store.CreateOutboxMessage(ctx, DeviceMessageOutboxCreateInput{
		MessageID:     "msg-1",
		OperationID:   op.OperationID,
		CorrelationID: op.CorrelationID,
		Stream:        "account.video.commands",
		MessageType:   "DeviceProvisionRequested",
		SchemaVersion: "1.0",
		PartitionKey:  deviceID,
		Payload:       map[string]any{"operation_id": op.OperationID},
		Status:        model.DeviceMessageOutboxStatusPending,
		AvailableAt:   now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != model.DeviceMessageOutboxStatusPending {
		t.Fatalf("unexpected outbox status: %+v", first)
	}

	if _, err := env.store.CreateOutboxMessage(ctx, DeviceMessageOutboxCreateInput{
		MessageID:     "msg-2",
		OperationID:   op.OperationID,
		CorrelationID: op.CorrelationID,
		Stream:        "account.video.commands",
		MessageType:   "DeviceDeactivateRequested",
		SchemaVersion: "1.0",
		PartitionKey:  deviceID,
		Payload:       map[string]any{"operation_id": op.OperationID},
		Status:        model.DeviceMessageOutboxStatusRetrying,
		AttemptCount:  2,
		LastError:     stringPtr("temporary failure"),
		AvailableAt:   now.Add(2 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	ready, err := env.store.ListOutboxMessagesReady(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].MessageID != "msg-1" {
		t.Fatalf("expected only ready msg-1, got %+v", ready)
	}

	publishedAt := now.Add(time.Second)
	published, err := env.store.UpdateOutboxMessage(ctx, "msg-1", DeviceMessageOutboxUpdateInput{
		Status:       model.DeviceMessageOutboxStatusPublished,
		AttemptCount: 1,
		AvailableAt:  first.AvailableAt,
		PublishedAt:  &publishedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if published.PublishedAt == nil || !published.PublishedAt.Equal(publishedAt) {
		t.Fatalf("expected published_at to be set, got %+v", published.PublishedAt)
	}
	if published.Status != model.DeviceMessageOutboxStatusPublished {
		t.Fatalf("expected published status, got %s", published.Status)
	}
}

func TestCreateOrGetInboxMessageDeduplicates(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	orgID, userID, deviceID := createDeviceFixture(t, env)

	ctx := context.Background()
	_, _, err := env.store.CreateOrGetDeviceOperation(ctx, DeviceOperationCreateInput{
		OperationID:    "op-inbox",
		CorrelationID:  "corr-inbox",
		OrganizationID: orgID,
		DeviceID:       deviceID,
		OperationType:  model.DeviceOperationTypeProvision,
		Status:         model.DeviceOperationStatusPublished,
		RequestedBy:    &userID,
		RequestPayload: map[string]any{"video_cloud_devid": "device-1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	receivedAt := time.Now().UTC().Truncate(time.Microsecond)
	message, created, err := env.store.CreateOrGetInboxMessage(ctx, DeviceMessageInboxCreateInput{
		MessageID:     "evt-1",
		OperationID:   "op-inbox",
		CorrelationID: "corr-inbox",
		Stream:        "video.account.events",
		MessageType:   "DeviceProvisionSucceeded",
		SchemaVersion: "1.0",
		PartitionKey:  deviceID,
		Payload:       map[string]any{"video_cloud_devid": "device-1"},
		Status:        model.DeviceMessageInboxStatusRetrying,
		AttemptCount:  1,
		LastError:     stringPtr("projection retry"),
		ReceivedAt:    receivedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected first inbox insert to create a row")
	}

	same, created, err := env.store.CreateOrGetInboxMessage(ctx, DeviceMessageInboxCreateInput{
		MessageID:     "evt-1",
		OperationID:   "op-inbox",
		CorrelationID: "corr-inbox",
		Stream:        "video.account.events",
		MessageType:   "DeviceProvisionSucceeded",
		SchemaVersion: "1.0",
		PartitionKey:  deviceID,
		Payload:       map[string]any{"video_cloud_devid": "device-1"},
		Status:        model.DeviceMessageInboxStatusRetrying,
		AttemptCount:  1,
		LastError:     stringPtr("projection retry"),
		ReceivedAt:    receivedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created || same.ID != message.ID {
		t.Fatalf("expected duplicate message_id to reuse existing row, got created=%v message=%+v", created, same)
	}

	_, created, err = env.store.CreateOrGetInboxMessage(ctx, DeviceMessageInboxCreateInput{
		MessageID:     "evt-1",
		OperationID:   "op-inbox",
		CorrelationID: "corr-inbox",
		Stream:        "video.account.events",
		MessageType:   "DeviceProvisionSucceeded",
		SchemaVersion: "1.0",
		PartitionKey:  deviceID,
		Payload:       map[string]any{"video_cloud_devid": "device-2"},
		Status:        model.DeviceMessageInboxStatusRetrying,
		AttemptCount:  1,
		LastError:     stringPtr("projection retry"),
		ReceivedAt:    receivedAt,
	})
	if !errors.Is(err, ErrConflict) || created {
		t.Fatalf("expected conflicting inbox payload to fail with conflict, got created=%v err=%v", created, err)
	}

	processedAt := receivedAt.Add(3 * time.Minute)
	processed, err := env.store.UpdateInboxMessage(ctx, "evt-1", DeviceMessageInboxUpdateInput{
		Status:       model.DeviceMessageInboxStatusProcessed,
		AttemptCount: 2,
		ProcessedAt:  &processedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if processed.ProcessedAt == nil || !processed.ProcessedAt.Equal(processedAt) {
		t.Fatalf("expected processed_at to be set, got %+v", processed.ProcessedAt)
	}
	if processed.Status != model.DeviceMessageInboxStatusProcessed {
		t.Fatalf("expected processed status, got %s", processed.Status)
	}
}

func stringPtr(value string) *string {
	return &value
}
