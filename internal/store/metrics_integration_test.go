package store

import (
	"context"
	"errors"
	"testing"

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
