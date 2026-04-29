package store

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/testutil"
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

	conflictingCorrelation, created, err := env.store.CreateOrGetDeviceOperation(ctx, DeviceOperationCreateInput{
		OperationID:    "op-1",
		CorrelationID:  "corr-2",
		OrganizationID: orgID,
		DeviceID:       deviceID,
		OperationType:  model.DeviceOperationTypeProvision,
		Status:         model.DeviceOperationStatusPending,
		RequestedBy:    &userID,
		RequestPayload: map[string]any{"video_cloud_devid": "device-1"},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected correlation conflict error, got row=%+v created=%v err=%v", conflictingCorrelation, created, err)
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

	stored, err := env.store.GetDeviceOperation(ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != op.ID || stored.Status != model.DeviceOperationStatusSucceeded {
		t.Fatalf("unexpected stored operation: %+v", stored)
	}

	if _, err := env.store.GetDeviceOperation(ctx, "missing-operation"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing operation to return ErrNotFound, got %v", err)
	}
	if _, err := env.store.UpdateDeviceOperation(ctx, "missing-operation", DeviceOperationUpdateInput{
		Status: model.DeviceOperationStatusFailed,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing operation update to return ErrNotFound, got %v", err)
	}
}

func TestDeviceMessagePersistenceRejectsInvalidSchemaValues(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	orgID, userID, deviceID := createDeviceFixture(t, env)

	ctx := context.Background()
	_, _, err := env.store.CreateOrGetDeviceOperation(ctx, DeviceOperationCreateInput{
		OperationID:    "op-invalid-status",
		CorrelationID:  "corr-invalid-status",
		OrganizationID: orgID,
		DeviceID:       deviceID,
		OperationType:  model.DeviceOperationTypeProvision,
		Status:         model.DeviceOperationStatus("unknown-status"),
		RequestedBy:    &userID,
		RequestPayload: map[string]any{"video_cloud_devid": "device-1"},
	})
	requirePGErrorCode(t, err, "23514")

	op, _, err := env.store.CreateOrGetDeviceOperation(ctx, DeviceOperationCreateInput{
		OperationID:    "op-schema-checks",
		CorrelationID:  "corr-schema-checks",
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
	_, err = env.store.CreateOutboxMessage(ctx, DeviceMessageOutboxCreateInput{
		MessageID:     "missing-op-message",
		OperationID:   "missing-operation",
		CorrelationID: op.CorrelationID,
		Stream:        "account.video.commands",
		MessageType:   "DeviceProvisionRequested",
		SchemaVersion: "1.0",
		PartitionKey:  deviceID,
		Payload:       map[string]any{"operation_id": "missing-operation"},
		Status:        model.DeviceMessageOutboxStatusPending,
		AvailableAt:   now,
	})
	requirePGErrorCode(t, err, "23503")

	_, err = env.store.CreateOutboxMessage(ctx, DeviceMessageOutboxCreateInput{
		MessageID:     "blank-partition-key",
		OperationID:   op.OperationID,
		CorrelationID: op.CorrelationID,
		Stream:        "account.video.commands",
		MessageType:   "DeviceProvisionRequested",
		SchemaVersion: "1.0",
		PartitionKey:  "   ",
		Payload:       map[string]any{"operation_id": op.OperationID},
		Status:        model.DeviceMessageOutboxStatusPending,
		AvailableAt:   now,
	})
	requirePGErrorCode(t, err, "23514")

	_, _, err = env.store.CreateOrGetInboxMessage(ctx, DeviceMessageInboxCreateInput{
		MessageID:     "invalid-inbox-type",
		OperationID:   op.OperationID,
		CorrelationID: op.CorrelationID,
		Stream:        "video.account.events",
		MessageType:   "UnknownMessageType",
		SchemaVersion: "1.0",
		PartitionKey:  deviceID,
		Payload:       map[string]any{"video_cloud_devid": "device-1"},
		Status:        model.DeviceMessageInboxStatusRetrying,
		ReceivedAt:    now,
	})
	requirePGErrorCode(t, err, "23514")
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

	stored, err := env.store.GetOutboxMessage(ctx, first.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != first.ID || stored.Status != model.DeviceMessageOutboxStatusPublished {
		t.Fatalf("unexpected stored outbox message: %+v", stored)
	}

	if _, err := env.store.GetOutboxMessage(ctx, "missing-message"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing outbox message to return ErrNotFound, got %v", err)
	}
	if _, err := env.store.UpdateOutboxMessage(ctx, "missing-message", DeviceMessageOutboxUpdateInput{
		Status:      model.DeviceMessageOutboxStatusRetrying,
		AvailableAt: now,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing outbox update to return ErrNotFound, got %v", err)
	}
}

func TestClaimOutboxMessagesReadyLeasesRows(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	orgID, userID, deviceID := createDeviceFixture(t, env)

	ctx := context.Background()
	op, _, err := env.store.CreateOrGetDeviceOperation(ctx, DeviceOperationCreateInput{
		OperationID:    "op-claim",
		CorrelationID:  "corr-claim",
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
	claimUntil := now.Add(5 * time.Minute)
	for _, message := range []DeviceMessageOutboxCreateInput{
		{
			MessageID:     "claim-msg-1",
			OperationID:   op.OperationID,
			CorrelationID: op.CorrelationID,
			Stream:        "account.video.commands",
			MessageType:   "DeviceProvisionRequested",
			SchemaVersion: "1.0",
			PartitionKey:  deviceID,
			Payload:       map[string]any{"operation_id": op.OperationID},
			Status:        model.DeviceMessageOutboxStatusPending,
			AvailableAt:   now.Add(-2 * time.Minute),
		},
		{
			MessageID:     "claim-msg-2",
			OperationID:   op.OperationID,
			CorrelationID: op.CorrelationID,
			Stream:        "account.video.commands",
			MessageType:   "DeviceDeactivateRequested",
			SchemaVersion: "1.0",
			PartitionKey:  deviceID,
			Payload:       map[string]any{"operation_id": op.OperationID},
			Status:        model.DeviceMessageOutboxStatusRetrying,
			AttemptCount:  1,
			LastError:     stringPtr("temporary failure"),
			AvailableAt:   now.Add(-time.Minute),
		},
		{
			MessageID:     "claim-msg-3",
			OperationID:   op.OperationID,
			CorrelationID: op.CorrelationID,
			Stream:        "account.video.commands",
			MessageType:   "DeviceProvisionRequested",
			SchemaVersion: "1.0",
			PartitionKey:  deviceID,
			Payload:       map[string]any{"operation_id": op.OperationID},
			Status:        model.DeviceMessageOutboxStatusPending,
			AvailableAt:   now.Add(time.Minute),
		},
	} {
		if _, err := env.store.CreateOutboxMessage(ctx, message); err != nil {
			t.Fatal(err)
		}
	}

	claimed, err := env.store.ClaimOutboxMessagesReady(ctx, now, claimUntil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 {
		t.Fatalf("expected two claimed rows, got %+v", claimed)
	}
	if claimed[0].MessageID != "claim-msg-1" || claimed[1].MessageID != "claim-msg-2" {
		t.Fatalf("expected oldest ready rows to be claimed, got %+v", claimed)
	}
	for _, message := range claimed {
		if !message.AvailableAt.Equal(claimUntil) {
			t.Fatalf("expected claimed row to be leased until %s, got %+v", claimUntil, message)
		}
	}

	ready, err := env.store.ListOutboxMessagesReady(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 0 {
		t.Fatalf("expected no rows to remain ready after claim, got %+v", ready)
	}

	claimed, err = env.store.ClaimOutboxMessagesReady(ctx, now, claimUntil.Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("expected leased rows to stay hidden until available_at, got %+v", claimed)
	}
}

func TestRecordOutboxPublishTransitionUpdatesOperationState(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	orgID, userID, deviceID := createDeviceFixture(t, env)

	ctx := context.Background()
	op, _, err := env.store.CreateOrGetDeviceOperation(ctx, DeviceOperationCreateInput{
		OperationID:    "op-transition",
		CorrelationID:  "corr-transition",
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
	if _, err := env.store.CreateOutboxMessage(ctx, DeviceMessageOutboxCreateInput{
		MessageID:     "transition-msg",
		OperationID:   op.OperationID,
		CorrelationID: op.CorrelationID,
		Stream:        "account.video.commands",
		MessageType:   "DeviceProvisionRequested",
		SchemaVersion: "1.0",
		PartitionKey:  deviceID,
		Payload:       map[string]any{"operation_id": op.OperationID},
		Status:        model.DeviceMessageOutboxStatusPending,
		AvailableAt:   now,
	}); err != nil {
		t.Fatal(err)
	}

	deadLetterAt := now.Add(time.Minute)
	retryable := false
	result, err := env.store.RecordOutboxPublishTransition(ctx, OutboxPublishTransitionInput{
		MessageID:             "transition-msg",
		ExpectedMessageStatus: model.DeviceMessageOutboxStatusPending,
		ExpectedAttemptCount:  0,
		ExpectedAvailableAt:   now,
		MessageStatus:         model.DeviceMessageOutboxStatusDeadLettered,
		AttemptCount:          3,
		LastError:             stringPtr("publish failed"),
		AvailableAt:           deadLetterAt,
		OperationStatus:       model.DeviceOperationStatusDeadLettered,
		OperationErrorCode:    stringPtr("publish_failed"),
		OperationErrorMessage: stringPtr("publish failed"),
		OperationRetryable:    &retryable,
		OperationCompletedAt:  &deadLetterAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Message.Status != model.DeviceMessageOutboxStatusDeadLettered {
		t.Fatalf("expected outbox dead-letter status, got %s", result.Message.Status)
	}
	if result.Operation.Status != model.DeviceOperationStatusDeadLettered {
		t.Fatalf("expected operation dead-letter status, got %s", result.Operation.Status)
	}
	if result.Operation.CompletedAt == nil || !result.Operation.CompletedAt.Equal(deadLetterAt) {
		t.Fatalf("expected completed_at to be set, got %+v", result.Operation.CompletedAt)
	}
}

func TestRecordOutboxPublishTransitionRejectsStaleLease(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	orgID, userID, deviceID := createDeviceFixture(t, env)

	ctx := context.Background()
	op, _, err := env.store.CreateOrGetDeviceOperation(ctx, DeviceOperationCreateInput{
		OperationID:    "op-stale-transition",
		CorrelationID:  "corr-stale-transition",
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

	readyAt := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := env.store.CreateOutboxMessage(ctx, DeviceMessageOutboxCreateInput{
		MessageID:     "stale-transition-msg",
		OperationID:   op.OperationID,
		CorrelationID: op.CorrelationID,
		Stream:        "account.video.commands",
		MessageType:   "DeviceProvisionRequested",
		SchemaVersion: "1.0",
		PartitionKey:  deviceID,
		Payload:       map[string]any{"operation_id": op.OperationID},
		Status:        model.DeviceMessageOutboxStatusPending,
		AvailableAt:   readyAt,
	}); err != nil {
		t.Fatal(err)
	}

	firstLeaseUntil := readyAt.Add(30 * time.Second)
	firstClaim, err := env.store.ClaimOutboxMessagesReady(ctx, readyAt, firstLeaseUntil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstClaim) != 1 {
		t.Fatalf("expected one first claim, got %+v", firstClaim)
	}

	secondLeaseUntil := firstLeaseUntil.Add(30 * time.Second)
	secondClaim, err := env.store.ClaimOutboxMessagesReady(ctx, firstLeaseUntil, secondLeaseUntil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondClaim) != 1 {
		t.Fatalf("expected lease-expired row to be reclaimable, got %+v", secondClaim)
	}

	publishedAt := secondLeaseUntil.Add(time.Second)
	result, err := env.store.RecordOutboxPublishTransition(ctx, OutboxPublishTransitionInput{
		MessageID:             secondClaim[0].MessageID,
		ExpectedMessageStatus: secondClaim[0].Status,
		ExpectedAttemptCount:  secondClaim[0].AttemptCount,
		ExpectedAvailableAt:   secondClaim[0].AvailableAt,
		MessageStatus:         model.DeviceMessageOutboxStatusPublished,
		AttemptCount:          secondClaim[0].AttemptCount + 1,
		AvailableAt:           publishedAt,
		PublishedAt:           &publishedAt,
		OperationStatus:       model.DeviceOperationStatusPublished,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Message.Status != model.DeviceMessageOutboxStatusPublished {
		t.Fatalf("expected published result, got %+v", result.Message)
	}

	retryable := false
	_, staleErr := env.store.RecordOutboxPublishTransition(ctx, OutboxPublishTransitionInput{
		MessageID:             firstClaim[0].MessageID,
		ExpectedMessageStatus: firstClaim[0].Status,
		ExpectedAttemptCount:  firstClaim[0].AttemptCount,
		ExpectedAvailableAt:   firstClaim[0].AvailableAt,
		MessageStatus:         model.DeviceMessageOutboxStatusDeadLettered,
		AttemptCount:          firstClaim[0].AttemptCount + 1,
		LastError:             stringPtr("stale worker lost lease"),
		AvailableAt:           publishedAt,
		OperationStatus:       model.DeviceOperationStatusDeadLettered,
		OperationErrorCode:    stringPtr("publish_failed"),
		OperationErrorMessage: stringPtr("stale worker lost lease"),
		OperationRetryable:    &retryable,
		OperationCompletedAt:  &publishedAt,
	})
	if !errors.Is(staleErr, ErrConflict) {
		t.Fatalf("expected stale lease conflict, got %v", staleErr)
	}

	storedMessage, err := env.store.GetOutboxMessage(ctx, "stale-transition-msg")
	if err != nil {
		t.Fatal(err)
	}
	if storedMessage.Status != model.DeviceMessageOutboxStatusPublished {
		t.Fatalf("expected published message to remain intact, got %+v", storedMessage)
	}
	if storedMessage.AttemptCount != 1 {
		t.Fatalf("expected published attempt count to stay 1, got %+v", storedMessage)
	}
	if storedMessage.PublishedAt == nil || !storedMessage.PublishedAt.Equal(publishedAt) {
		t.Fatalf("expected published timestamp to be preserved, got %+v", storedMessage.PublishedAt)
	}

	storedOperation, err := env.store.GetDeviceOperation(ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if storedOperation.Status != model.DeviceOperationStatusPublished {
		t.Fatalf("expected published operation to remain intact, got %+v", storedOperation)
	}
	if storedOperation.CompletedAt != nil {
		t.Fatalf("expected published operation to remain incomplete, got %+v", storedOperation.CompletedAt)
	}
}

func TestRecordOutboxPublishTransitionLetsPublishedOutcomeOverrideLaterFailure(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	orgID, userID, deviceID := createDeviceFixture(t, env)

	ctx := context.Background()
	op, _, err := env.store.CreateOrGetDeviceOperation(ctx, DeviceOperationCreateInput{
		OperationID:    "op-stale-published",
		CorrelationID:  "corr-stale-published",
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

	readyAt := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := env.store.CreateOutboxMessage(ctx, DeviceMessageOutboxCreateInput{
		MessageID:     "stale-published-msg",
		OperationID:   op.OperationID,
		CorrelationID: op.CorrelationID,
		Stream:        "account.video.commands",
		MessageType:   "DeviceProvisionRequested",
		SchemaVersion: "1.0",
		PartitionKey:  deviceID,
		Payload:       map[string]any{"operation_id": op.OperationID},
		Status:        model.DeviceMessageOutboxStatusPending,
		AvailableAt:   readyAt,
	}); err != nil {
		t.Fatal(err)
	}

	firstLeaseUntil := readyAt.Add(30 * time.Second)
	firstClaim, err := env.store.ClaimOutboxMessagesReady(ctx, readyAt, firstLeaseUntil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstClaim) != 1 {
		t.Fatalf("expected one first claim, got %+v", firstClaim)
	}

	secondLeaseUntil := firstLeaseUntil.Add(30 * time.Second)
	secondClaim, err := env.store.ClaimOutboxMessagesReady(ctx, firstLeaseUntil, secondLeaseUntil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondClaim) != 1 {
		t.Fatalf("expected lease-expired row to be reclaimable, got %+v", secondClaim)
	}

	publishedAt := firstLeaseUntil.Add(time.Second)
	result, err := env.store.RecordOutboxPublishTransition(ctx, OutboxPublishTransitionInput{
		MessageID:             firstClaim[0].MessageID,
		ExpectedMessageStatus: firstClaim[0].Status,
		ExpectedAttemptCount:  firstClaim[0].AttemptCount,
		ExpectedAvailableAt:   firstClaim[0].AvailableAt,
		MessageStatus:         model.DeviceMessageOutboxStatusPublished,
		AttemptCount:          firstClaim[0].AttemptCount + 1,
		AvailableAt:           publishedAt,
		PublishedAt:           &publishedAt,
		OperationStatus:       model.DeviceOperationStatusPublished,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Message.Status != model.DeviceMessageOutboxStatusPublished {
		t.Fatalf("expected published result, got %+v", result.Message)
	}

	retryable := false
	_, conflictErr := env.store.RecordOutboxPublishTransition(ctx, OutboxPublishTransitionInput{
		MessageID:             secondClaim[0].MessageID,
		ExpectedMessageStatus: secondClaim[0].Status,
		ExpectedAttemptCount:  secondClaim[0].AttemptCount,
		ExpectedAvailableAt:   secondClaim[0].AvailableAt,
		MessageStatus:         model.DeviceMessageOutboxStatusDeadLettered,
		AttemptCount:          secondClaim[0].AttemptCount + 1,
		LastError:             stringPtr("duplicate claimant observed publish failure"),
		AvailableAt:           secondLeaseUntil.Add(time.Second),
		OperationStatus:       model.DeviceOperationStatusDeadLettered,
		OperationErrorCode:    stringPtr("publish_failed"),
		OperationErrorMessage: stringPtr("duplicate claimant observed publish failure"),
		OperationRetryable:    &retryable,
		OperationCompletedAt:  &publishedAt,
	})
	if !errors.Is(conflictErr, ErrConflict) {
		t.Fatalf("expected later failure to conflict with published outcome, got %v", conflictErr)
	}

	storedMessage, err := env.store.GetOutboxMessage(ctx, "stale-published-msg")
	if err != nil {
		t.Fatal(err)
	}
	if storedMessage.Status != model.DeviceMessageOutboxStatusPublished {
		t.Fatalf("expected published message to remain intact, got %+v", storedMessage)
	}
	if storedMessage.PublishedAt == nil || !storedMessage.PublishedAt.Equal(publishedAt) {
		t.Fatalf("expected published timestamp to be preserved, got %+v", storedMessage.PublishedAt)
	}

	storedOperation, err := env.store.GetDeviceOperation(ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if storedOperation.Status != model.DeviceOperationStatusPublished {
		t.Fatalf("expected published operation to remain intact, got %+v", storedOperation)
	}
	if storedOperation.CompletedAt != nil {
		t.Fatalf("expected published operation to remain incomplete, got %+v", storedOperation.CompletedAt)
	}
}

func TestRecordOutboxPublishTransitionPreservesInboxCompletedOperation(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	orgID, userID, deviceID := createDeviceFixture(t, env)

	ctx := context.Background()
	op, _, err := env.store.CreateOrGetDeviceOperation(ctx, DeviceOperationCreateInput{
		OperationID:    "op-preserve-inbox-outcome",
		CorrelationID:  "corr-preserve-inbox-outcome",
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

	readyAt := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := env.store.CreateOutboxMessage(ctx, DeviceMessageOutboxCreateInput{
		MessageID:     "preserve-inbox-outcome-msg",
		OperationID:   op.OperationID,
		CorrelationID: op.CorrelationID,
		Stream:        "account.video.commands",
		MessageType:   "DeviceProvisionRequested",
		SchemaVersion: "1.0",
		PartitionKey:  deviceID,
		Payload:       map[string]any{"operation_id": op.OperationID},
		Status:        model.DeviceMessageOutboxStatusPending,
		AvailableAt:   readyAt,
	}); err != nil {
		t.Fatal(err)
	}

	firstLeaseUntil := readyAt.Add(30 * time.Second)
	firstClaim, err := env.store.ClaimOutboxMessagesReady(ctx, readyAt, firstLeaseUntil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstClaim) != 1 {
		t.Fatalf("expected one first claim, got %+v", firstClaim)
	}

	secondLeaseUntil := firstLeaseUntil.Add(30 * time.Second)
	secondClaim, err := env.store.ClaimOutboxMessagesReady(ctx, firstLeaseUntil, secondLeaseUntil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondClaim) != 1 {
		t.Fatalf("expected lease-expired row to be reclaimable, got %+v", secondClaim)
	}

	deadLetterAt := secondLeaseUntil.Add(time.Second)
	retryable := false
	if _, err := env.store.RecordOutboxPublishTransition(ctx, OutboxPublishTransitionInput{
		MessageID:             secondClaim[0].MessageID,
		ExpectedMessageStatus: secondClaim[0].Status,
		ExpectedAttemptCount:  secondClaim[0].AttemptCount,
		ExpectedAvailableAt:   secondClaim[0].AvailableAt,
		MessageStatus:         model.DeviceMessageOutboxStatusDeadLettered,
		AttemptCount:          secondClaim[0].AttemptCount + 1,
		LastError:             stringPtr("duplicate claimant observed publish failure"),
		AvailableAt:           deadLetterAt,
		OperationStatus:       model.DeviceOperationStatusDeadLettered,
		OperationErrorCode:    stringPtr("publish_failed"),
		OperationErrorMessage: stringPtr("duplicate claimant observed publish failure"),
		OperationRetryable:    &retryable,
		OperationCompletedAt:  &deadLetterAt,
	}); err != nil {
		t.Fatal(err)
	}

	receivedAt := deadLetterAt.Add(time.Second)
	if _, _, err := env.store.CreateOrGetInboxMessage(ctx, DeviceMessageInboxCreateInput{
		MessageID:     "preserve-inbox-outcome-evt",
		OperationID:   op.OperationID,
		CorrelationID: op.CorrelationID,
		Stream:        "video.account.events",
		MessageType:   "DeviceProvisionSucceeded",
		SchemaVersion: "1.0",
		PartitionKey:  deviceID,
		Payload: map[string]any{
			"video_cloud_devid": "device-1",
			"activity_id":       "activity-1",
		},
		Status:       model.DeviceMessageInboxStatusRetrying,
		AttemptCount: 0,
		ReceivedAt:   receivedAt,
	}); err != nil {
		t.Fatal(err)
	}

	completedAt := receivedAt.Add(time.Second)
	inboxResult, err := env.store.RecordInboxProcessTransition(ctx, InboxProcessTransitionInput{
		MessageID:            "preserve-inbox-outcome-evt",
		MessageStatus:        model.DeviceMessageInboxStatusProcessed,
		AttemptCount:         1,
		ProcessedAt:          &completedAt,
		OperationStatus:      ptrTo(model.DeviceOperationStatusSucceeded),
		OperationResult:      map[string]any{"video_cloud_devid": "device-1", "activity_id": "activity-1"},
		OperationCompletedAt: &completedAt,
		OrganizationID:       orgID,
		DeviceID:             deviceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if inboxResult.Operation == nil || inboxResult.Operation.Status != model.DeviceOperationStatusSucceeded {
		t.Fatalf("expected inbox transition to complete the operation, got %+v", inboxResult.Operation)
	}

	publishedAt := firstLeaseUntil.Add(time.Second)
	result, err := env.store.RecordOutboxPublishTransition(ctx, OutboxPublishTransitionInput{
		MessageID:             firstClaim[0].MessageID,
		ExpectedMessageStatus: firstClaim[0].Status,
		ExpectedAttemptCount:  firstClaim[0].AttemptCount,
		ExpectedAvailableAt:   firstClaim[0].AvailableAt,
		MessageStatus:         model.DeviceMessageOutboxStatusPublished,
		AttemptCount:          firstClaim[0].AttemptCount + 1,
		AvailableAt:           publishedAt,
		PublishedAt:           &publishedAt,
		OperationStatus:       model.DeviceOperationStatusPublished,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Message.Status != model.DeviceMessageOutboxStatusPublished {
		t.Fatalf("expected delayed publish repair to win on the outbox row, got %+v", result.Message)
	}
	if result.Operation.Status != model.DeviceOperationStatusSucceeded {
		t.Fatalf("expected completed operation to stay succeeded, got %+v", result.Operation)
	}
	if got := result.Operation.ResultPayload["video_cloud_devid"]; got != "device-1" {
		t.Fatalf("expected publish repair to preserve inbox result payload, got %+v", result.Operation.ResultPayload)
	}
	if result.Operation.CompletedAt == nil || !result.Operation.CompletedAt.Equal(completedAt) {
		t.Fatalf("expected publish repair to preserve operation completion time, got %+v", result.Operation.CompletedAt)
	}

	storedOperation, err := env.store.GetDeviceOperation(ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if storedOperation.Status != model.DeviceOperationStatusSucceeded {
		t.Fatalf("expected stored operation to keep inbox outcome, got %+v", storedOperation)
	}
	if got := storedOperation.ResultPayload["activity_id"]; got != "activity-1" {
		t.Fatalf("expected stored operation result payload to remain intact, got %+v", storedOperation.ResultPayload)
	}
	if storedOperation.CompletedAt == nil || !storedOperation.CompletedAt.Equal(completedAt) {
		t.Fatalf("expected stored operation completion time to remain intact, got %+v", storedOperation.CompletedAt)
	}
}

func TestRecordInboxProcessTransitionUpdatesOperationAndProjection(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	orgID, userID, deviceID := createDeviceFixture(t, env)

	ctx := context.Background()
	op, _, err := env.store.CreateOrGetDeviceOperation(ctx, DeviceOperationCreateInput{
		OperationID:    "op-inbox-transition",
		CorrelationID:  "corr-inbox-transition",
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
	if _, _, err := env.store.CreateOrGetInboxMessage(ctx, DeviceMessageInboxCreateInput{
		MessageID:     "evt-transition",
		OperationID:   op.OperationID,
		CorrelationID: op.CorrelationID,
		Stream:        "video.account.events",
		MessageType:   "DeviceProvisionSucceeded",
		SchemaVersion: "1.0",
		PartitionKey:  deviceID,
		Payload: map[string]any{
			"video_cloud_devid": "device-1",
			"activity_id":       "activity-1",
		},
		Status:       model.DeviceMessageInboxStatusRetrying,
		AttemptCount: 0,
		ReceivedAt:   receivedAt,
	}); err != nil {
		t.Fatal(err)
	}

	activatedAt := receivedAt.Add(time.Minute)
	processedAt := activatedAt.Add(time.Second)
	result, err := env.store.RecordInboxProcessTransition(ctx, InboxProcessTransitionInput{
		MessageID:            "evt-transition",
		MessageStatus:        model.DeviceMessageInboxStatusProcessed,
		AttemptCount:         1,
		ProcessedAt:          &processedAt,
		OperationStatus:      ptrTo(model.DeviceOperationStatusSucceeded),
		OperationResult:      map[string]any{"video_cloud_devid": "device-1", "activity_id": "activity-1"},
		OperationCompletedAt: &activatedAt,
		OrganizationID:       orgID,
		DeviceID:             deviceID,
		Projection:           ptrTo(ProvisionSucceededProjection(channel.DeviceProvisionSucceededPayload{VideoCloudDevid: "device-1", ActivityID: "activity-1", ActivatedAt: activatedAt})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Message.Status != model.DeviceMessageInboxStatusProcessed {
		t.Fatalf("expected processed inbox status, got %s", result.Message.Status)
	}
	if result.Message.ProcessedAt == nil || !result.Message.ProcessedAt.Equal(processedAt) {
		t.Fatalf("expected inbox processed_at to be set, got %+v", result.Message.ProcessedAt)
	}
	if result.Operation == nil || result.Operation.Status != model.DeviceOperationStatusSucceeded {
		t.Fatalf("expected succeeded operation, got %+v", result.Operation)
	}
	if result.Operation.CompletedAt == nil || !result.Operation.CompletedAt.Equal(activatedAt) {
		t.Fatalf("expected completed operation timestamp, got %+v", result.Operation.CompletedAt)
	}
	if got := result.Operation.ResultPayload["video_cloud_devid"]; got != "device-1" {
		t.Fatalf("expected operation result payload to be stored, got %+v", result.Operation.ResultPayload)
	}
	if result.Device == nil {
		t.Fatal("expected projected device")
	}
	if got := result.Device.Metadata[model.DeviceMetadataVideoCloudDevid]; got != "device-1" {
		t.Fatalf("expected projected video_cloud_devid, got %+v", got)
	}
	if got := result.Device.Metadata[model.DeviceMetadataVideoCloudActivationStatus]; got != string(model.VideoCloudActivationStatusActivated) {
		t.Fatalf("expected activated projection metadata, got %+v", got)
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

	stored, err := env.store.GetInboxMessage(ctx, message.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != message.ID || stored.Status != model.DeviceMessageInboxStatusProcessed {
		t.Fatalf("unexpected stored inbox message: %+v", stored)
	}

	if _, err := env.store.GetInboxMessage(ctx, "missing-event"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing inbox message to return ErrNotFound, got %v", err)
	}
	if _, err := env.store.UpdateInboxMessage(ctx, "missing-event", DeviceMessageInboxUpdateInput{
		Status: model.DeviceMessageInboxStatusFailed,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing inbox update to return ErrNotFound, got %v", err)
	}
}

func TestCreateOrGetInboxMessagePreservesDeadLetterPayloadSnapshot(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	orgID, userID, deviceID := createDeviceFixture(t, env)

	ctx := context.Background()
	_, _, err := env.store.CreateOrGetDeviceOperation(ctx, DeviceOperationCreateInput{
		OperationID:    "op-dead-letter-payload",
		CorrelationID:  "corr-dead-letter-payload",
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
		MessageID:     "evt-dead-letter-payload",
		OperationID:   "op-dead-letter-payload",
		CorrelationID: "corr-dead-letter-payload",
		Stream:        "video.account.events",
		MessageType:   "DeviceProvisionSucceeded",
		SchemaVersion: "1.0",
		PartitionKey:  deviceID,
		Payload: map[string]any{
			"_raw_payload":          "{not-json",
			"_raw_payload_base64":   base64.StdEncoding.EncodeToString([]byte("{not-json")),
			"_payload_decode_error": "invalid character 'n' looking for beginning of object key string",
		},
		Status:       model.DeviceMessageInboxStatusDeadLettered,
		AttemptCount: 1,
		LastError:    stringPtr("invalid payload"),
		ReceivedAt:   receivedAt,
		ProcessedAt:  &receivedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected first inbox insert to create a row")
	}

	stored, err := env.store.GetInboxMessage(ctx, message.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Payload["_raw_payload"]; got != "{not-json" {
		t.Fatalf("expected raw payload snapshot, got %+v", stored.Payload)
	}
	if got := stored.Payload["_raw_payload_base64"]; got != base64.StdEncoding.EncodeToString([]byte("{not-json")) {
		t.Fatalf("expected raw payload bytes snapshot, got %+v", stored.Payload)
	}
	if got := stored.Payload["_payload_decode_error"]; got == nil {
		t.Fatalf("expected payload decode error snapshot, got %+v", stored.Payload)
	}
}

func stringPtr(value string) *string {
	return &value
}

func ptrTo[T any](value T) *T {
	return &value
}

func requirePGErrorCode(t *testing.T, err error, want string) {
	t.Helper()

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected pg error %s, got %v", want, err)
	}
	if pgErr.Code != want {
		t.Fatalf("expected pg error %s, got %s (%s)", want, pgErr.Code, pgErr.Message)
	}
}
