package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type failingAuthTokenSink struct{}

func (failingAuthTokenSink) DeliverAuthToken(context.Context, AuthTokenDelivery) error {
	return errors.New("delivery failed")
}

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

func TestHealthRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := New(nil, nil).Router()

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected health 200, got %d", res.Code)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON health body, got %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("expected ok status, got %q", body.Status)
	}
}

func TestAuthTokenDeliveryHook(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	noSinkServer := New(nil, nil)
	if err := noSinkServer.deliverAuthToken(c, "user@example.com", "email_verification", "token", time.Now()); !errors.Is(err, ErrAuthTokenSinkUnavailable) {
		t.Fatalf("expected unavailable sink error, got %v", err)
	}

	errorServer := NewWithAuthTokenSink(nil, nil, failingAuthTokenSink{})
	if err := errorServer.deliverAuthToken(c, "user@example.com", "email_verification", "token", time.Now()); err == nil {
		t.Fatal("expected sink delivery error")
	}
}

func TestLogAuthTokenSinkWritesDelivery(t *testing.T) {
	var buf bytes.Buffer
	sink := NewLogAuthTokenSink(log.New(&buf, "", 0))
	expiresAt := time.Date(2026, 5, 4, 12, 30, 0, 0, time.UTC)

	err := sink.DeliverAuthToken(context.Background(), AuthTokenDelivery{
		Purpose:   "password_reset",
		Email:     "user@example.com",
		Token:     "reset-token",
		ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	got := buf.String()
	for _, want := range []string{
		"purpose=password_reset",
		"email=user@example.com",
		"token=reset-token",
		"expires_at=2026-05-04T12:30:00Z",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected log output to contain %q, got %q", want, got)
		}
	}
}

func TestLogQuotaRaiseNotificationSinkWritesDelivery(t *testing.T) {
	var buf bytes.Buffer
	sink := NewLogQuotaRaiseNotificationSink(log.New(&buf, "", 0))

	approvedQuota := 12
	decisionReason := "approved for pilot"
	recipientName := "Owner"
	err := sink.DeliverQuotaRaiseNotification(context.Background(), QuotaRaiseNotificationDelivery{
		RecipientEmail:   "owner@example.com",
		RecipientName:    &recipientName,
		OrganizationID:   "org-1",
		OrganizationName: "Owner Org",
		RequestedQuota:   8,
		ApprovedQuota:    &approvedQuota,
		DecisionReason:   &decisionReason,
		Decision:         "approved",
	})
	if err != nil {
		t.Fatal(err)
	}

	got := buf.String()
	for _, want := range []string{
		"decision=approved",
		"email=owner@example.com",
		"org_id=org-1",
		"org_name=Owner Org",
		"requested_quota=8",
		"approved_quota=",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected log output to contain %q, got %q", want, got)
		}
	}
}

func TestSMTPQuotaRaiseNotificationSinkWritesDelivery(t *testing.T) {
	sink := NewSMTPQuotaRaiseNotificationSink("smtp.example:587", "no-reply@example.com", nil)
	var gotAddr string
	var gotFrom string
	var gotTo []string
	var gotMsg []byte
	sink.sendMail = func(addr string, _ smtp.Auth, from string, to []string, msg []byte) error {
		gotAddr = addr
		gotFrom = from
		gotTo = append([]string(nil), to...)
		gotMsg = append([]byte(nil), msg...)
		return nil
	}

	approvedQuota := 12
	decisionReason := "approved for pilot"
	recipientName := "Owner"
	err := sink.DeliverQuotaRaiseNotification(context.Background(), QuotaRaiseNotificationDelivery{
		RecipientEmail:   "owner@example.com",
		RecipientName:    &recipientName,
		OrganizationID:   "org-1",
		OrganizationName: "Owner Org",
		RequestedQuota:   8,
		ApprovedQuota:    &approvedQuota,
		DecisionReason:   &decisionReason,
		Decision:         "approved",
	})
	if err != nil {
		t.Fatal(err)
	}

	if gotAddr != "smtp.example:587" || gotFrom != "no-reply@example.com" {
		t.Fatalf("unexpected SMTP envelope: addr=%q from=%q", gotAddr, gotFrom)
	}
	if len(gotTo) != 1 || gotTo[0] != "owner@example.com" {
		t.Fatalf("unexpected recipients: %+v", gotTo)
	}
	for _, want := range []string{
		"To: owner@example.com",
		"From: no-reply@example.com",
		"Subject: Quota raise approved",
		"Quota raise decision: approved",
		"Organization: Owner Org (org-1)",
		"Requester: Owner <owner@example.com>",
		"Requested quota: 8",
		"Approved quota: 12",
		"Decision reason: approved for pilot",
	} {
		if !strings.Contains(string(gotMsg), want) {
			t.Fatalf("expected SMTP message to contain %q, got %q", want, string(gotMsg))
		}
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

	conflictRecorder := httptest.NewRecorder()
	conflictContext, _ := gin.CreateTestContext(conflictRecorder)
	writeStoreError(conflictContext, store.ErrConflict)
	if conflictRecorder.Code != http.StatusConflict {
		t.Fatalf("expected conflict store error 409, got %d", conflictRecorder.Code)
	}

	notFoundRecorder := httptest.NewRecorder()
	notFoundContext, _ := gin.CreateTestContext(notFoundRecorder)
	writeStoreError(notFoundContext, store.ErrNotFound)
	if notFoundRecorder.Code != http.StatusNotFound {
		t.Fatalf("expected not found store error 404, got %d", notFoundRecorder.Code)
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

	rateLimitRecorder := httptest.NewRecorder()
	rateLimitContext, _ := gin.CreateTestContext(rateLimitRecorder)
	writeStoreError(rateLimitContext, store.ErrRateLimited)
	if rateLimitRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("expected rate limited store error 429, got %d", rateLimitRecorder.Code)
	}

	authRateLimitRecorder := httptest.NewRecorder()
	authRateLimitContext, _ := gin.CreateTestContext(authRateLimitRecorder)
	writeAuthTokenStoreError(authRateLimitContext, store.ErrRateLimited, "ignored")
	if authRateLimitRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("expected auth token rate limit 429, got %d", authRateLimitRecorder.Code)
	}

	authDefaultRecorder := httptest.NewRecorder()
	authDefaultContext, _ := gin.CreateTestContext(authDefaultRecorder)
	writeAuthTokenStoreError(authDefaultContext, errors.New("boom"), "Could not issue reset token")
	if authDefaultRecorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected auth token default 500, got %d", authDefaultRecorder.Code)
	}
}

func TestNewAuthTokenAndUnsupportedPurpose(t *testing.T) {
	server := New(nil, nil)
	token, expiresAt, err := server.newAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("expected token")
	}
	if time.Until(expiresAt) < 29*time.Minute || time.Until(expiresAt) > 31*time.Minute {
		t.Fatalf("expected roughly 30 minute expiry, got %s", time.Until(expiresAt))
	}

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	if _, _, err := server.createAuthToken(c, "user-1", "unsupported"); err == nil {
		t.Fatal("expected unsupported token purpose error")
	}
}

func TestAuthRecoveryValidationRejectsInvalidRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := New(nil, nil).Router()

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{
			name:   "verify email missing token",
			method: http.MethodPost,
			path:   "/v1/auth/verify-email",
			body:   `{}`,
		},
		{
			name:   "resend verification invalid email",
			method: http.MethodPost,
			path:   "/v1/auth/resend-verification",
			body:   `{"email":"not-an-email"}`,
		},
		{
			name:   "forgot password invalid email",
			method: http.MethodPost,
			path:   "/v1/auth/forgot-password",
			body:   `{"email":"not-an-email"}`,
		},
		{
			name:   "reset password short new password",
			method: http.MethodPost,
			path:   "/v1/auth/reset-password",
			body:   `{"token":"token","new_password":"short"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("expected validation 400, got %d: %s", res.Code, res.Body.String())
			}
		})
	}
}

func TestAuthRecoveryValidationRejectsBlankTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := New(nil, nil).Router()

	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "verify email blank token",
			path: "/v1/auth/verify-email",
			body: `{"token":"   "}`,
		},
		{
			name: "reset password blank token",
			path: "/v1/auth/reset-password",
			body: `{"token":"   ","new_password":"new-password123"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("expected blank token validation 400, got %d: %s", res.Code, res.Body.String())
			}
		})
	}
}

func TestBindStrictRejectsUnknownFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	type strictRequest struct {
		Name string `json:"name"`
	}

	rejectRecorder := httptest.NewRecorder()
	rejectContext, _ := gin.CreateTestContext(rejectRecorder)
	rejectContext.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"camera","claim_material":{"serial_number":"CAM-001"}}`))
	if bindStrict(rejectContext, &strictRequest{}) {
		t.Fatal("expected unknown field to be rejected")
	}
	if rejectRecorder.Code != http.StatusBadRequest {
		t.Fatalf("expected unknown field 400, got %d", rejectRecorder.Code)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rejectRecorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON error body, got %v", err)
	}
	if body.Error.Code != "invalid_request" || !strings.Contains(body.Error.Message, `unknown field "claim_material"`) {
		t.Fatalf("expected unknown field invalid_request, got %+v", body)
	}

	acceptRecorder := httptest.NewRecorder()
	acceptContext, _ := gin.CreateTestContext(acceptRecorder)
	acceptContext.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"camera"}`))
	var accepted strictRequest
	if !bindStrict(acceptContext, &accepted) {
		t.Fatalf("expected strict request to be accepted: %s", acceptRecorder.Body.String())
	}
	if accepted.Name != "camera" {
		t.Fatalf("expected decoded name, got %+v", accepted)
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
		wantProduct        model.ProductReadinessState
	}{
		{
			name: "accepted provisioning waits for activation",
			device: readinessDevice(model.DeviceStatusUnknown, map[string]any{
				model.DeviceMetadataVideoCloudActivationStatus: string(model.VideoCloudActivationStatusPending),
			}),
			provisioningStatus: model.DeviceOperationStatusPending,
			want:               model.DeviceReadinessStateActivationPending,
			wantProduct:        model.ProductReadinessStateCloudActivationPending,
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
			wantProduct:        model.ProductReadinessStateFailed,
		},
		{
			name: "activation succeeded but offline waits for transport",
			device: readinessDevice(model.DeviceStatusOffline, map[string]any{
				model.DeviceMetadataVideoCloudActivationStatus: string(model.VideoCloudActivationStatusActivated),
			}),
			provisioningStatus: model.DeviceOperationStatusSucceeded,
			want:               model.DeviceReadinessStateTransportPending,
			wantProduct:        model.ProductReadinessStateActivated,
		},
		{
			name: "activation succeeded and online is ready",
			device: readinessDevice(model.DeviceStatusOnline, map[string]any{
				model.DeviceMetadataVideoCloudActivationStatus: string(model.VideoCloudActivationStatusActivated),
			}),
			provisioningStatus: model.DeviceOperationStatusSucceeded,
			want:               model.DeviceReadinessStateReady,
			wantProduct:        model.ProductReadinessStateOnline,
		},
		{
			name: "deactivation pending takes precedence",
			device: readinessDevice(model.DeviceStatusOnline, map[string]any{
				model.DeviceMetadataVideoCloudActivationStatus: string(model.VideoCloudActivationStatusActivated),
			}),
			provisioningStatus: model.DeviceOperationStatusSucceeded,
			deactivationStatus: operationStatusPtr(model.DeviceOperationStatusPublished),
			want:               model.DeviceReadinessStateDeactivationPending,
			wantProduct:        model.ProductReadinessStateDeactivationPending,
		},
		{
			name: "deactivation success is deactivated",
			device: readinessDevice(model.DeviceStatusOffline, map[string]any{
				model.DeviceMetadataVideoCloudActivationStatus: string(model.VideoCloudActivationStatusDeactivated),
			}),
			provisioningStatus: model.DeviceOperationStatusSucceeded,
			deactivationStatus: operationStatusPtr(model.DeviceOperationStatusSucceeded),
			want:               model.DeviceReadinessStateDeactivated,
			wantProduct:        model.ProductReadinessStateDeactivated,
		},
		{
			name:               "registry disabled stays account-side only",
			device:             readinessDisabledDevice(model.DeviceStatusDisabled, nil),
			provisioningStatus: model.DeviceOperationStatusSucceeded,
			want:               model.DeviceReadinessStateDisabled,
			wantProduct:        model.ProductReadinessStateRegistered,
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
			if got.ProductState != tt.wantProduct {
				t.Fatalf("expected product readiness %s, got %+v", tt.wantProduct, got)
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

func readinessDisabledDevice(status model.DeviceStatus, metadata map[string]any) model.Device {
	now := time.Now().UTC()
	device := readinessDevice(status, metadata)
	device.DisabledAt = &now
	return device
}

func operationStatusPtr(status model.DeviceOperationStatus) *model.DeviceOperationStatus {
	return &status
}
