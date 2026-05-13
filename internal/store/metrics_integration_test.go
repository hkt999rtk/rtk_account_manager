package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func TestListAuditEventsReturnsRecordedLifecycleEvents(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	registered, err := env.store.Register(ctx, RegisterInput{
		Email:                     "audit@example.com",
		PasswordHash:              "hash",
		OrganizationName:          "Audit Org",
		OrganizationTier:          model.OrganizationTierEvaluation,
		EvaluationDeviceQuota:     5,
		SignupPendingVerification: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	request, err := env.store.CreateQuotaRaiseRequest(ctx, QuotaRaiseRequestInput{
		OrganizationID: registered.Organization.ID,
		RequestedBy:    registered.User.ID,
		RequestedQuota: 12,
		UseCase:        "pilot expansion",
		ContactInfo:    map[string]any{"email": "audit@example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}

	loadedRequest, err := env.store.GetQuotaRaiseRequest(ctx, request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loadedRequest.ID != request.ID || loadedRequest.Status != model.QuotaRaiseRequestStatusPending {
		t.Fatalf("expected round-tripped pending request, got %+v", loadedRequest)
	}
	if _, err := env.store.GetQuotaRaiseRequest(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing quota raise request to return ErrNotFound, got %v", err)
	}

	device, err := env.store.CreateDevice(ctx, registered.Organization.ID, DeviceInput{
		Name:     "Audit Camera",
		Category: model.DeviceCategoryIPCamera,
		Metadata: map[string]any{"location": "lab"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if device.ID == "" {
		t.Fatal("expected created device ID")
	}
	activeDevices, err := env.store.countActiveDevices(ctx, registered.Organization.ID)
	if err != nil {
		t.Fatal(err)
	}
	if activeDevices != 1 {
		t.Fatalf("expected one active device, got %d", activeDevices)
	}

	reason := "not enough detail"
	decision, _, _, err := env.store.DecideQuotaRaiseRequest(ctx, QuotaRaiseDecisionInput{
		RequestID:      request.ID,
		DecidedBy:      registered.User.ID,
		DecisionReason: &reason,
		Approved:       false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Status != model.QuotaRaiseRequestStatusDeclined {
		t.Fatalf("expected declined request, got %+v", decision)
	}

	eventPage, err := env.store.ListAuditEvents(ctx, AuditEventListFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	events := eventPage.Events
	if len(events) != 3 {
		t.Fatalf("expected 3 audit events, got %d: %+v", len(events), events)
	}

	byType := make(map[string]model.AuditEvent, len(events))
	for _, event := range events {
		byType[event.EventType] = event
	}

	signup := byType["signup_created"]
	if signup.SubjectType != "organization" || signup.SubjectID != registered.Organization.ID {
		t.Fatalf("expected signup audit event for organization %s, got %+v", registered.Organization.ID, signup)
	}
	if got, ok := signup.Payload["organization_tier"].(string); !ok || got != string(model.OrganizationTierEvaluation) {
		t.Fatalf("expected evaluation tier payload, got %+v", signup.Payload)
	}

	quotaRequested := byType["quota_raise_requested"]
	if got, ok := quotaRequested.Payload["requested_quota"].(float64); !ok || got != 12 {
		t.Fatalf("expected requested quota payload, got %+v", quotaRequested.Payload)
	}

	quotaDeclined := byType["quota_raise_declined"]
	if got, ok := quotaDeclined.Payload["decision_reason"].(string); !ok || got != reason {
		t.Fatalf("expected decline reason payload, got %+v", quotaDeclined.Payload)
	}
}

func TestEvaluationQuotaUsageUtilizationHandlesZeroAndNonZeroQuotas(t *testing.T) {
	zero := EvaluationQuotaUsage{ActiveDevices: 3, EvaluationDeviceQuota: 0}
	if zero.Utilization() != 0 {
		t.Fatalf("expected zero quota utilization to be 0, got %f", zero.Utilization())
	}

	usage := EvaluationQuotaUsage{ActiveDevices: 3, EvaluationDeviceQuota: 12}
	if usage.Utilization() != 0.25 {
		t.Fatalf("expected utilization 0.25, got %f", usage.Utilization())
	}
}

func TestLifecycleMetricsAggregatesQueueAndOperationHealth(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	orgID, userID, deviceID := createDeviceFixtureForEmail(t, env, "lifecycle-metrics@example.com")
	now := time.Now().UTC().Truncate(time.Microsecond)

	pendingOp, _, err := env.store.CreateOrGetDeviceOperation(ctx, DeviceOperationCreateInput{
		OperationID:    "metrics-op-pending",
		CorrelationID:  "metrics-corr-pending",
		OrganizationID: orgID,
		DeviceID:       deviceID,
		OperationType:  model.DeviceOperationTypeProvision,
		Status:         model.DeviceOperationStatusPending,
		RequestedBy:    &userID,
		RequestPayload: map[string]any{"video_cloud_devid": "video-metrics-1"},
		ResultPayload:  map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	failedOp, _, err := env.store.CreateOrGetDeviceOperation(ctx, DeviceOperationCreateInput{
		OperationID:    "metrics-op-failed",
		CorrelationID:  "metrics-corr-failed",
		OrganizationID: orgID,
		DeviceID:       deviceID,
		OperationType:  model.DeviceOperationTypeDeactivate,
		Status:         model.DeviceOperationStatusFailed,
		RequestedBy:    &userID,
		RequestPayload: map[string]any{"video_cloud_devid": "video-metrics-1"},
		ResultPayload:  map[string]any{"error": "deactivate failed"},
		ErrorCode:      stringPtr("deactivate_failed"),
		ErrorMessage:   stringPtr("video service rejected deactivate"),
		Retryable:      boolPtr(false),
		CompletedAt:    timePtr(now),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := env.store.CreateOutboxMessage(ctx, DeviceMessageOutboxCreateInput{
		MessageID:     "metrics-outbox-pending",
		OperationID:   pendingOp.OperationID,
		CorrelationID: pendingOp.CorrelationID,
		Stream:        "account.video.commands",
		MessageType:   "DeviceProvisionRequested",
		SchemaVersion: "1.0",
		PartitionKey:  deviceID,
		Payload:       map[string]any{"operation_id": pendingOp.OperationID},
		Status:        model.DeviceMessageOutboxStatusPending,
		AvailableAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CreateOutboxMessage(ctx, DeviceMessageOutboxCreateInput{
		MessageID:     "metrics-outbox-dead",
		OperationID:   failedOp.OperationID,
		CorrelationID: failedOp.CorrelationID,
		Stream:        "account.video.commands",
		MessageType:   "DeviceDeactivateRequested",
		SchemaVersion: "1.0",
		PartitionKey:  deviceID,
		Payload:       map[string]any{"operation_id": failedOp.OperationID},
		Status:        model.DeviceMessageOutboxStatusDeadLettered,
		AttemptCount:  3,
		LastError:     stringPtr("publish_failed"),
		AvailableAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.store.CreateOrGetInboxMessage(ctx, DeviceMessageInboxCreateInput{
		MessageID:     "metrics-inbox-dead",
		OperationID:   failedOp.OperationID,
		CorrelationID: failedOp.CorrelationID,
		Stream:        "video.account.events",
		MessageType:   "DeviceDeactivateFailed",
		SchemaVersion: "1.0",
		PartitionKey:  deviceID,
		Payload:       map[string]any{"operation_id": failedOp.OperationID},
		Status:        model.DeviceMessageInboxStatusDeadLettered,
		AttemptCount:  3,
		LastError:     stringPtr("projection_failed"),
		ReceivedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}

	metrics, err := env.store.GetLifecycleMetrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Outbox.ByStatus[string(model.DeviceMessageOutboxStatusPending)] != 1 ||
		metrics.Outbox.ByStatus[string(model.DeviceMessageOutboxStatusDeadLettered)] != 1 {
		t.Fatalf("unexpected outbox status counts: %+v", metrics.Outbox.ByStatus)
	}
	if len(metrics.Outbox.DeadLetteredByError) != 1 ||
		metrics.Outbox.DeadLetteredByError[0].MessageType != "DeviceDeactivateRequested" ||
		metrics.Outbox.DeadLetteredByError[0].ErrorCode != "publish_failed" ||
		metrics.Outbox.DeadLetteredByError[0].Count != 1 {
		t.Fatalf("unexpected outbox dead-letter counts: %+v", metrics.Outbox.DeadLetteredByError)
	}
	if metrics.Inbox.ByStatus[string(model.DeviceMessageInboxStatusDeadLettered)] != 1 {
		t.Fatalf("unexpected inbox status counts: %+v", metrics.Inbox.ByStatus)
	}
	if len(metrics.Inbox.DeadLetteredByError) != 1 ||
		metrics.Inbox.DeadLetteredByError[0].MessageType != "DeviceDeactivateFailed" ||
		metrics.Inbox.DeadLetteredByError[0].ErrorCode != "projection_failed" {
		t.Fatalf("unexpected inbox dead-letter counts: %+v", metrics.Inbox.DeadLetteredByError)
	}
	if metrics.Operations.ByStatus[string(model.DeviceOperationStatusPending)] != 1 ||
		metrics.Operations.ByStatus[string(model.DeviceOperationStatusFailed)] != 1 {
		t.Fatalf("unexpected operation status counts: %+v", metrics.Operations.ByStatus)
	}
	if !hasOperationTypeStatus(metrics.Operations.ByTypeAndStatus, string(model.DeviceOperationTypeDeactivate), string(model.DeviceOperationStatusFailed), 1) {
		t.Fatalf("expected failed deactivate type/status count, got %+v", metrics.Operations.ByTypeAndStatus)
	}
	if metrics.Operations.OldestActiveAgeSeconds < 0 {
		t.Fatalf("expected non-negative active age, got %d", metrics.Operations.OldestActiveAgeSeconds)
	}
	if metrics.Operations.LastTerminalCompletedAt == nil || !metrics.Operations.LastTerminalCompletedAt.Equal(now) {
		t.Fatalf("expected terminal completion timestamp %s, got %+v", now, metrics.Operations.LastTerminalCompletedAt)
	}
}

func hasOperationTypeStatus(counts []LifecycleOperationStatusCount, operationType, status string, count int64) bool {
	for _, got := range counts {
		if got.OperationType == operationType && got.Status == status && got.Count == count {
			return true
		}
	}
	return false
}

func boolPtr(value bool) *bool {
	return &value
}
