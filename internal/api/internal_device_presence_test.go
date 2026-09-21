package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type presenceHandlerStore struct {
	Store
	created    *store.DeviceMessageInboxCreateInput
	transition *store.InboxProcessTransitionInput
	status     model.DeviceMessageInboxStatus
	createErr  error
}

func (s *presenceHandlerStore) CreateOrGetInboxMessage(_ context.Context, in store.DeviceMessageInboxCreateInput) (model.DeviceMessageInbox, bool, error) {
	s.created = &in
	if s.createErr != nil {
		return model.DeviceMessageInbox{}, false, s.createErr
	}
	status := s.status
	if status == "" {
		status = model.DeviceMessageInboxStatusRetrying
	}
	return model.DeviceMessageInbox{MessageID: in.MessageID, Status: status}, true, nil
}

func (s *presenceHandlerStore) RecordInboxProcessTransition(_ context.Context, in store.InboxProcessTransitionInput) (store.InboxProcessTransitionResult, error) {
	s.transition = &in
	return store.InboxProcessTransitionResult{}, nil
}

func TestInternalDevicePresenceEventRequiresAuthAndValidEnvelope(t *testing.T) {
	storage := &presenceHandlerStore{}
	server := New(storage, auth.NewService("access-secret", "refresh-secret", time.Minute, time.Hour))
	router := server.Router()
	request := func(token string, envelope channel.Envelope) *httptest.ResponseRecorder {
		body, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/internal/device-presence-events", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	event := presenceTestEnvelope(t)
	if got := request("secret", event); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured internal auth status=%d body=%s", got.Code, got.Body.String())
	}
	server.ConfigureInternalAuthToken("secret")
	if got := request("wrong", event); got.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status=%d body=%s", got.Code, got.Body.String())
	}
	event.MessageType = channel.MessageTypeDeviceMetadataChanged
	if got := request("secret", event); got.Code != http.StatusBadRequest {
		t.Fatalf("wrong event type status=%d body=%s", got.Code, got.Body.String())
	}
	event = presenceTestEnvelope(t)
	event.PartitionKey = "another-device"
	if got := request("secret", event); got.Code != http.StatusBadRequest {
		t.Fatalf("partition mismatch status=%d body=%s", got.Code, got.Body.String())
	}
	if storage.created != nil {
		t.Fatal("invalid or unauthorized event reached inbox")
	}
}

func TestInternalDevicePresenceEventProjectsAndReplaysIdempotently(t *testing.T) {
	storage := &presenceHandlerStore{}
	server := New(storage, auth.NewService("access-secret", "refresh-secret", time.Minute, time.Hour))
	server.ConfigureInternalAuthToken("secret")
	router := server.Router()
	event := presenceTestEnvelope(t)
	request := func() *httptest.ResponseRecorder {
		body, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/internal/device-presence-events", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	if got := request(); got.Code != http.StatusOK {
		t.Fatalf("valid event status=%d body=%s", got.Code, got.Body.String())
	}
	if storage.created == nil || storage.created.MessageType != string(channel.MessageTypeDeviceOnlineChanged) || storage.created.PartitionKey != event.PartitionKey {
		t.Fatalf("inbox input=%+v", storage.created)
	}
	if storage.transition == nil || storage.transition.Projection == nil || storage.transition.Projection.Status == nil ||
		*storage.transition.Projection.Status != model.DeviceStatusOnline || storage.transition.OperationStatus != nil {
		t.Fatalf("projection transition=%+v", storage.transition)
	}
	storage.status = model.DeviceMessageInboxStatusProcessed
	storage.transition = nil
	if got := request(); got.Code != http.StatusOK || storage.transition != nil {
		t.Fatalf("processed replay status=%d transition=%+v", got.Code, storage.transition)
	}
	storage.createErr = store.ErrConflict
	if got := request(); got.Code != http.StatusConflict {
		t.Fatalf("conflicting replay status=%d body=%s", got.Code, got.Body.String())
	}
}

func presenceTestEnvelope(t *testing.T) channel.Envelope {
	t.Helper()
	observedAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	payload, err := json.Marshal(channel.DeviceOnlineChangedPayload{
		OrgID:           "11111111-1111-1111-1111-111111111111",
		AccountDeviceID: "22222222-2222-2222-2222-222222222222",
		VideoCloudDevid: "video-1",
		Status:          channel.OnlineStatusOnline,
		LastSeenAt:      observedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return channel.Envelope{
		MessageID: "presence-1", CorrelationID: "provision-1", OperationID: "provision-1",
		SourceService: channel.ServiceRealtekVideoCloud, TargetService: channel.ServiceAccountManager,
		MessageType: channel.MessageTypeDeviceOnlineChanged, SchemaVersion: channel.SchemaVersionV1,
		PartitionKey: "22222222-2222-2222-2222-222222222222", OccurredAt: observedAt, Payload: payload,
	}
}
