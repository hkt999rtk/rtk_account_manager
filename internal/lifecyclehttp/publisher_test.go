package lifecyclehttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/broker"
	"rtk_account_manager/internal/channel"
)

const (
	testOrgID     = "11111111-1111-4111-8111-111111111111"
	testAccountID = "22222222-2222-4222-8222-222222222222"
)

func TestPublisherCallsInternalLifecycleAPIAndProjectsSuccess(t *testing.T) {
	now := time.Date(2026, 8, 2, 3, 4, 5, 0, time.UTC)
	var projected broker.Message
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/internal/account-manager/devices/video-1/deactivate" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer shared-token" {
			t.Fatalf("authorization header=%q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	publisher, err := NewPublisher(Options{
		BaseURL: server.URL, Token: "shared-token", Now: func() time.Time { return now },
		Project: func(_ context.Context, message broker.Message) error { projected = message; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope := requestEnvelope(t, channel.MessageTypeDeviceDeactivateRequested, &channel.DeviceDeactivateRequestedPayload{
		OrgID: testOrgID, AccountDeviceID: testAccountID, VideoCloudDevid: "video-1", RequestedBy: "user-1", Reason: "test",
	})
	if err := publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, envelope); err != nil {
		t.Fatal(err)
	}
	if projected.Envelope.MessageType != channel.MessageTypeDeviceDeactivateSucceeded || projected.Envelope.CausationID != envelope.MessageID {
		t.Fatalf("projected=%+v", projected.Envelope)
	}
	payload, err := projected.Envelope.ValidateAndDecode(channel.StreamVideoAccountEvents)
	if err != nil {
		t.Fatal(err)
	}
	result := payload.(*channel.DeviceDeactivateSucceededPayload)
	if result.VideoCloudDevid != "video-1" || !result.DeactivatedAt.Equal(now) {
		t.Fatalf("payload=%+v", result)
	}
}

func TestPublisherForwardsRevisionedProductGrantToInternalActivation(t *testing.T) {
	revision := int64(7)
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/internal/account-manager/devices/video-1/activate" || r.Header.Get("Authorization") != "Bearer shared-token" {
			t.Errorf("request path=%q authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	publisher, err := NewPublisher(Options{BaseURL: server.URL, Token: "shared-token", Project: func(context.Context, broker.Message) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	envelope := requestEnvelope(t, channel.MessageTypeDeviceProvisionRequested, &channel.DeviceProvisionRequestedPayload{
		OrgID: testOrgID, AccountDeviceID: testAccountID, VideoCloudDevid: "video-1", ActivityID: "activity-1", ClipPublicKey: "clip-key",
		ServiceOptions: []string{"mqtt", "iot_shadow"}, ProductID: "33333333-3333-4333-8333-333333333333",
		ProductServiceRevision: &revision, ServiceGrantSHA256: strings.Repeat("a", 64), RequestedBy: "user-1",
	})
	if err := publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, envelope); err != nil {
		t.Fatal(err)
	}
	if received["product_id"] != "33333333-3333-4333-8333-333333333333" || received["product_service_revision"] != float64(7) ||
		received["service_grant_sha256"] != strings.Repeat("a", 64) || len(received["service_options"].([]any)) != 2 {
		t.Fatalf("grant was not forwarded: %+v", received)
	}
}

func TestPublisherSendsEntitlementSnapshotByPUTAndProjectsResult(t *testing.T) {
	now := time.Date(2026, 9, 19, 1, 2, 3, 0, time.UTC)
	var requestBody map[string]any
	var projected broker.Message
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/internal/account-manager/devices/video-1/entitlement" ||
			r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("method=%s path=%s auth=%s", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	publisher, err := NewPublisher(Options{BaseURL: server.URL, Token: "token", Now: func() time.Time { return now },
		Project: func(_ context.Context, message broker.Message) error { projected = message; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	envelope := requestEnvelope(t, channel.MessageTypeDeviceEntitlementSnapshotRequested, &channel.DeviceEntitlementSnapshotRequestedPayload{
		OrgID: testOrgID, AccountDeviceID: testAccountID, VideoCloudDevid: "video-1",
		ProductID: "33333333-3333-4333-8333-333333333333", ProductServiceRevision: 2,
		ServiceGrantSHA256: strings.Repeat("a", 64), PlatformEntitlementRevision: 3,
		ServiceOptions: []string{"mqtt"}, State: "revoked", RequestedBy: "user-1",
	})
	if err := publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, envelope); err != nil {
		t.Fatal(err)
	}
	if requestBody["platform_entitlement_revision"] != float64(3) || requestBody["state"] != "revoked" ||
		requestBody["brand_cloud_id"] != testOrgID || requestBody["service_grant_sha256"] != strings.Repeat("a", 64) {
		t.Fatalf("request body=%+v", requestBody)
	}
	if projected.Envelope.MessageType != channel.MessageTypeDeviceEntitlementSnapshotSucceeded {
		t.Fatalf("projected=%+v", projected.Envelope)
	}
	result, err := projected.Envelope.ValidateAndDecode(channel.StreamVideoAccountEvents)
	if err != nil {
		t.Fatal(err)
	}
	if succeeded := result.(*channel.DeviceEntitlementSnapshotSucceededPayload); succeeded.PlatformEntitlementRevision != 3 || !succeeded.AppliedAt.Equal(now) {
		t.Fatalf("result=%+v", succeeded)
	}
}

func TestPublisherRetriesSnapshotAfterReceiverCommitsButReturnsUnavailable(t *testing.T) {
	requests := 0
	projected := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/internal/account-manager/devices/video-1/entitlement" {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		requests++
		if requests == 1 {
			// The receiver's durable write succeeded; its cache refresh did not.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK) // identical revision replay repaired the cache
	}))
	defer server.Close()
	publisher, err := NewPublisher(Options{BaseURL: server.URL, Token: "token", Project: func(context.Context, broker.Message) error {
		projected++
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	envelope := requestEnvelope(t, channel.MessageTypeDeviceEntitlementSnapshotRequested, &channel.DeviceEntitlementSnapshotRequestedPayload{
		OrgID: testOrgID, AccountDeviceID: testAccountID, VideoCloudDevid: "video-1",
		ProductID: "33333333-3333-4333-8333-333333333333", ProductServiceRevision: 2,
		ServiceGrantSHA256: strings.Repeat("a", 64), PlatformEntitlementRevision: 1,
		ServiceOptions: []string{"mqtt"}, State: "revoked", RequestedBy: "user-1",
	})
	if err := publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, envelope); !broker.IsTransient(err) || projected != 0 {
		t.Fatalf("first attempt err=%v projected=%d", err, projected)
	}
	if err := publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, envelope); err != nil || projected != 1 || requests != 2 {
		t.Fatalf("replay err=%v projected=%d requests=%d", err, projected, requests)
	}
}

func TestPublisherRetriesTransientHTTPWithoutProjecting(t *testing.T) {
	projected := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	publisher, err := NewPublisher(Options{BaseURL: server.URL, Token: "token", Project: func(context.Context, broker.Message) error { projected = true; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	err = publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, requestEnvelope(t, channel.MessageTypeDeviceUnprovisionRequested, &channel.DeviceUnprovisionRequestedPayload{
		OrgID: testOrgID, AccountDeviceID: testAccountID, VideoCloudDevid: "video-1", RequestedBy: "user-1", Reason: "test", UnprovisionedAt: time.Now().UTC(),
	}))
	if !broker.IsTransient(err) || projected {
		t.Fatalf("err=%v projected=%v", err, projected)
	}
}

func TestPublisherProjectsTerminalFailureAndRedactsResponseBody(t *testing.T) {
	var projected broker.Message
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"fail","reason":"device not activated","secret":"must-not-leak"}`))
	}))
	defer server.Close()
	publisher, err := NewPublisher(Options{BaseURL: server.URL, Token: "token", Project: func(_ context.Context, message broker.Message) error { projected = message; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	err = publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, requestEnvelope(t, channel.MessageTypeDeviceDeactivateRequested, &channel.DeviceDeactivateRequestedPayload{
		OrgID: testOrgID, AccountDeviceID: testAccountID, VideoCloudDevid: "video-1", RequestedBy: "user-1", Reason: "test",
	}))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := projected.Envelope.ValidateAndDecode(channel.StreamVideoAccountEvents)
	if err != nil {
		t.Fatal(err)
	}
	result := payload.(*channel.DeviceDeactivateFailedPayload)
	if result.ErrorMessage != "device not activated" || strings.Contains(result.ErrorMessage, "must-not-leak") {
		t.Fatalf("payload=%+v", result)
	}
}

func TestPublisherMakesProjectionFailureRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	publisher, err := NewPublisher(Options{BaseURL: server.URL, Token: "token", Project: func(context.Context, broker.Message) error { return errors.New("database unavailable") }})
	if err != nil {
		t.Fatal(err)
	}
	err = publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, requestEnvelope(t, channel.MessageTypeDeviceDeactivateRequested, &channel.DeviceDeactivateRequestedPayload{
		OrgID: testOrgID, AccountDeviceID: testAccountID, VideoCloudDevid: "video-1", RequestedBy: "user-1", Reason: "test",
	}))
	if !broker.IsTransient(err) {
		t.Fatalf("err=%v", err)
	}
}

func TestNewPublisherRejectsUnsafeConfiguration(t *testing.T) {
	project := func(context.Context, broker.Message) error { return nil }
	for _, tc := range []struct{ name, baseURL, token string }{
		{"relative URL", "/video", "token"}, {"credential URL", "https://user@example.test", "token"}, {"path URL", "https://video.example.test/api", "token"}, {"missing token", "https://video.example.test", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewPublisher(Options{BaseURL: tc.baseURL, Token: tc.token, Project: project}); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func requestEnvelope(t *testing.T, messageType channel.MessageType, payload channel.Payload) channel.Envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	envelope := channel.Envelope{
		MessageID: "message-1", CorrelationID: "correlation-1", OperationID: "operation-1",
		SourceService: channel.ServiceAccountManager, TargetService: channel.ServiceRealtekVideoCloud,
		MessageType: messageType, SchemaVersion: channel.SchemaVersionV1, PartitionKey: testAccountID, OccurredAt: time.Now().UTC(), Payload: raw,
	}
	if _, err := envelope.ValidateAndDecode(channel.StreamAccountVideoCommands); err != nil {
		t.Fatal(err)
	}
	return envelope
}
