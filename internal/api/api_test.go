package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

func TestRequireAuthRejectsMissingToken(t *testing.T) {
	server := New(nil, auth.NewService("access-secret", "refresh-secret", time.Minute, time.Hour))
	router := server.Router()

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", res.Code)
	}
}

func TestRequireAuthRejectsInvalidToken(t *testing.T) {
	server := New(nil, auth.NewService("access-secret", "refresh-secret", time.Minute, time.Hour))
	router := server.Router()

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", res.Code)
	}
}

func TestRequireAuthRejectsRefreshTokenAsBearer(t *testing.T) {
	authService := auth.NewService("access-secret", "refresh-secret", time.Minute, time.Hour)
	refreshToken, _, err := authService.IssueRefreshToken("user-1")
	if err != nil {
		t.Fatal(err)
	}
	server := New(nil, authService)
	router := server.Router()

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+refreshToken)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", res.Code)
	}
}

func TestPaginationClampsAndDefaultsValues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/orgs?limit=250&offset=bad", nil)

	limit, offset := pagination(c)
	if limit != 200 || offset != 0 {
		t.Fatalf("expected limit 200 and offset 0, got limit=%d offset=%d", limit, offset)
	}

	c, _ = gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/orgs?limit=0&offset=3", nil)
	limit, offset = pagination(c)
	if limit != 50 || offset != 3 {
		t.Fatalf("expected limit 50 and offset 3, got limit=%d offset=%d", limit, offset)
	}
}

func TestTrimPtrNormalizesOptionalStrings(t *testing.T) {
	if trimPtr(nil) != nil {
		t.Fatal("expected nil pointer to stay nil")
	}
	blank := "   "
	if trimPtr(&blank) != nil {
		t.Fatal("expected blank string to become nil")
	}
	value := "  camera  "
	trimmed := trimPtr(&value)
	if trimmed == nil || *trimmed != "camera" {
		t.Fatalf("expected trimmed value, got %v", trimmed)
	}
}

func TestValidationHelpersWriteErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	roleRecorder := httptest.NewRecorder()
	roleContext, _ := gin.CreateTestContext(roleRecorder)
	if validRole(roleContext, model.Role("invalid")) {
		t.Fatal("expected invalid role")
	}
	if roleRecorder.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid role 400, got %d", roleRecorder.Code)
	}

	errRecorder := httptest.NewRecorder()
	errContext, _ := gin.CreateTestContext(errRecorder)
	writeStoreError(errContext, store.ErrDisabled)
	if errRecorder.Code != http.StatusConflict {
		t.Fatalf("expected disabled resource 409, got %d", errRecorder.Code)
	}

	notProvisionedRecorder := httptest.NewRecorder()
	notProvisionedContext, _ := gin.CreateTestContext(notProvisionedRecorder)
	writeStoreError(notProvisionedContext, store.ErrNotProvisioned)
	if notProvisionedRecorder.Code != http.StatusConflict {
		t.Fatalf("expected not provisioned resource 409, got %d", notProvisionedRecorder.Code)
	}

	defaultRecorder := httptest.NewRecorder()
	defaultContext, _ := gin.CreateTestContext(defaultRecorder)
	writeStoreError(defaultContext, errors.New("boom"))
	if defaultRecorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected internal error 500, got %d", defaultRecorder.Code)
	}

	inconsistentRecorder := httptest.NewRecorder()
	inconsistentContext, _ := gin.CreateTestContext(inconsistentRecorder)
	writeStoreError(inconsistentContext, errOperationStateInconsistent)
	if inconsistentRecorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected inconsistent operation state 500, got %d", inconsistentRecorder.Code)
	}
	var inconsistentBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(inconsistentRecorder.Body.Bytes(), &inconsistentBody); err != nil {
		t.Fatalf("expected JSON error body, got %v", err)
	}
	if inconsistentBody.Error.Code != "operation_state_inconsistent" {
		t.Fatalf("expected operation_state_inconsistent error code, got %+v", inconsistentBody)
	}
}

func TestRejectUnsupportedClaimMaterial(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name string
		req  provisionRequest
	}{
		{
			name: "standalone claim material object",
			req: provisionRequest{
				ClaimMaterial: map[string]any{"serial_number": "CAM-001"},
			},
		},
		{
			name: "qr payload",
			req: provisionRequest{
				VideoCloudDevid: "video-device-1",
				ActivityID:      "activity-1",
				ClipPublicKey:   "clip-key-1",
				QRPayload:       stringPtr("rtkc://claim/device-http-1"),
			},
		},
		{
			name: "qr code synonym",
			req: provisionRequest{
				QRCode: stringPtr("rtk:claim:payload"),
			},
		},
		{
			name: "serial number",
			req: provisionRequest{
				VideoCloudDevid: "video-device-1",
				ActivityID:      "activity-1",
				ClipPublicKey:   "clip-key-1",
				SerialNumber:    stringPtr("CAM-001"),
			},
		},
		{
			name: "activation code",
			req: provisionRequest{
				ActivationCode: stringPtr("ACT-123456"),
			},
		},
		{
			name: "mac address",
			req: provisionRequest{
				MACAddress: stringPtr("00:11:22:33:44:55"),
			},
		},
		{
			name: "factory identity",
			req: provisionRequest{
				FactoryIdentity: stringPtr("factory-device-1"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)

			if rejectUnsupportedClaimMaterial(context, tt.req) {
				t.Fatal("expected unsupported claim material to be rejected")
			}
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", recorder.Code)
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("expected JSON error body, got %v", err)
			}
			if body.Error.Code != "unsupported_claim_material" {
				t.Fatalf("expected unsupported_claim_material error code, got %+v", body)
			}
		})
	}

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	if !rejectUnsupportedClaimMaterial(context, provisionRequest{
		VideoCloudDevid: "video-device-1",
		ActivityID:      "activity-1",
		ClipPublicKey:   "clip-key-1",
	}) {
		t.Fatal("expected current video claim material to be accepted")
	}
}

func TestMatchExistingProvisionOperation(t *testing.T) {
	existing := model.DeviceOperation{
		OperationID:    "provision-op-1",
		CorrelationID:  "provision-op-1",
		OrganizationID: "org-1",
		DeviceID:       "device-1",
		OperationType:  model.DeviceOperationTypeProvision,
		RequestPayload: map[string]any{
			"video_cloud_devid": "video-device-1",
			"activity_id":       "activity-1",
			"clip_public_key":   "clip-key-1",
		},
	}

	if err := matchExistingProvisionOperation(existing, "provision-op-1", "org-1", "device-1", map[string]any{
		"video_cloud_devid": "video-device-1",
		"activity_id":       "activity-1",
		"clip_public_key":   "clip-key-1",
	}); err != nil {
		t.Fatalf("expected matching provision payload to reuse operation, got %v", err)
	}

	if err := matchExistingProvisionOperation(existing, "provision-op-1", "org-1", "device-1", map[string]any{
		"video_cloud_devid": "video-device-1",
		"activity_id":       "activity-2",
		"clip_public_key":   "clip-key-1",
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected conflicting provision payload to be rejected, got %v", err)
	}
}

func TestMatchExistingDeactivateOperation(t *testing.T) {
	existing := model.DeviceOperation{
		OperationID:    "deactivate-op-1",
		CorrelationID:  "deactivate-op-1",
		OrganizationID: "org-1",
		DeviceID:       "device-1",
		OperationType:  model.DeviceOperationTypeDeactivate,
		RequestPayload: map[string]any{
			"video_cloud_devid": "video-device-1",
			"reason":            "account_device_disabled",
		},
	}

	if err := matchExistingDeactivateOperation(existing, "deactivate-op-1", "org-1", "device-1", "account_device_disabled", map[string]any{}); err != nil {
		t.Fatalf("expected matching deactivate reason to reuse operation without live metadata, got %v", err)
	}

	if err := matchExistingDeactivateOperation(existing, "deactivate-op-1", "org-1", "device-1", "user_request", map[string]any{}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected conflicting deactivate reason to be rejected, got %v", err)
	}

	if err := matchExistingDeactivateOperation(existing, "deactivate-op-1", "org-1", "device-1", "account_device_disabled", map[string]any{
		model.DeviceMetadataVideoCloudDevid: "video-device-2",
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected conflicting live metadata to be rejected, got %v", err)
	}
}

func TestReadinessFromProjectionStates(t *testing.T) {
	tests := []struct {
		name               string
		device             model.Device
		provisioningStatus model.DeviceOperationStatus
		deactivationStatus *model.DeviceOperationStatus
		want               model.DeviceReadinessState
	}{
		{
			name: "accepted provisioning waits for activation",
			device: readinessDevice(model.DeviceStatusUnknown, map[string]any{
				model.DeviceMetadataVideoCloudActivationStatus: string(model.VideoCloudActivationStatusPending),
			}),
			provisioningStatus: model.DeviceOperationStatusPending,
			want:               model.DeviceReadinessStateActivationPending,
		},
		{
			name: "activation failure stays visible",
			device: readinessDevice(model.DeviceStatusUnknown, map[string]any{
				model.DeviceMetadataVideoCloudActivationStatus: string(model.VideoCloudActivationStatusFailed),
				model.DeviceMetadataVideoCloudLastError: map[string]any{
					"code": "upstream_timeout",
				},
			}),
			provisioningStatus: model.DeviceOperationStatusFailed,
			want:               model.DeviceReadinessStateActivationFailed,
		},
		{
			name: "activation succeeded but offline waits for transport",
			device: readinessDevice(model.DeviceStatusOffline, map[string]any{
				model.DeviceMetadataVideoCloudActivationStatus: string(model.VideoCloudActivationStatusActivated),
			}),
			provisioningStatus: model.DeviceOperationStatusSucceeded,
			want:               model.DeviceReadinessStateTransportPending,
		},
		{
			name: "activation succeeded and online is ready",
			device: readinessDevice(model.DeviceStatusOnline, map[string]any{
				model.DeviceMetadataVideoCloudActivationStatus: string(model.VideoCloudActivationStatusActivated),
			}),
			provisioningStatus: model.DeviceOperationStatusSucceeded,
			want:               model.DeviceReadinessStateReady,
		},
		{
			name: "deactivation pending takes precedence",
			device: readinessDevice(model.DeviceStatusOnline, map[string]any{
				model.DeviceMetadataVideoCloudActivationStatus: string(model.VideoCloudActivationStatusActivated),
			}),
			provisioningStatus: model.DeviceOperationStatusSucceeded,
			deactivationStatus: operationStatusPtr(model.DeviceOperationStatusPublished),
			want:               model.DeviceReadinessStateDeactivationPending,
		},
		{
			name: "deactivation success is deactivated",
			device: readinessDevice(model.DeviceStatusOffline, map[string]any{
				model.DeviceMetadataVideoCloudActivationStatus: string(model.VideoCloudActivationStatusDeactivated),
			}),
			provisioningStatus: model.DeviceOperationStatusSucceeded,
			deactivationStatus: operationStatusPtr(model.DeviceOperationStatusSucceeded),
			want:               model.DeviceReadinessStateDeactivated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var latestDeactivation *model.DeviceOperation
			if tt.deactivationStatus != nil {
				latestDeactivation = &model.DeviceOperation{
					OperationType: model.DeviceOperationTypeDeactivate,
					Status:        *tt.deactivationStatus,
				}
			}

			got := readinessFromProjection(tt.device, model.DeviceOperation{
				OperationType: model.DeviceOperationTypeProvision,
				Status:        tt.provisioningStatus,
			}, latestDeactivation)

			if got.State != tt.want {
				t.Fatalf("expected readiness %s, got %+v", tt.want, got)
			}
			if got.Sources.DeviceStatus != tt.device.Status || got.Sources.ProvisioningOperationStatus != tt.provisioningStatus {
				t.Fatalf("expected source facts to reflect device and operation, got %+v", got.Sources)
			}
			if tt.deactivationStatus != nil && (got.Sources.DeactivationOperationStatus == nil || *got.Sources.DeactivationOperationStatus != *tt.deactivationStatus) {
				t.Fatalf("expected deactivation source status %s, got %+v", *tt.deactivationStatus, got.Sources.DeactivationOperationStatus)
			}
		})
	}
}

func readinessDevice(status model.DeviceStatus, metadata map[string]any) model.Device {
	return model.Device{
		Status:   status,
		Metadata: metadata,
	}
}

func operationStatusPtr(status model.DeviceOperationStatus) *model.DeviceOperationStatus {
	return &status
}
