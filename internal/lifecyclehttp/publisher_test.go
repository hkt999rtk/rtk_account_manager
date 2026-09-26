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
	var requestBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/internal/account-manager/devices/video-1/deactivate" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer shared-token" {
			t.Fatalf("authorization header=%q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatal(err)
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
		OrgID: testOrgID, AccountDeviceID: testAccountID, VideoCloudDevid: "video-1", ActivityID: "activity-1", RequestedBy: "user-1", Reason: "test",
	})
	if err := publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, envelope); err != nil {
		t.Fatal(err)
	}
	if requestBody["activity_id"] != "activity-1" {
		t.Fatalf("deactivation request did not pin activity generation: %+v", requestBody)
	}
	if projected.Envelope.MessageType != channel.MessageTypeDeviceDeactivateSucceeded || projected.Envelope.CausationID != envelope.MessageID {
		t.Fatalf("projected=%+v", projected.Envelope)
	}
	payload, err := projected.Envelope.ValidateAndDecode(channel.StreamVideoAccountEvents)
	if err != nil {
		t.Fatal(err)
	}
	result := payload.(*channel.DeviceDeactivateSucceededPayload)
	if result.VideoCloudDevid != "video-1" || result.ActivityID != "activity-1" || !result.DeactivatedAt.Equal(now) {
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
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","devid":"video-1","org_id":"` + testOrgID + `","account_device_id":"` + testAccountID + `","activity_id":"activity-1","activated":true}`))
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
		TransferReservationID: strings.Repeat("b", 64),
	})
	if err := publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, envelope); err != nil {
		t.Fatal(err)
	}
	if received["product_id"] != "33333333-3333-4333-8333-333333333333" || received["product_service_revision"] != float64(7) ||
		received["service_grant_sha256"] != strings.Repeat("a", 64) || len(received["service_options"].([]any)) != 2 ||
		received["transfer_reservation_id"] != strings.Repeat("b", 64) {
		t.Fatalf("grant was not forwarded: %+v", received)
	}
}

func TestPublisherRejectsUnmatchedActivationReceiptWithoutProjecting(t *testing.T) {
	valid := `{"status":"ok","devid":"video-1","org_id":"` + testOrgID + `","account_device_id":"` + testAccountID + `","activity_id":"activity-1","activated":true}`
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"empty", "", http.StatusOK},
		{"malformed", `{`, http.StatusOK},
		{"wrong device", strings.Replace(valid, `"devid":"video-1"`, `"devid":"video-2"`, 1), http.StatusOK},
		{"wrong organization", strings.Replace(valid, testOrgID, "different-org", 1), http.StatusOK},
		{"wrong account device", strings.Replace(valid, testAccountID, "different-account", 1), http.StatusOK},
		{"wrong activity", strings.Replace(valid, `"activity_id":"activity-1"`, `"activity_id":"activity-old"`, 1), http.StatusOK},
		{"not activated", strings.Replace(valid, `"activated":true`, `"activated":false`, 1), http.StatusOK},
		{"wrong status", strings.Replace(valid, `"status":"ok"`, `"status":"pending"`, 1), http.StatusOK},
		{"unexpected 201", valid, http.StatusCreated},
		{"oversized", valid + strings.Repeat(" ", 4096), http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projected := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			publisher, err := NewPublisher(Options{BaseURL: server.URL, Token: "token", Project: func(context.Context, broker.Message) error {
				projected = true
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			envelope := requestEnvelope(t, channel.MessageTypeDeviceProvisionRequested, &channel.DeviceProvisionRequestedPayload{
				OrgID: testOrgID, AccountDeviceID: testAccountID, VideoCloudDevid: "video-1",
				ActivityID: "activity-1", ClipPublicKey: "clip-key", RequestedBy: "user-1",
			})
			if err := publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, envelope); !broker.IsTransient(err) || projected {
				t.Fatalf("receipt %q: err=%v projected=%v", tc.name, err, projected)
			}
		})
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
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "devid": "video-1", "operation_id": requestBody["operation_id"],
			"applied_revision": 3, "platform_entitlement_revision": 3, "product_service_revision": 2,
			"service_grant_sha256": strings.Repeat("a", 64), "state": "revoked"})
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
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusOK) // identical revision replay repaired the cache
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "devid": "video-1", "operation_id": body["operation_id"],
			"applied_revision": 1, "platform_entitlement_revision": 1, "product_service_revision": 2,
			"service_grant_sha256": strings.Repeat("a", 64), "state": "revoked"})
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

func TestPublisherDoesNotProjectAcceptedOrMismatchedSnapshotReceipt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"accepted", http.StatusAccepted, `{}`},
		{"wrong-revision", http.StatusOK, `{"status":"ok","devid":"video-1","operation_id":"wrong","applied_revision":1,"platform_entitlement_revision":1,"product_service_revision":2,"service_grant_sha256":"` + strings.Repeat("a", 64) + `","state":"revoked"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projected := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			publisher, err := NewPublisher(Options{BaseURL: server.URL, Token: "token", Project: func(context.Context, broker.Message) error { projected = true; return nil }})
			if err != nil {
				t.Fatal(err)
			}
			envelope := requestEnvelope(t, channel.MessageTypeDeviceEntitlementSnapshotRequested, &channel.DeviceEntitlementSnapshotRequestedPayload{
				OrgID: testOrgID, AccountDeviceID: testAccountID, VideoCloudDevid: "video-1", ProductID: "33333333-3333-4333-8333-333333333333",
				ProductServiceRevision: 2, ServiceGrantSHA256: strings.Repeat("a", 64), PlatformEntitlementRevision: 1,
				ServiceOptions: []string{"mqtt"}, State: "revoked", RequestedBy: "user-1",
			})
			if err := publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, envelope); !broker.IsTransient(err) || projected {
				t.Fatalf("unexpected publication: err=%v projected=%t", err, projected)
			}
		})
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

func TestPublisherProjectsStaleActivityConflictWithoutRetry(t *testing.T) {
	var projected broker.Message
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"status":"fail","reason":"device activation generation mismatch"}`))
	}))
	defer server.Close()
	publisher, err := NewPublisher(Options{
		BaseURL: server.URL, Token: "token",
		Project: func(_ context.Context, message broker.Message) error { projected = message; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope := requestEnvelope(t, channel.MessageTypeDeviceDeactivateRequested, &channel.DeviceDeactivateRequestedPayload{
		OrgID: testOrgID, AccountDeviceID: testAccountID, VideoCloudDevid: "video-1",
		ActivityID: "activity-old", RequestedBy: "user-1", Reason: "test",
	})
	if err := publisher.Publish(context.Background(), channel.StreamAccountVideoCommands, envelope); err != nil {
		t.Fatal(err)
	}
	if projected.Envelope.MessageType != channel.MessageTypeDeviceDeactivateFailed {
		t.Fatalf("message type = %s", projected.Envelope.MessageType)
	}
	payload, err := projected.Envelope.ValidateAndDecode(channel.StreamVideoAccountEvents)
	if err != nil {
		t.Fatal(err)
	}
	failed := payload.(*channel.DeviceDeactivateFailedPayload)
	if failed.ActivityID != "activity-old" || failed.Retryable || failed.ErrorMessage != "device activation generation mismatch" {
		t.Fatalf("stale generation result = %+v", failed)
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
