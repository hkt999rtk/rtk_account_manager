package api

import (
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

	defaultRecorder := httptest.NewRecorder()
	defaultContext, _ := gin.CreateTestContext(defaultRecorder)
	writeStoreError(defaultContext, errors.New("boom"))
	if defaultRecorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected internal error 500, got %d", defaultRecorder.Code)
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
