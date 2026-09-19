package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type entitlementSnapshotHandlerStore struct {
	Store
	input store.DeviceEntitlementSnapshotInput
}

func (s *entitlementSnapshotHandlerStore) StartDeviceEntitlementSnapshot(_ context.Context, in store.DeviceEntitlementSnapshotInput) (store.DeviceEntitlementSnapshotResult, error) {
	s.input = in
	return store.DeviceEntitlementSnapshotResult{
		Created: true,
		Snapshot: store.DeviceEntitlementSnapshot{
			OrganizationID: in.OrganizationID, AccountDeviceID: in.DeviceID, Revision: 1,
			ServiceOptions: in.ServiceOptions, State: in.State, OperationID: in.OperationID,
		},
		Operation: model.DeviceOperation{OperationID: in.OperationID, CorrelationID: in.CorrelationID,
			DeviceID: in.DeviceID, OperationType: model.DeviceOperationTypeEntitlementUpdate,
			Status: model.DeviceOperationStatusPending, CreatedAt: in.Now, UpdatedAt: in.Now},
		Message: model.DeviceMessageOutbox{MessageID: in.MessageID},
	}, nil
}

func TestCreateDeviceEntitlementSnapshotHandlerUsesAuthenticatedActorAndReturnsRevision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &entitlementSnapshotHandlerStore{}
	server := &Server{store: fake}
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/v1/orgs/cloud-1/devices/device-1/entitlement-snapshots", strings.NewReader(`{"operation_id":"request-1","service_options":["mqtt"],"state":"revoked"}`))
	context.Params = gin.Params{{Key: "orgId", Value: "cloud-1"}, {Key: "deviceId", Value: "device-1"}}
	context.Set("userID", "authenticated-actor")
	server.createDeviceEntitlementSnapshot(context)
	if recorder.Code != http.StatusAccepted || fake.input.RequestedBy != "authenticated-actor" ||
		fake.input.OperationID != "request-1" || fake.input.State != "revoked" ||
		len(fake.input.ServiceOptions) != 1 || fake.input.ServiceOptions[0] != "mqtt" {
		t.Fatalf("status=%d input=%+v body=%s", recorder.Code, fake.input, recorder.Body.String())
	}
	var response struct {
		Snapshot struct {
			Revision int64  `json:"revision"`
			State    string `json:"state"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Snapshot.Revision != 1 || response.Snapshot.State != "revoked" {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	if fake.input.Now.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("stale timestamp: %s", fake.input.Now)
	}
}

func TestEntitlementSnapshotRouteIsGatedAndAuthenticated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := "/v1/orgs/11111111-1111-4111-8111-111111111111/devices/22222222-2222-4222-8222-222222222222/entitlement-snapshots"
	for _, tt := range []struct {
		name    string
		enabled bool
		want    int
	}{
		{name: "disabled", want: http.StatusNotFound},
		{name: "enabled_requires_auth", enabled: true, want: http.StatusUnauthorized},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := New(&entitlementSnapshotHandlerStore{}, auth.NewService("access-secret", "refresh-secret", time.Minute, time.Hour))
			server.ConfigurePlatformServiceProductWrites(tt.enabled)
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"state":"revoked"}`)))
			if response.Code != tt.want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
