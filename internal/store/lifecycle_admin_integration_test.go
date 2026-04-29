package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
)

func TestListOutboxMessagesByStatusFiltersLifecycleRows(t *testing.T) {
	env := newStoreIntegrationEnv(t)

	ctx := context.Background()
	first := createLifecycleOutboxFixture(t, env, "list-outbox-1", time.Date(2026, 4, 29, 9, 0, 0, 0, time.UTC))
	second := createLifecycleOutboxFixture(t, env, "list-outbox-2", time.Date(2026, 4, 29, 9, 5, 0, 0, time.UTC))
	third := createLifecycleOutboxFixture(t, env, "list-outbox-3", time.Date(2026, 4, 29, 9, 10, 0, 0, time.UTC))

	if _, err := env.store.UpdateOutboxMessage(ctx, first.Message.MessageID, DeviceMessageOutboxUpdateInput{
		Status:       model.DeviceMessageOutboxStatusRetrying,
		AttemptCount: 2,
		LastError:    stringPtr("retry later"),
		AvailableAt:  first.Message.AvailableAt.Add(2 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.UpdateOutboxMessage(ctx, second.Message.MessageID, DeviceMessageOutboxUpdateInput{
		Status:       model.DeviceMessageOutboxStatusDeadLettered,
		AttemptCount: 5,
		LastError:    stringPtr("permanent failure"),
		AvailableAt:  second.Message.AvailableAt,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.UpdateOutboxMessage(ctx, third.Message.MessageID, DeviceMessageOutboxUpdateInput{
		Status:       model.DeviceMessageOutboxStatusPublished,
		AttemptCount: 1,
		AvailableAt:  third.Message.AvailableAt,
		PublishedAt:  timePtr(third.Message.AvailableAt.Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}

	messages, err := env.store.ListOutboxMessagesByStatus(ctx, []model.DeviceMessageOutboxStatus{
		model.DeviceMessageOutboxStatusRetrying,
		model.DeviceMessageOutboxStatusDeadLettered,
	}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected 2 outbox messages, got %d", len(messages))
	}
	if messages[0].MessageID != second.Message.MessageID || messages[1].MessageID != first.Message.MessageID {
		t.Fatalf("unexpected outbox list order: %+v", messages)
	}
}

func TestGetOutboxMessageDetailIncludesOperation(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	fixture := createLifecycleOutboxFixture(t, env, "show-outbox", time.Date(2026, 4, 29, 10, 0, 0, 0, time.UTC))

	detail, err := env.store.GetOutboxMessageDetail(context.Background(), fixture.Message.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Message.MessageID != fixture.Message.MessageID {
		t.Fatalf("unexpected outbox detail message: %+v", detail.Message)
	}
	if detail.Operation == nil || detail.Operation.OperationID != fixture.Operation.OperationID {
		t.Fatalf("expected related operation %q, got %+v", fixture.Operation.OperationID, detail.Operation)
	}
}

func TestRequeueOutboxMessageResetsRetryState(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	fixture := createLifecycleOutboxFixture(t, env, "requeue-outbox", time.Date(2026, 4, 29, 11, 0, 0, 0, time.UTC))

	completedAt := fixture.Message.AvailableAt.Add(10 * time.Minute)
	if _, err := env.store.UpdateOutboxMessage(ctx, fixture.Message.MessageID, DeviceMessageOutboxUpdateInput{
		Status:       model.DeviceMessageOutboxStatusDeadLettered,
		AttemptCount: 5,
		LastError:    stringPtr("publish failed"),
		AvailableAt:  completedAt,
	}); err != nil {
		t.Fatal(err)
	}
	retryable := false
	if _, err := env.store.UpdateDeviceOperation(ctx, fixture.Operation.OperationID, DeviceOperationUpdateInput{
		Status:        model.DeviceOperationStatusDeadLettered,
		ResultPayload: map[string]any{"phase": "publish"},
		ErrorCode:     stringPtr("publish_failed"),
		ErrorMessage:  stringPtr("publish failed"),
		Retryable:     &retryable,
		CompletedAt:   &completedAt,
	}); err != nil {
		t.Fatal(err)
	}

	requeueAt := time.Date(2026, 4, 29, 11, 30, 0, 0, time.UTC)
	message, operation, changed, err := env.store.RequeueOutboxMessage(ctx, fixture.Message.MessageID, requeueAt)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected dead-lettered outbox row to be requeued")
	}
	if message.Status != model.DeviceMessageOutboxStatusPending || message.AttemptCount != 0 {
		t.Fatalf("unexpected outbox message after requeue: %+v", message)
	}
	if message.LastError != nil || message.PublishedAt != nil || !message.AvailableAt.Equal(requeueAt) {
		t.Fatalf("expected cleared outbox retry fields, got %+v", message)
	}
	if operation == nil {
		t.Fatal("expected related operation in requeue result")
	}
	if operation.Status != model.DeviceOperationStatusPending {
		t.Fatalf("expected pending operation after requeue, got %+v", operation)
	}
	if operation.ErrorCode != nil || operation.ErrorMessage != nil || operation.Retryable != nil || operation.CompletedAt != nil {
		t.Fatalf("expected cleared operation error fields, got %+v", operation)
	}
}

func TestRequeueOutboxMessageRejectsCompletedLifecycleOperation(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	fixture := createLifecycleOutboxFixture(t, env, "requeue-outbox-guard", time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC))

	if _, err := env.store.UpdateOutboxMessage(ctx, fixture.Message.MessageID, DeviceMessageOutboxUpdateInput{
		Status:       model.DeviceMessageOutboxStatusRetrying,
		AttemptCount: 2,
		LastError:    stringPtr("stale retry"),
		AvailableAt:  fixture.Message.AvailableAt.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	completedAt := fixture.Message.AvailableAt.Add(5 * time.Minute)
	if _, err := env.store.UpdateDeviceOperation(ctx, fixture.Operation.OperationID, DeviceOperationUpdateInput{
		Status:        model.DeviceOperationStatusSucceeded,
		ResultPayload: map[string]any{"video_cloud_devid": "video-requeue-outbox-guard"},
		CompletedAt:   &completedAt,
	}); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := env.store.RequeueOutboxMessage(ctx, fixture.Message.MessageID, completedAt.Add(time.Minute))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict when requeue would roll back a completed operation, got %v", err)
	}
}

func TestListInboxMessagesByStatusAndShowDetail(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	fixture := createLifecycleOutboxFixture(t, env, "list-inbox", time.Date(2026, 4, 29, 13, 0, 0, 0, time.UTC))

	firstReceivedAt := time.Date(2026, 4, 29, 13, 5, 0, 0, time.UTC)
	secondReceivedAt := time.Date(2026, 4, 29, 13, 10, 0, 0, time.UTC)
	if _, _, err := env.store.CreateOrGetInboxMessage(ctx, DeviceMessageInboxCreateInput{
		MessageID:     "evt-list-inbox-1",
		OperationID:   fixture.Operation.OperationID,
		CorrelationID: fixture.Operation.CorrelationID,
		Stream:        "video.account.events",
		MessageType:   string(channel.MessageTypeDeviceProvisionFailed),
		SchemaVersion: "1.0",
		PartitionKey:  fixture.Operation.DeviceID,
		Payload:       map[string]any{"error_code": "bad_request"},
		Status:        model.DeviceMessageInboxStatusRetrying,
		AttemptCount:  1,
		ReceivedAt:    firstReceivedAt,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.store.CreateOrGetInboxMessage(ctx, DeviceMessageInboxCreateInput{
		MessageID:     "evt-list-inbox-2",
		OperationID:   fixture.Operation.OperationID,
		CorrelationID: fixture.Operation.CorrelationID,
		Stream:        "video.account.events",
		MessageType:   string(channel.MessageTypeDeviceProvisionFailed),
		SchemaVersion: "1.0",
		PartitionKey:  fixture.Operation.DeviceID,
		Payload:       map[string]any{"error_code": "fatal"},
		Status:        model.DeviceMessageInboxStatusDeadLettered,
		AttemptCount:  5,
		LastError:     stringPtr("fatal"),
		ReceivedAt:    secondReceivedAt,
		ProcessedAt:   timePtr(secondReceivedAt.Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}

	messages, err := env.store.ListInboxMessagesByStatus(ctx, []model.DeviceMessageInboxStatus{
		model.DeviceMessageInboxStatusRetrying,
		model.DeviceMessageInboxStatusDeadLettered,
	}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected 2 inbox messages, got %d", len(messages))
	}
	if messages[0].MessageID != "evt-list-inbox-2" || messages[1].MessageID != "evt-list-inbox-1" {
		t.Fatalf("unexpected inbox list order: %+v", messages)
	}

	detail, err := env.store.GetInboxMessageDetail(ctx, "evt-list-inbox-2")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Operation == nil || detail.Operation.OperationID != fixture.Operation.OperationID {
		t.Fatalf("expected related operation %q, got %+v", fixture.Operation.OperationID, detail.Operation)
	}
}

func TestRequeueInboxMessageReopensDeadLetteredRow(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	fixture := createLifecycleOutboxFixture(t, env, "requeue-inbox", time.Date(2026, 4, 29, 14, 0, 0, 0, time.UTC))

	processedAt := fixture.Message.AvailableAt.Add(15 * time.Minute)
	if _, _, err := env.store.CreateOrGetInboxMessage(ctx, DeviceMessageInboxCreateInput{
		MessageID:     "evt-requeue-inbox",
		OperationID:   fixture.Operation.OperationID,
		CorrelationID: fixture.Operation.CorrelationID,
		Stream:        "video.account.events",
		MessageType:   string(channel.MessageTypeDeviceProvisionFailed),
		SchemaVersion: "1.0",
		PartitionKey:  fixture.Operation.DeviceID,
		Payload:       map[string]any{"error_code": "fatal"},
		Status:        model.DeviceMessageInboxStatusDeadLettered,
		AttemptCount:  5,
		LastError:     stringPtr("fatal"),
		ReceivedAt:    fixture.Message.AvailableAt,
		ProcessedAt:   &processedAt,
	}); err != nil {
		t.Fatal(err)
	}

	message, operation, changed, err := env.store.RequeueInboxMessage(ctx, "evt-requeue-inbox")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected dead-lettered inbox row to reopen")
	}
	if message.Status != model.DeviceMessageInboxStatusRetrying || message.AttemptCount != 0 {
		t.Fatalf("unexpected inbox message after requeue: %+v", message)
	}
	if message.LastError != nil || message.ProcessedAt != nil {
		t.Fatalf("expected cleared inbox retry fields, got %+v", message)
	}
	if operation == nil || operation.OperationID != fixture.Operation.OperationID {
		t.Fatalf("expected related operation %q, got %+v", fixture.Operation.OperationID, operation)
	}
}

func createLifecycleOutboxFixture(t *testing.T, env storeIntegrationEnv, suffix string, now time.Time) DeviceLifecycleOperationResult {
	t.Helper()

	orgID, userID, deviceID := createDeviceFixtureForEmail(t, env, suffix+"@example.com")
	result, err := env.store.StartDeviceLifecycleOperation(context.Background(), DeviceLifecycleOperationInput{
		OperationID:       "op-" + suffix,
		CorrelationID:     "corr-" + suffix,
		MessageID:         "msg-" + suffix,
		OrganizationID:    orgID,
		DeviceID:          deviceID,
		OperationType:     model.DeviceOperationTypeProvision,
		RequestedBy:       &userID,
		RequestPayload:    map[string]any{"video_cloud_devid": "video-" + suffix},
		OutboxMessageType: string(channel.MessageTypeDeviceProvisionRequested),
		OutboxPayload: map[string]any{
			"org_id":            orgID,
			"account_device_id": deviceID,
			"video_cloud_devid": "video-" + suffix,
			"activity_id":       "activity-" + suffix,
			"clip_public_key":   "clip-" + suffix,
			"requested_by":      userID,
		},
		Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func timePtr(value time.Time) *time.Time {
	return &value
}
