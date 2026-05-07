package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
	"rtk_account_manager/internal/testutil"
)

type integrationEnv struct {
	router           *gin.Engine
	db               *pgxpool.Pool
	tokenSink        *recordingAuthTokenSink
	notificationSink *recordingQuotaRaiseNotificationSink
}

type recordingAuthTokenSink struct {
	deliveries []AuthTokenDelivery
}

func (s *recordingAuthTokenSink) DeliverAuthToken(_ context.Context, delivery AuthTokenDelivery) error {
	s.deliveries = append(s.deliveries, delivery)
	return nil
}

type recordingQuotaRaiseNotificationSink struct {
	deliveries []QuotaRaiseNotificationDelivery
}

func (s *recordingQuotaRaiseNotificationSink) DeliverQuotaRaiseNotification(_ context.Context, delivery QuotaRaiseNotificationDelivery) error {
	s.deliveries = append(s.deliveries, delivery)
	return nil
}

func newIntegrationEnv(t *testing.T) integrationEnv {
	t.Helper()

	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := database.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)

	testutil.LockIntegrationDatabase(t, db)

	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		TRUNCATE auth_tokens, quota_raise_requests, refresh_tokens, device_tags, device_group_members, device_groups, devices, organization_members, organizations, users
		RESTART IDENTITY CASCADE
	`); err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	authService := auth.NewService("test-access-secret", "test-refresh-secret", time.Minute, time.Hour)
	tokenSink := &recordingAuthTokenSink{}
	notificationSink := &recordingQuotaRaiseNotificationSink{}
	return integrationEnv{
		router:           NewWithAuthTokenAndNotificationSink(store.New(db), authService, tokenSink, notificationSink).Router(),
		db:               db,
		tokenSink:        tokenSink,
		notificationSink: notificationSink,
	}
}

func TestIntegrationRegisterLoginRefreshAndLogout(t *testing.T) {
	env := newIntegrationEnv(t)

	registered := registerUser(t, env.router, "owner@example.com", "Owner Org")
	if registered.User.ID == "" || registered.Organization.ID == "" {
		t.Fatal("expected user and organization IDs")
	}

	meRes := performJSON(env.router, http.MethodGet, "/v1/me", nil, registered.Tokens.AccessToken)
	if meRes.Code != http.StatusOK {
		t.Fatalf("expected me 200, got %d: %s", meRes.Code, meRes.Body.String())
	}
	meBody := decodeBody[meBody](t, meRes)
	if meBody.User.ID != registered.User.ID || len(meBody.Organizations) != 1 || meBody.Organizations[0].ID != registered.Organization.ID {
		t.Fatalf("unexpected me response: %+v", meBody)
	}

	loginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "owner@example.com",
		"password": "password123",
	}, "")
	if loginRes.Code != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", loginRes.Code, loginRes.Body.String())
	}

	loginBody := decodeBody[tokenBody](t, loginRes)
	refreshRes := performJSON(env.router, http.MethodPost, "/v1/auth/refresh", map[string]any{
		"refresh_token": loginBody.Tokens.RefreshToken,
	}, "")
	if refreshRes.Code != http.StatusOK {
		t.Fatalf("expected refresh 200, got %d: %s", refreshRes.Code, refreshRes.Body.String())
	}

	oldRefreshRes := performJSON(env.router, http.MethodPost, "/v1/auth/refresh", map[string]any{
		"refresh_token": loginBody.Tokens.RefreshToken,
	}, "")
	if oldRefreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected old refresh token to be revoked, got %d", oldRefreshRes.Code)
	}

	refreshedBody := decodeBody[tokenBody](t, refreshRes)
	logoutRes := performJSON(env.router, http.MethodPost, "/v1/auth/logout", map[string]any{
		"refresh_token": refreshedBody.Tokens.RefreshToken,
	}, refreshedBody.Tokens.AccessToken)
	if logoutRes.Code != http.StatusNoContent {
		t.Fatalf("expected logout 204, got %d", logoutRes.Code)
	}

	revokedRefreshRes := performJSON(env.router, http.MethodPost, "/v1/auth/refresh", map[string]any{
		"refresh_token": refreshedBody.Tokens.RefreshToken,
	}, "")
	if revokedRefreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected logged-out refresh token 401, got %d", revokedRefreshRes.Code)
	}

	invalidLoginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "owner@example.com",
		"password": "wrong-password",
	}, "")
	if invalidLoginRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected invalid login 401, got %d", invalidLoginRes.Code)
	}

	invalidRefreshRes := performJSON(env.router, http.MethodPost, "/v1/auth/refresh", map[string]any{
		"refresh_token": "not-a-token",
	}, "")
	if invalidRefreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected invalid refresh token 401, got %d", invalidRefreshRes.Code)
	}
}

func TestIntegrationEmailVerificationAndPasswordRecovery(t *testing.T) {
	env := newIntegrationEnv(t)

	registered := registerUser(t, env.router, "verify@example.com", "Verify Org")
	if registered.User.EmailVerified {
		t.Fatal("expected newly registered user to start unverified")
	}

	var issuedVerificationTokens int
	if err := env.db.QueryRow(context.Background(), `
		SELECT count(*) FROM auth_tokens WHERE user_id = $1 AND purpose = 'email_verification'
	`, registered.User.ID).Scan(&issuedVerificationTokens); err != nil {
		t.Fatal(err)
	}
	if issuedVerificationTokens != 1 {
		t.Fatalf("expected registration to issue one verification token, got %d", issuedVerificationTokens)
	}

	accountStore := store.New(env.db)
	verificationToken := latestAuthToken(t, env.tokenSink, "verify@example.com", "email_verification")
	verifyRes := performJSON(env.router, http.MethodPost, "/v1/auth/verify-email", map[string]any{
		"token": verificationToken,
	}, "")
	if verifyRes.Code != http.StatusOK {
		t.Fatalf("expected verify email 200, got %d: %s", verifyRes.Code, verifyRes.Body.String())
	}
	verified := decodeBody[userBody](t, verifyRes)
	if !verified.User.EmailVerified || verified.User.EmailVerifiedAt == nil {
		t.Fatalf("expected verified user response, got %+v", verified.User)
	}
	reuseVerifyRes := performJSON(env.router, http.MethodPost, "/v1/auth/verify-email", map[string]any{
		"token": verificationToken,
	}, "")
	if reuseVerifyRes.Code != http.StatusBadRequest {
		t.Fatalf("expected consumed verification token 400, got %d", reuseVerifyRes.Code)
	}

	resendTarget := registerUser(t, env.router, "resend@example.com", "Resend Org")
	resendRes := performJSON(env.router, http.MethodPost, "/v1/auth/resend-verification", map[string]any{
		"email": "resend@example.com",
	}, "")
	if resendRes.Code != http.StatusAccepted {
		t.Fatalf("expected resend for unverified user 202, got %d", resendRes.Code)
	}
	resendToken := latestAuthToken(t, env.tokenSink, "resend@example.com", "email_verification")
	verifyResendRes := performJSON(env.router, http.MethodPost, "/v1/auth/verify-email", map[string]any{
		"token": resendToken,
	}, "")
	if verifyResendRes.Code != http.StatusOK {
		t.Fatalf("expected resent verification token 200, got %d: %s", verifyResendRes.Code, verifyResendRes.Body.String())
	}
	if resendTarget.User.ID == "" {
		t.Fatal("expected resend target user id")
	}
	registerUser(t, env.router, "resend-delivery-failure@example.com", "Resend Delivery Failure Org")
	failingDeliveryRouter := NewWithAuthTokenSink(
		accountStore,
		auth.NewService("test-access-secret", "test-refresh-secret", time.Minute, time.Hour),
		failingAuthTokenSink{},
	).Router()
	registerDeliveryFailureRes := performJSON(failingDeliveryRouter, http.MethodPost, "/v1/auth/register", map[string]any{
		"email":             "delivery-failure@example.com",
		"password":          "password123",
		"display_name":      "delivery failure",
		"organization_name": "Delivery Failure Org",
	}, "")
	if registerDeliveryFailureRes.Code != http.StatusInternalServerError {
		t.Fatalf("expected register delivery failure 500, got %d", registerDeliveryFailureRes.Code)
	}
	resendDeliveryFailureRes := performJSON(failingDeliveryRouter, http.MethodPost, "/v1/auth/resend-verification", map[string]any{
		"email": "resend-delivery-failure@example.com",
	}, "")
	if resendDeliveryFailureRes.Code != http.StatusAccepted {
		t.Fatalf("expected resend delivery failure to remain enumeration-safe 202, got %d", resendDeliveryFailureRes.Code)
	}
	forgotDeliveryFailureRes := performJSON(failingDeliveryRouter, http.MethodPost, "/v1/auth/forgot-password", map[string]any{
		"email": "resend@example.com",
	}, "")
	if forgotDeliveryFailureRes.Code != http.StatusAccepted {
		t.Fatalf("expected forgot delivery failure to remain enumeration-safe 202, got %d", forgotDeliveryFailureRes.Code)
	}

	resendVerifiedRes := performJSON(env.router, http.MethodPost, "/v1/auth/resend-verification", map[string]any{
		"email": "verify@example.com",
	}, "")
	if resendVerifiedRes.Code != http.StatusAccepted {
		t.Fatalf("expected resend for verified user to stay enumeration-safe 202, got %d", resendVerifiedRes.Code)
	}
	createdUnknownVerification, err := accountStore.CreateEmailVerificationTokenForEmail(context.Background(), "missing@example.com", auth.HashToken("missing-verification"), time.Now().Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if createdUnknownVerification {
		t.Fatal("expected unknown verification email not to create a token")
	}
	createdVerifiedVerification, err := accountStore.CreateEmailVerificationTokenForEmail(context.Background(), "verify@example.com", auth.HashToken("verified-verification"), time.Now().Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if createdVerifiedVerification {
		t.Fatal("expected verified user not to create another verification token")
	}

	expiredVerificationToken := "expired-verification-token"
	if err := accountStore.CreateEmailVerificationToken(context.Background(), registered.User.ID, auth.HashToken(expiredVerificationToken), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	expiredVerifyRes := performJSON(env.router, http.MethodPost, "/v1/auth/verify-email", map[string]any{
		"token": expiredVerificationToken,
	}, "")
	if expiredVerifyRes.Code != http.StatusBadRequest {
		t.Fatalf("expected expired verification token 400, got %d", expiredVerifyRes.Code)
	}

	forgotUnknownRes := performJSON(env.router, http.MethodPost, "/v1/auth/forgot-password", map[string]any{
		"email": "unknown@example.com",
	}, "")
	if forgotUnknownRes.Code != http.StatusAccepted {
		t.Fatalf("expected unknown forgot-password to stay enumeration-safe 202, got %d", forgotUnknownRes.Code)
	}
	createdUnknownReset, err := accountStore.CreatePasswordResetTokenForEmail(context.Background(), "missing@example.com", auth.HashToken("missing-reset"), time.Now().Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if createdUnknownReset {
		t.Fatal("expected unknown reset email not to create a token")
	}
	forgotRes := performJSON(env.router, http.MethodPost, "/v1/auth/forgot-password", map[string]any{
		"email": "verify@example.com",
	}, "")
	if forgotRes.Code != http.StatusAccepted {
		t.Fatalf("expected forgot-password 202, got %d: %s", forgotRes.Code, forgotRes.Body.String())
	}
	rateLimited := registerUser(t, env.router, "rate-limit@example.com", "Rate Limit Org")
	for i := 0; i < 5; i++ {
		if err := accountStore.CreatePasswordResetToken(context.Background(), rateLimited.User.ID, auth.HashToken("rate-limit-reset-"+strconv.Itoa(i)), time.Now().Add(30*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := accountStore.CreatePasswordResetToken(context.Background(), rateLimited.User.ID, auth.HashToken("rate-limit-reset-final"), time.Now().Add(30*time.Minute)); !errors.Is(err, store.ErrRateLimited) {
		t.Fatalf("expected password reset token throttling, got %v", err)
	}
	forgotRateLimitedRes := performJSON(env.router, http.MethodPost, "/v1/auth/forgot-password", map[string]any{
		"email": "rate-limit@example.com",
	}, "")
	if forgotRateLimitedRes.Code != http.StatusAccepted {
		t.Fatalf("expected forgot-password throttling to remain enumeration-safe 202, got %d", forgotRateLimitedRes.Code)
	}
	resendRateLimited := registerUser(t, env.router, "resend-rate-limit@example.com", "Resend Rate Limit Org")
	for i := 0; i < 4; i++ {
		if err := accountStore.CreateEmailVerificationToken(context.Background(), resendRateLimited.User.ID, auth.HashToken("rate-limit-verify-"+strconv.Itoa(i)), time.Now().Add(30*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	resendRateLimitedRes := performJSON(env.router, http.MethodPost, "/v1/auth/resend-verification", map[string]any{
		"email": "resend-rate-limit@example.com",
	}, "")
	if resendRateLimitedRes.Code != http.StatusAccepted {
		t.Fatalf("expected resend-verification throttling to remain enumeration-safe 202, got %d", resendRateLimitedRes.Code)
	}

	expiredResetUser := registerUser(t, env.router, "expired-reset@example.com", "Expired Reset Org")
	expiredResetToken := "expired-reset-token"
	if err := accountStore.CreatePasswordResetToken(context.Background(), expiredResetUser.User.ID, auth.HashToken(expiredResetToken), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	expiredResetRes := performJSON(env.router, http.MethodPost, "/v1/auth/reset-password", map[string]any{
		"token":        expiredResetToken,
		"new_password": "expired-reset123",
	}, "")
	if expiredResetRes.Code != http.StatusBadRequest {
		t.Fatalf("expected expired reset token 400, got %d", expiredResetRes.Code)
	}

	resetToken := latestAuthToken(t, env.tokenSink, "verify@example.com", "password_reset")
	resetRes := performJSON(env.router, http.MethodPost, "/v1/auth/reset-password", map[string]any{
		"token":        resetToken,
		"new_password": "reset-password123",
	}, "")
	if resetRes.Code != http.StatusNoContent {
		t.Fatalf("expected reset password 204, got %d: %s", resetRes.Code, resetRes.Body.String())
	}
	reuseResetRes := performJSON(env.router, http.MethodPost, "/v1/auth/reset-password", map[string]any{
		"token":        resetToken,
		"new_password": "second-reset123",
	}, "")
	if reuseResetRes.Code != http.StatusBadRequest {
		t.Fatalf("expected consumed reset token 400, got %d", reuseResetRes.Code)
	}
	oldPasswordLoginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "verify@example.com",
		"password": "password123",
	}, "")
	if oldPasswordLoginRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected old password to fail after reset, got %d", oldPasswordLoginRes.Code)
	}
	revokedRefreshRes := performJSON(env.router, http.MethodPost, "/v1/auth/refresh", map[string]any{
		"refresh_token": registered.Tokens.RefreshToken,
	}, "")
	if revokedRefreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected reset to revoke active refresh tokens, got %d", revokedRefreshRes.Code)
	}
	newPasswordLoginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "verify@example.com",
		"password": "reset-password123",
	}, "")
	if newPasswordLoginRes.Code != http.StatusOK {
		t.Fatalf("expected new password login 200, got %d: %s", newPasswordLoginRes.Code, newPasswordLoginRes.Body.String())
	}

	disabled := registerUser(t, env.router, "recovery-disabled@example.com", "Recovery Disabled Org")
	disabledVerificationToken := latestAuthToken(t, env.tokenSink, "recovery-disabled@example.com", "email_verification")
	if _, err := env.db.Exec(context.Background(), `
		UPDATE users SET disabled_at = now(), updated_at = now() WHERE id = $1
	`, disabled.User.ID); err != nil {
		t.Fatal(err)
	}
	disabledVerifyRes := performJSON(env.router, http.MethodPost, "/v1/auth/verify-email", map[string]any{
		"token": disabledVerificationToken,
	}, "")
	if disabledVerifyRes.Code != http.StatusBadRequest {
		t.Fatalf("expected disabled user verification token 400, got %d", disabledVerifyRes.Code)
	}
	if err := accountStore.CreatePasswordResetToken(context.Background(), disabled.User.ID, auth.HashToken("disabled-reset-token"), time.Now().Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	disabledResetRes := performJSON(env.router, http.MethodPost, "/v1/auth/reset-password", map[string]any{
		"token":        "disabled-reset-token",
		"new_password": "disabled-reset123",
	}, "")
	if disabledResetRes.Code != http.StatusBadRequest {
		t.Fatalf("expected disabled user reset token 400, got %d", disabledResetRes.Code)
	}
	disabledForgotRes := performJSON(env.router, http.MethodPost, "/v1/auth/forgot-password", map[string]any{
		"email": "recovery-disabled@example.com",
	}, "")
	if disabledForgotRes.Code != http.StatusAccepted {
		t.Fatalf("expected disabled forgot-password to remain enumeration-safe 202, got %d", disabledForgotRes.Code)
	}
	disabledResendRes := performJSON(env.router, http.MethodPost, "/v1/auth/resend-verification", map[string]any{
		"email": "recovery-disabled@example.com",
	}, "")
	if disabledResendRes.Code != http.StatusAccepted {
		t.Fatalf("expected disabled resend-verification to remain enumeration-safe 202, got %d", disabledResendRes.Code)
	}
}

func TestIntegrationSignupEvaluationQuotaAndRaiseWorkflow(t *testing.T) {
	env := newIntegrationEnv(t)

	signupRes := performJSON(env.router, http.MethodPost, "/v1/auth/signup", map[string]any{
		"email":             "eval@example.com",
		"password":          "password123",
		"display_name":      "Eval User",
		"organization_name": "Eval Org",
	}, "")
	if signupRes.Code != http.StatusAccepted {
		t.Fatalf("expected signup 202, got %d: %s", signupRes.Code, signupRes.Body.String())
	}
	signupBody := decodeBody[signupBody](t, signupRes)
	if !signupBody.User.SignupPendingVerification || signupBody.User.EmailVerified {
		t.Fatalf("expected signup-pending user, got %+v", signupBody.User)
	}
	if signupBody.Organization.Tier != string(model.OrganizationTierEvaluation) || signupBody.Organization.EvaluationDeviceQuota != 5 {
		t.Fatalf("expected evaluation org quota defaults, got %+v", signupBody.Organization)
	}

	unauthorizedLoginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "eval@example.com",
		"password": "password123",
	}, "")
	if unauthorizedLoginRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected pending signup login to fail, got %d", unauthorizedLoginRes.Code)
	}

	verifyToken := latestAuthToken(t, env.tokenSink, "eval@example.com", "email_verification")
	verifyRes := performJSON(env.router, http.MethodPost, "/v1/auth/verify-email", map[string]any{
		"token": verifyToken,
	}, "")
	if verifyRes.Code != http.StatusOK {
		t.Fatalf("expected signup verification 200, got %d: %s", verifyRes.Code, verifyRes.Body.String())
	}
	verifiedBody := decodeBody[userBody](t, verifyRes)
	if !verifiedBody.User.EmailVerified || verifiedBody.User.SignupPendingVerification {
		t.Fatalf("expected verified signup user, got %+v", verifiedBody.User)
	}

	loginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "eval@example.com",
		"password": "password123",
	}, "")
	if loginRes.Code != http.StatusOK {
		t.Fatalf("expected verified signup login 200, got %d: %s", loginRes.Code, loginRes.Body.String())
	}
	loginBody := decodeBody[tokenBody](t, loginRes)

	for i := 0; i < 5; i++ {
		res := performJSON(env.router, http.MethodPost, "/v1/orgs/"+signupBody.Organization.ID+"/devices", devicePayload("eval-device-"+strconv.Itoa(i), "EVAL-"+strconv.Itoa(i)), loginBody.Tokens.AccessToken)
		if res.Code != http.StatusCreated {
			t.Fatalf("expected device %d create 201, got %d: %s", i, res.Code, res.Body.String())
		}
	}
	quotaExceededRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+signupBody.Organization.ID+"/devices", devicePayload("eval-device-5", "EVAL-5"), loginBody.Tokens.AccessToken)
	if quotaExceededRes.Code != http.StatusConflict {
		t.Fatalf("expected quota exceeded 409, got %d: %s", quotaExceededRes.Code, quotaExceededRes.Body.String())
	}
	var quotaExceeded struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(quotaExceededRes.Body.Bytes(), &quotaExceeded); err != nil {
		t.Fatal(err)
	}
	if quotaExceeded.Error.Code != "EVALUATION_QUOTA_EXCEEDED" {
		t.Fatalf("expected evaluation quota error code, got %+v", quotaExceeded)
	}

	raiseReqRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+signupBody.Organization.ID+"/quota-raise-requests", map[string]any{
		"requested_quota": 8,
		"use_case":        "pilot expansion",
		"contact_info": map[string]any{
			"email": "buyer@example.com",
		},
	}, loginBody.Tokens.AccessToken)
	if raiseReqRes.Code != http.StatusCreated {
		t.Fatalf("expected quota raise request 201, got %d: %s", raiseReqRes.Code, raiseReqRes.Body.String())
	}
	raiseReqBody := decodeBody[quotaRaiseRequestBody](t, raiseReqRes)
	if raiseReqBody.QuotaRaiseRequest.Status != string(model.QuotaRaiseRequestStatusPending) {
		t.Fatalf("expected pending quota raise request, got %+v", raiseReqBody.QuotaRaiseRequest)
	}

	nonAdminApproveRes := performJSON(env.router, http.MethodPost, "/v1/admin/quota-raise-requests/"+raiseReqBody.QuotaRaiseRequest.ID+"/approve", map[string]any{
		"approved_quota": 500,
	}, loginBody.Tokens.AccessToken)
	if nonAdminApproveRes.Code != http.StatusForbidden {
		t.Fatalf("expected non-admin approval attempt 403, got %d: %s", nonAdminApproveRes.Code, nonAdminApproveRes.Body.String())
	}

	admin := registerUser(t, env.router, "platform-admin@example.com", "Admin Org")
	if _, err := env.db.Exec(context.Background(), `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	approveRes := performJSON(env.router, http.MethodPost, "/v1/admin/quota-raise-requests/"+raiseReqBody.QuotaRaiseRequest.ID+"/approve", map[string]any{
		"approved_quota": 500,
	}, admin.Tokens.AccessToken)
	if approveRes.Code != http.StatusOK {
		t.Fatalf("expected quota approval 200, got %d: %s", approveRes.Code, approveRes.Body.String())
	}
	approvedBody := decodeBody[quotaRaiseDecisionBody](t, approveRes)
	if approvedBody.Organization.EvaluationDeviceQuota != 200 {
		t.Fatalf("expected approved quota to cap at 200, got %+v", approvedBody.Organization)
	}

	declineReqRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+signupBody.Organization.ID+"/quota-raise-requests", map[string]any{
		"requested_quota": 12,
		"use_case":        "contract exit",
		"contact_info": map[string]any{
			"email": "buyer@example.com",
		},
	}, loginBody.Tokens.AccessToken)
	if declineReqRes.Code != http.StatusCreated {
		t.Fatalf("expected second quota raise request 201, got %d: %s", declineReqRes.Code, declineReqRes.Body.String())
	}
	declineReqBody := decodeBody[quotaRaiseRequestBody](t, declineReqRes)
	declineRes := performJSON(env.router, http.MethodPost, "/v1/admin/quota-raise-requests/"+declineReqBody.QuotaRaiseRequest.ID+"/decline", map[string]any{
		"decision_reason": "not enough supporting detail",
	}, admin.Tokens.AccessToken)
	if declineRes.Code != http.StatusOK {
		t.Fatalf("expected quota decline 200, got %d: %s", declineRes.Code, declineRes.Body.String())
	}
	declinedBody := decodeBody[quotaRaiseDecisionBody](t, declineRes)
	if declinedBody.QuotaRaiseRequest.Status != string(model.QuotaRaiseRequestStatusDeclined) {
		t.Fatalf("expected declined quota raise request, got %+v", declinedBody.QuotaRaiseRequest)
	}
	if declinedBody.Organization.EvaluationDeviceQuota != 200 {
		t.Fatalf("expected decline to keep capped quota at 200, got %+v", declinedBody.Organization)
	}

	metricsRes := performJSON(env.router, http.MethodGet, "/v1/admin/metrics", nil, admin.Tokens.AccessToken)
	if metricsRes.Code != http.StatusOK {
		t.Fatalf("expected admin metrics 200, got %d: %s", metricsRes.Code, metricsRes.Body.String())
	}
	metricsBody := decodeBody[evalTierMetricsBody](t, metricsRes)
	if metricsBody.Signups.EvaluationCreated != 1 {
		t.Fatalf("expected one evaluation signup, got %+v", metricsBody.Signups)
	}
	if metricsBody.Signups.VerificationCompleted != 1 {
		t.Fatalf("expected one signup verification completion, got %+v", metricsBody.Signups)
	}
	if metricsBody.Signups.VerificationCompletionRate != 1 {
		t.Fatalf("expected 100%% verification completion, got %+v", metricsBody.Signups)
	}
	if metricsBody.QuotaRaiseRequests.Pending != 0 || metricsBody.QuotaRaiseRequests.Approved != 1 || metricsBody.QuotaRaiseRequests.Declined != 1 {
		t.Fatalf("expected quota raise status counts to reflect one approve and one decline, got %+v", metricsBody.QuotaRaiseRequests)
	}
	if len(metricsBody.EvaluationQuotaUsage) != 1 {
		t.Fatalf("expected one evaluation org usage entry, got %+v", metricsBody.EvaluationQuotaUsage)
	}
	if metricsBody.EvaluationQuotaUsage[0].ActiveDevices != 5 || metricsBody.EvaluationQuotaUsage[0].EvaluationDeviceQuota != 200 {
		t.Fatalf("expected live quota usage to reflect approved quota and existing devices, got %+v", metricsBody.EvaluationQuotaUsage[0])
	}

	postApprovalRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+signupBody.Organization.ID+"/devices", devicePayload("eval-device-5", "EVAL-5"), loginBody.Tokens.AccessToken)
	if postApprovalRes.Code != http.StatusCreated {
		t.Fatalf("expected device create after approval 201, got %d: %s", postApprovalRes.Code, postApprovalRes.Body.String())
	}
}

func TestIntegrationAdminMetricsReportsEmptySnapshot(t *testing.T) {
	env := newIntegrationEnv(t)

	admin := registerUser(t, env.router, "metrics-admin@example.com", "Metrics Admin Org")
	if _, err := env.db.Exec(context.Background(), `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}

	metricsRes := performJSON(env.router, http.MethodGet, "/v1/admin/metrics", nil, admin.Tokens.AccessToken)
	if metricsRes.Code != http.StatusOK {
		t.Fatalf("expected empty admin metrics 200, got %d: %s", metricsRes.Code, metricsRes.Body.String())
	}
	metricsBody := decodeBody[evalTierMetricsBody](t, metricsRes)
	if metricsBody.Signups.EvaluationCreated != 0 || metricsBody.Signups.VerificationCompleted != 0 || metricsBody.Signups.VerificationCompletionRate != 0 {
		t.Fatalf("expected empty signup metrics, got %+v", metricsBody.Signups)
	}
	if metricsBody.QuotaRaiseRequests.Pending != 0 || metricsBody.QuotaRaiseRequests.Approved != 0 || metricsBody.QuotaRaiseRequests.Declined != 0 {
		t.Fatalf("expected empty quota-raise metrics, got %+v", metricsBody.QuotaRaiseRequests)
	}
	if len(metricsBody.EvaluationQuotaUsage) != 0 {
		t.Fatalf("expected no evaluation quota usage rows, got %+v", metricsBody.EvaluationQuotaUsage)
	}
}

func TestIntegrationQuotaRaiseValidationAndDefaultApproval(t *testing.T) {
	env := newIntegrationEnv(t)

	signupRes := performJSON(env.router, http.MethodPost, "/v1/auth/signup", map[string]any{
		"email":             "validate-quota@example.com",
		"password":          "password123",
		"display_name":      "Validate Quota",
		"organization_name": "Validate Quota Org",
	}, "")
	if signupRes.Code != http.StatusAccepted {
		t.Fatalf("expected signup 202, got %d: %s", signupRes.Code, signupRes.Body.String())
	}
	signupBody := decodeBody[signupBody](t, signupRes)
	verifyToken := latestAuthToken(t, env.tokenSink, "validate-quota@example.com", "email_verification")
	verifyRes := performJSON(env.router, http.MethodPost, "/v1/auth/verify-email", map[string]any{
		"token": verifyToken,
	}, "")
	if verifyRes.Code != http.StatusOK {
		t.Fatalf("expected verify 200, got %d: %s", verifyRes.Code, verifyRes.Body.String())
	}
	loginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "validate-quota@example.com",
		"password": "password123",
	}, "")
	if loginRes.Code != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", loginRes.Code, loginRes.Body.String())
	}
	loginBody := decodeBody[tokenBody](t, loginRes)

	invalidRequests := []map[string]any{
		{"requested_quota": 0, "use_case": "pilot", "contact_info": map[string]any{"email": "buyer@example.com"}},
		{"requested_quota": 201, "use_case": "pilot", "contact_info": map[string]any{"email": "buyer@example.com"}},
		{"requested_quota": 8, "use_case": "   ", "contact_info": map[string]any{"email": "buyer@example.com"}},
		{"requested_quota": 8, "use_case": "pilot", "contact_info": map[string]any{}},
	}
	for i, body := range invalidRequests {
		res := performJSON(env.router, http.MethodPost, "/v1/orgs/"+signupBody.Organization.ID+"/quota-raise-requests", body, loginBody.Tokens.AccessToken)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("expected invalid quota raise request %d to fail 400, got %d: %s", i, res.Code, res.Body.String())
		}
	}

	raiseReqRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+signupBody.Organization.ID+"/quota-raise-requests", map[string]any{
		"requested_quota": 8,
		"use_case":        "pilot expansion",
		"contact_info": map[string]any{
			"email": "buyer@example.com",
		},
	}, loginBody.Tokens.AccessToken)
	if raiseReqRes.Code != http.StatusCreated {
		t.Fatalf("expected quota raise request 201, got %d: %s", raiseReqRes.Code, raiseReqRes.Body.String())
	}
	raiseReqBody := decodeBody[quotaRaiseRequestBody](t, raiseReqRes)

	admin := registerUser(t, env.router, "validate-quota-admin@example.com", "Validate Quota Admin Org")
	if _, err := env.db.Exec(context.Background(), `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	approveRes := performJSON(env.router, http.MethodPost, "/v1/admin/quota-raise-requests/"+raiseReqBody.QuotaRaiseRequest.ID+"/approve", nil, admin.Tokens.AccessToken)
	if approveRes.Code != http.StatusOK {
		t.Fatalf("expected quota approval 200, got %d: %s", approveRes.Code, approveRes.Body.String())
	}
	approvedBody := decodeBody[quotaRaiseDecisionBody](t, approveRes)
	if approvedBody.Organization.EvaluationDeviceQuota != 8 {
		t.Fatalf("expected default approval quota to use requested amount, got %+v", approvedBody.Organization)
	}
	declineReqRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+signupBody.Organization.ID+"/quota-raise-requests", map[string]any{
		"requested_quota": 12,
		"use_case":        "contract exit",
		"contact_info": map[string]any{
			"email": "buyer@example.com",
		},
	}, loginBody.Tokens.AccessToken)
	if declineReqRes.Code != http.StatusCreated {
		t.Fatalf("expected decline quota raise request 201, got %d: %s", declineReqRes.Code, declineReqRes.Body.String())
	}
	declineReqBody := decodeBody[quotaRaiseRequestBody](t, declineReqRes)
	declineRes := performJSON(env.router, http.MethodPost, "/v1/admin/quota-raise-requests/"+declineReqBody.QuotaRaiseRequest.ID+"/decline", nil, admin.Tokens.AccessToken)
	if declineRes.Code != http.StatusOK {
		t.Fatalf("expected quota decline 200, got %d: %s", declineRes.Code, declineRes.Body.String())
	}
	declinedBody := decodeBody[quotaRaiseDecisionBody](t, declineRes)
	if declinedBody.QuotaRaiseRequest.Status != string(model.QuotaRaiseRequestStatusDeclined) {
		t.Fatalf("expected declined quota raise request, got %+v", declinedBody.QuotaRaiseRequest)
	}
	if len(env.notificationSink.deliveries) != 2 {
		t.Fatalf("expected approval and decline notifications, got %+v", env.notificationSink.deliveries)
	}
	if env.notificationSink.deliveries[0].Decision != string(model.QuotaRaiseRequestStatusApproved) || env.notificationSink.deliveries[0].RecipientEmail != "validate-quota@example.com" {
		t.Fatalf("expected approval notification for requester, got %+v", env.notificationSink.deliveries[0])
	}
	if env.notificationSink.deliveries[1].Decision != string(model.QuotaRaiseRequestStatusDeclined) || env.notificationSink.deliveries[1].RecipientEmail != "validate-quota@example.com" {
		t.Fatalf("expected decline notification for requester, got %+v", env.notificationSink.deliveries[1])
	}
}

func TestIntegrationCurrentUserCanChangePassword(t *testing.T) {
	env := newIntegrationEnv(t)

	registered := registerUser(t, env.router, "password-change@example.com", "Password Org")

	wrongCurrentRes := performJSON(env.router, http.MethodPatch, "/v1/me/password", map[string]any{
		"current_password": "wrong-password",
		"new_password":     "new-password123",
	}, registered.Tokens.AccessToken)
	if wrongCurrentRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected wrong current password 401, got %d", wrongCurrentRes.Code)
	}

	shortPasswordRes := performJSON(env.router, http.MethodPatch, "/v1/me/password", map[string]any{
		"current_password": "password123",
		"new_password":     "short",
	}, registered.Tokens.AccessToken)
	if shortPasswordRes.Code != http.StatusBadRequest {
		t.Fatalf("expected short new password 400, got %d", shortPasswordRes.Code)
	}

	changeRes := performJSON(env.router, http.MethodPatch, "/v1/me/password", map[string]any{
		"current_password": "password123",
		"new_password":     "new-password123",
	}, registered.Tokens.AccessToken)
	if changeRes.Code != http.StatusNoContent {
		t.Fatalf("expected password change 204, got %d: %s", changeRes.Code, changeRes.Body.String())
	}

	oldPasswordLoginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "password-change@example.com",
		"password": "password123",
	}, "")
	if oldPasswordLoginRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected old password login 401, got %d", oldPasswordLoginRes.Code)
	}

	revokedRefreshRes := performJSON(env.router, http.MethodPost, "/v1/auth/refresh", map[string]any{
		"refresh_token": registered.Tokens.RefreshToken,
	}, "")
	if revokedRefreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected password change to revoke refresh token, got %d", revokedRefreshRes.Code)
	}

	meRes := performJSON(env.router, http.MethodGet, "/v1/me", nil, registered.Tokens.AccessToken)
	if meRes.Code != http.StatusOK {
		t.Fatalf("expected existing access token to remain valid until expiry, got %d", meRes.Code)
	}

	newPasswordLoginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "password-change@example.com",
		"password": "new-password123",
	}, "")
	if newPasswordLoginRes.Code != http.StatusOK {
		t.Fatalf("expected new password login 200, got %d: %s", newPasswordLoginRes.Code, newPasswordLoginRes.Body.String())
	}
}

func TestIntegrationCurrentUserCanDisableSelfWithOwnerSafety(t *testing.T) {
	env := newIntegrationEnv(t)

	lastOwner := registerUser(t, env.router, "last-owner@example.com", "Last Owner Org")
	lastOwnerDeleteRes := performJSON(env.router, http.MethodDelete, "/v1/me", nil, lastOwner.Tokens.AccessToken)
	if lastOwnerDeleteRes.Code != http.StatusConflict {
		t.Fatalf("expected last owner self-delete 409, got %d: %s", lastOwnerDeleteRes.Code, lastOwnerDeleteRes.Body.String())
	}

	owner := registerUser(t, env.router, "self-delete@example.com", "Self Delete Org")
	backupOwner := registerUser(t, env.router, "backup-owner@example.com", "Backup Owner Org")

	addBackupOwnerRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "backup-owner@example.com",
		"role":  "owner",
	}, owner.Tokens.AccessToken)
	if addBackupOwnerRes.Code != http.StatusCreated {
		t.Fatalf("expected add backup owner 201, got %d: %s", addBackupOwnerRes.Code, addBackupOwnerRes.Body.String())
	}

	deleteRes := performJSON(env.router, http.MethodDelete, "/v1/me", nil, owner.Tokens.AccessToken)
	if deleteRes.Code != http.StatusNoContent {
		t.Fatalf("expected self-delete 204, got %d: %s", deleteRes.Code, deleteRes.Body.String())
	}

	meRes := performJSON(env.router, http.MethodGet, "/v1/me", nil, owner.Tokens.AccessToken)
	if meRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled self access token 401, got %d", meRes.Code)
	}
	refreshRes := performJSON(env.router, http.MethodPost, "/v1/auth/refresh", map[string]any{
		"refresh_token": owner.Tokens.RefreshToken,
	}, "")
	if refreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled self refresh token 401, got %d", refreshRes.Code)
	}
	loginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "self-delete@example.com",
		"password": "password123",
	}, "")
	if loginRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled self login 401, got %d", loginRes.Code)
	}

	membersRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/members", nil, backupOwner.Tokens.AccessToken)
	if membersRes.Code != http.StatusOK {
		t.Fatalf("expected backup owner member list 200, got %d: %s", membersRes.Code, membersRes.Body.String())
	}
	members := decodeBody[membersBody](t, membersRes)
	foundDisabledOwner := false
	for _, member := range members.Members {
		if member.UserID == owner.User.ID {
			foundDisabledOwner = member.DisabledAt != nil
			break
		}
	}
	if !foundDisabledOwner {
		t.Fatal("expected disabled current user to remain listed as a disabled member")
	}

	backupDowngradeRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+backupOwner.User.ID, map[string]any{
		"role": "admin",
	}, backupOwner.Tokens.AccessToken)
	if backupDowngradeRes.Code != http.StatusConflict {
		t.Fatalf("expected only active owner downgrade 409, got %d", backupDowngradeRes.Code)
	}

	backupRemoveRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/members/"+backupOwner.User.ID, nil, backupOwner.Tokens.AccessToken)
	if backupRemoveRes.Code != http.StatusConflict {
		t.Fatalf("expected only active owner remove 409, got %d", backupRemoveRes.Code)
	}
}

func TestIntegrationDisabledUserCannotUseExistingTokens(t *testing.T) {
	env := newIntegrationEnv(t)

	registered := registerUser(t, env.router, "disabled@example.com", "Disabled Org")
	if _, err := env.db.Exec(context.Background(), `
		UPDATE users SET disabled_at = now() WHERE id = $1
	`, registered.User.ID); err != nil {
		t.Fatal(err)
	}

	meRes := performJSON(env.router, http.MethodGet, "/v1/me", nil, registered.Tokens.AccessToken)
	if meRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled user access token 401, got %d", meRes.Code)
	}

	createOrgRes := performJSON(env.router, http.MethodPost, "/v1/orgs", map[string]any{
		"name": "Should Fail",
	}, registered.Tokens.AccessToken)
	if createOrgRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled user org create 401, got %d", createOrgRes.Code)
	}

	refreshRes := performJSON(env.router, http.MethodPost, "/v1/auth/refresh", map[string]any{
		"refresh_token": registered.Tokens.RefreshToken,
	}, "")
	if refreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled user refresh 401, got %d", refreshRes.Code)
	}
}

func TestIntegrationOwnerCanDisableAndEnableMemberUser(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	member := registerUser(t, env.router, "member@example.com", "Member Org")
	admin := registerUser(t, env.router, "admin@example.com", "Admin Org")

	addMemberRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "member@example.com",
		"role":  "member",
	}, owner.Tokens.AccessToken)
	if addMemberRes.Code != http.StatusCreated {
		t.Fatalf("expected add member 201, got %d: %s", addMemberRes.Code, addMemberRes.Body.String())
	}
	addAdminRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "admin@example.com",
		"role":  "admin",
	}, owner.Tokens.AccessToken)
	if addAdminRes.Code != http.StatusCreated {
		t.Fatalf("expected add admin 201, got %d: %s", addAdminRes.Code, addAdminRes.Body.String())
	}

	adminDisableRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+member.User.ID+"/disable", nil, admin.Tokens.AccessToken)
	if adminDisableRes.Code != http.StatusForbidden {
		t.Fatalf("expected admin disable member 403, got %d", adminDisableRes.Code)
	}
	memberDisableRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+member.User.ID+"/disable", nil, member.Tokens.AccessToken)
	if memberDisableRes.Code != http.StatusForbidden {
		t.Fatalf("expected member disable member 403, got %d", memberDisableRes.Code)
	}

	disableRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+member.User.ID+"/disable", nil, owner.Tokens.AccessToken)
	if disableRes.Code != http.StatusOK {
		t.Fatalf("expected owner disable member 200, got %d: %s", disableRes.Code, disableRes.Body.String())
	}
	disabledMember := decodeBody[memberBody](t, disableRes)
	if disabledMember.Member.DisabledAt == nil {
		t.Fatal("expected disabled member response to include disabled_at")
	}

	memberMeRes := performJSON(env.router, http.MethodGet, "/v1/me", nil, member.Tokens.AccessToken)
	if memberMeRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled member access token 401, got %d", memberMeRes.Code)
	}
	memberRefreshRes := performJSON(env.router, http.MethodPost, "/v1/auth/refresh", map[string]any{
		"refresh_token": member.Tokens.RefreshToken,
	}, "")
	if memberRefreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled member refresh 401, got %d", memberRefreshRes.Code)
	}
	memberLoginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "member@example.com",
		"password": "password123",
	}, "")
	if memberLoginRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled member login 401, got %d", memberLoginRes.Code)
	}

	membersRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/members", nil, owner.Tokens.AccessToken)
	if membersRes.Code != http.StatusOK {
		t.Fatalf("expected member list 200, got %d", membersRes.Code)
	}
	members := decodeBody[membersBody](t, membersRes)
	if members.Pagination.Total != 3 {
		t.Fatalf("expected disabled member to remain listed, got pagination %+v", members.Pagination)
	}

	enableRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+member.User.ID+"/enable", nil, owner.Tokens.AccessToken)
	if enableRes.Code != http.StatusOK {
		t.Fatalf("expected owner enable member 200, got %d: %s", enableRes.Code, enableRes.Body.String())
	}
	enabledMember := decodeBody[memberBody](t, enableRes)
	if enabledMember.Member.DisabledAt != nil {
		t.Fatal("expected enabled member response to clear disabled_at")
	}

	memberLoginAfterEnableRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "member@example.com",
		"password": "password123",
	}, "")
	if memberLoginAfterEnableRes.Code != http.StatusOK {
		t.Fatalf("expected enabled member login 200, got %d", memberLoginAfterEnableRes.Code)
	}
}

func TestIntegrationOwnerCanUpdateAndRemoveMember(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	member := registerUser(t, env.router, "member@example.com", "Member Org")

	addMemberRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "member@example.com",
		"role":  "member",
	}, owner.Tokens.AccessToken)
	if addMemberRes.Code != http.StatusCreated {
		t.Fatalf("expected add member 201, got %d: %s", addMemberRes.Code, addMemberRes.Body.String())
	}

	updateRoleRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+member.User.ID, map[string]any{
		"role": "admin",
	}, owner.Tokens.AccessToken)
	if updateRoleRes.Code != http.StatusOK {
		t.Fatalf("expected member role update 200, got %d: %s", updateRoleRes.Code, updateRoleRes.Body.String())
	}
	updated := decodeBody[memberBody](t, updateRoleRes)
	if updated.Member.Role != "admin" {
		t.Fatalf("expected updated role admin, got %+v", updated.Member)
	}

	removeMemberRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/members/"+member.User.ID, nil, owner.Tokens.AccessToken)
	if removeMemberRes.Code != http.StatusNoContent {
		t.Fatalf("expected member remove 204, got %d", removeMemberRes.Code)
	}

	memberListRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/members", nil, owner.Tokens.AccessToken)
	if memberListRes.Code != http.StatusOK {
		t.Fatalf("expected member list 200, got %d", memberListRes.Code)
	}
	members := decodeBody[membersBody](t, memberListRes)
	if members.Pagination.Total != 1 {
		t.Fatalf("expected only owner after member removal, got pagination %+v", members.Pagination)
	}
}

func TestIntegrationValidationAndNotFoundErrors(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")

	malformedCreateOrgRes := performRaw(env.router, http.MethodPost, "/v1/orgs", []byte(`{"name":`), owner.Tokens.AccessToken)
	if malformedCreateOrgRes.Code != http.StatusBadRequest {
		t.Fatalf("expected malformed org create 400, got %d", malformedCreateOrgRes.Code)
	}

	invalidAddRoleRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "missing@example.com",
		"role":  "invalid",
	}, owner.Tokens.AccessToken)
	if invalidAddRoleRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid add member role 400, got %d", invalidAddRoleRes.Code)
	}

	invalidUpdateRoleRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+owner.User.ID, map[string]any{
		"role": "invalid",
	}, owner.Tokens.AccessToken)
	if invalidUpdateRoleRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid update member role 400, got %d", invalidUpdateRoleRes.Code)
	}

	missingUserRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "missing@example.com",
		"role":  "member",
	}, owner.Tokens.AccessToken)
	if missingUserRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing member user 404, got %d", missingUserRes.Code)
	}

	removeMissingRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/members/00000000-0000-0000-0000-000000000000", nil, owner.Tokens.AccessToken)
	if removeMissingRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing member remove 404, got %d", removeMissingRes.Code)
	}

	deleteMissingDeviceRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/00000000-0000-0000-0000-000000000000", nil, owner.Tokens.AccessToken)
	if deleteMissingDeviceRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing device delete 404, got %d", deleteMissingDeviceRes.Code)
	}
}

func TestIntegrationRoleAuthorizationDeviceScopeAndSerialUniqueness(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	member := registerUser(t, env.router, "member@example.com", "Member Org")
	admin := registerUser(t, env.router, "admin@example.com", "Admin Org")

	addMemberRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "member@example.com",
		"role":  "member",
	}, owner.Tokens.AccessToken)
	if addMemberRes.Code != http.StatusCreated {
		t.Fatalf("expected add member 201, got %d: %s", addMemberRes.Code, addMemberRes.Body.String())
	}

	addAdminRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "admin@example.com",
		"role":  "admin",
	}, owner.Tokens.AccessToken)
	if addAdminRes.Code != http.StatusCreated {
		t.Fatalf("expected add admin 201, got %d: %s", addAdminRes.Code, addAdminRes.Body.String())
	}

	adminAddMemberRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "admin@example.com",
		"role":  "member",
	}, admin.Tokens.AccessToken)
	if adminAddMemberRes.Code != http.StatusForbidden {
		t.Fatalf("expected admin add member 403, got %d", adminAddMemberRes.Code)
	}

	adminUpdateMemberRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+member.User.ID, map[string]any{
		"role": "admin",
	}, admin.Tokens.AccessToken)
	if adminUpdateMemberRes.Code != http.StatusForbidden {
		t.Fatalf("expected admin update member 403, got %d", adminUpdateMemberRes.Code)
	}

	adminRemoveMemberRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/members/"+member.User.ID, nil, admin.Tokens.AccessToken)
	if adminRemoveMemberRes.Code != http.StatusForbidden {
		t.Fatalf("expected admin remove member 403, got %d", adminRemoveMemberRes.Code)
	}

	memberCreateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("cam-1", "SERIAL-1"), member.Tokens.AccessToken)
	if memberCreateRes.Code != http.StatusForbidden {
		t.Fatalf("expected member device create 403, got %d", memberCreateRes.Code)
	}

	deviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("cam-1", "SERIAL-1"), owner.Tokens.AccessToken)
	if deviceRes.Code != http.StatusCreated {
		t.Fatalf("expected device create 201, got %d: %s", deviceRes.Code, deviceRes.Body.String())
	}
	createdDeviceBody := decodeBody[deviceBody](t, deviceRes)

	memberListRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices", nil, member.Tokens.AccessToken)
	if memberListRes.Code != http.StatusOK {
		t.Fatalf("expected member list devices 200, got %d", memberListRes.Code)
	}

	memberGetRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, nil, member.Tokens.AccessToken)
	if memberGetRes.Code != http.StatusOK {
		t.Fatalf("expected member get device 200, got %d", memberGetRes.Code)
	}

	memberUpdateRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, devicePayload("member-update", "SERIAL-2"), member.Tokens.AccessToken)
	if memberUpdateRes.Code != http.StatusForbidden {
		t.Fatalf("expected member update device 403, got %d", memberUpdateRes.Code)
	}

	memberStatusRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID+"/status", map[string]any{
		"status": "offline",
	}, member.Tokens.AccessToken)
	if memberStatusRes.Code != http.StatusForbidden {
		t.Fatalf("expected member status update 403, got %d", memberStatusRes.Code)
	}

	memberDeleteRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, nil, member.Tokens.AccessToken)
	if memberDeleteRes.Code != http.StatusForbidden {
		t.Fatalf("expected member delete device 403, got %d", memberDeleteRes.Code)
	}

	adminDeviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("admin-cam", "ADMIN-SERIAL-1"), admin.Tokens.AccessToken)
	if adminDeviceRes.Code != http.StatusCreated {
		t.Fatalf("expected admin create device 201, got %d: %s", adminDeviceRes.Code, adminDeviceRes.Body.String())
	}
	adminDeviceBody := decodeBody[deviceBody](t, adminDeviceRes)

	adminUpdateDeviceRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+adminDeviceBody.Device.ID, devicePayload("admin-cam-updated", "ADMIN-SERIAL-2"), admin.Tokens.AccessToken)
	if adminUpdateDeviceRes.Code != http.StatusOK {
		t.Fatalf("expected admin update device 200, got %d", adminUpdateDeviceRes.Code)
	}

	adminStatusRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+adminDeviceBody.Device.ID+"/status", map[string]any{
		"status": "online",
	}, admin.Tokens.AccessToken)
	if adminStatusRes.Code != http.StatusOK {
		t.Fatalf("expected admin status update 200, got %d", adminStatusRes.Code)
	}

	adminDeleteRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+adminDeviceBody.Device.ID, nil, admin.Tokens.AccessToken)
	if adminDeleteRes.Code != http.StatusNoContent {
		t.Fatalf("expected admin delete device 204, got %d", adminDeleteRes.Code)
	}

	duplicateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("cam-dup", "SERIAL-1"), owner.Tokens.AccessToken)
	if duplicateRes.Code != http.StatusConflict {
		t.Fatalf("expected duplicate serial 409, got %d", duplicateRes.Code)
	}

	otherOrgRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+member.Organization.ID+"/devices", devicePayload("cam-other", "SERIAL-1"), member.Tokens.AccessToken)
	if otherOrgRes.Code != http.StatusCreated {
		t.Fatalf("expected same serial in different org 201, got %d: %s", otherOrgRes.Code, otherOrgRes.Body.String())
	}

	crossOrgRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+member.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, nil, member.Tokens.AccessToken)
	if crossOrgRes.Code != http.StatusNotFound {
		t.Fatalf("expected cross-org device lookup 404, got %d", crossOrgRes.Code)
	}

	otherOrgResByOwner := performJSON(env.router, http.MethodGet, "/v1/orgs/"+member.Organization.ID, nil, owner.Tokens.AccessToken)
	if otherOrgResByOwner.Code != http.StatusNotFound {
		t.Fatalf("expected cross-org organization lookup 404, got %d", otherOrgResByOwner.Code)
	}

	otherOrgMembersRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+member.Organization.ID+"/members", nil, owner.Tokens.AccessToken)
	if otherOrgMembersRes.Code != http.StatusNotFound {
		t.Fatalf("expected cross-org member list 404, got %d", otherOrgMembersRes.Code)
	}

	listBody := decodeBody[devicesBody](t, memberListRes)
	if len(listBody.Devices) != 1 || listBody.Devices[0].ID != createdDeviceBody.Device.ID {
		t.Fatalf("expected member list to include created device, got %+v", listBody.Devices)
	}
	if listBody.Pagination.Limit != 50 || listBody.Pagination.Offset != 0 || listBody.Pagination.Total != 1 {
		t.Fatalf("expected default device pagination, got %+v", listBody.Pagination)
	}

	updatedDeviceRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, devicePayload("cam-updated", "SERIAL-UPDATED"), owner.Tokens.AccessToken)
	if updatedDeviceRes.Code != http.StatusOK {
		t.Fatalf("expected owner update device 200, got %d", updatedDeviceRes.Code)
	}
	updatedDeviceBody := decodeBody[deviceBody](t, updatedDeviceRes)
	if updatedDeviceBody.Device.Name != "cam-updated" || updatedDeviceBody.Device.SerialNumber == nil || *updatedDeviceBody.Device.SerialNumber != "SERIAL-UPDATED" {
		t.Fatalf("expected updated device fields, got %+v", updatedDeviceBody.Device)
	}

	invalidCategoryRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", map[string]any{
		"name":     "bad-category",
		"category": "bad",
	}, owner.Tokens.AccessToken)
	if invalidCategoryRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid category 400, got %d", invalidCategoryRes.Code)
	}

	statusRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID+"/status", map[string]any{
		"status": "online",
	}, owner.Tokens.AccessToken)
	if statusRes.Code != http.StatusOK {
		t.Fatalf("expected status update 200, got %d", statusRes.Code)
	}

	invalidStatusRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID+"/status", map[string]any{
		"status": "bad",
	}, owner.Tokens.AccessToken)
	if invalidStatusRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid status 400, got %d", invalidStatusRes.Code)
	}

	deleteRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, nil, owner.Tokens.AccessToken)
	if deleteRes.Code != http.StatusNoContent {
		t.Fatalf("expected delete 204, got %d", deleteRes.Code)
	}

	var status string
	var disabledAt *time.Time
	err := env.db.QueryRow(context.Background(), `SELECT status, disabled_at FROM devices WHERE id = $1`, createdDeviceBody.Device.ID).Scan(&status, &disabledAt)
	if err != nil {
		t.Fatal(err)
	}
	if status != "disabled" || disabledAt == nil {
		t.Fatalf("expected soft-disabled device, got status=%s disabled_at=%v", status, disabledAt)
	}

	getDisabledRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, nil, owner.Tokens.AccessToken)
	if getDisabledRes.Code != http.StatusOK {
		t.Fatalf("expected disabled device to remain readable, got %d", getDisabledRes.Code)
	}

	updateDisabledRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, devicePayload("disabled-update", "DISABLED-UPDATED"), owner.Tokens.AccessToken)
	if updateDisabledRes.Code != http.StatusConflict {
		t.Fatalf("expected disabled device update 409, got %d", updateDisabledRes.Code)
	}

	statusDisabledRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID+"/status", map[string]any{
		"status": "online",
	}, owner.Tokens.AccessToken)
	if statusDisabledRes.Code != http.StatusConflict {
		t.Fatalf("expected disabled device status update 409, got %d", statusDisabledRes.Code)
	}

	deleteDisabledRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+createdDeviceBody.Device.ID, nil, owner.Tokens.AccessToken)
	if deleteDisabledRes.Code != http.StatusNoContent {
		t.Fatalf("expected repeated disabled device delete to remain 204, got %d", deleteDisabledRes.Code)
	}
}

func TestIntegrationFleetGroupsAndTags(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	admin := registerUser(t, env.router, "admin@example.com", "Admin Org")
	member := registerUser(t, env.router, "member@example.com", "Member Org")
	outsider := registerUser(t, env.router, "outsider@example.com", "Outsider Org")

	for _, membership := range []struct {
		email string
		role  string
	}{
		{email: "admin@example.com", role: "admin"},
		{email: "member@example.com", role: "member"},
	} {
		res := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
			"email": membership.email,
			"role":  membership.role,
		}, owner.Tokens.AccessToken)
		if res.Code != http.StatusCreated {
			t.Fatalf("expected add member %s 201, got %d: %s", membership.email, res.Code, res.Body.String())
		}
	}

	deviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("fleet-camera", "FLEET-001"), owner.Tokens.AccessToken)
	if deviceRes.Code != http.StatusCreated {
		t.Fatalf("expected device create 201, got %d: %s", deviceRes.Code, deviceRes.Body.String())
	}
	device := decodeBody[deviceBody](t, deviceRes)

	disabledDeviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("disabled-camera", "FLEET-002"), owner.Tokens.AccessToken)
	if disabledDeviceRes.Code != http.StatusCreated {
		t.Fatalf("expected disabled candidate create 201, got %d: %s", disabledDeviceRes.Code, disabledDeviceRes.Body.String())
	}
	disabledDevice := decodeBody[deviceBody](t, disabledDeviceRes)
	deleteDisabledRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+disabledDevice.Device.ID, nil, owner.Tokens.AccessToken)
	if deleteDisabledRes.Code != http.StatusNoContent {
		t.Fatalf("expected disable device 204, got %d", deleteDisabledRes.Code)
	}

	memberCreateGroupRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/device-groups", map[string]any{
		"name": "Lobby",
	}, member.Tokens.AccessToken)
	if memberCreateGroupRes.Code != http.StatusForbidden {
		t.Fatalf("expected member group create 403, got %d", memberCreateGroupRes.Code)
	}

	groupRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/device-groups", map[string]any{
		"name":        "Lobby",
		"description": "Front-of-house cameras",
	}, owner.Tokens.AccessToken)
	if groupRes.Code != http.StatusCreated {
		t.Fatalf("expected group create 201, got %d: %s", groupRes.Code, groupRes.Body.String())
	}
	group := decodeBody[deviceGroupBody](t, groupRes)

	duplicateGroupRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/device-groups", map[string]any{
		"name": "Lobby",
	}, admin.Tokens.AccessToken)
	if duplicateGroupRes.Code != http.StatusConflict {
		t.Fatalf("expected duplicate group 409, got %d", duplicateGroupRes.Code)
	}

	blankGroupRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/device-groups", map[string]any{
		"name": "   ",
	}, owner.Tokens.AccessToken)
	if blankGroupRes.Code != http.StatusBadRequest {
		t.Fatalf("expected blank group 400, got %d", blankGroupRes.Code)
	}

	updateGroupRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/device-groups/"+group.Group.ID, map[string]any{
		"name":        "Lobby Cameras",
		"description": "Updated group",
	}, admin.Tokens.AccessToken)
	if updateGroupRes.Code != http.StatusOK {
		t.Fatalf("expected admin group update 200, got %d: %s", updateGroupRes.Code, updateGroupRes.Body.String())
	}
	updatedGroup := decodeBody[deviceGroupBody](t, updateGroupRes)
	if updatedGroup.Group.Name != "Lobby Cameras" {
		t.Fatalf("expected updated group name, got %+v", updatedGroup.Group)
	}

	getGroupRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/device-groups/"+group.Group.ID, nil, member.Tokens.AccessToken)
	if getGroupRes.Code != http.StatusOK {
		t.Fatalf("expected member group get 200, got %d", getGroupRes.Code)
	}
	listGroupsRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/device-groups", nil, member.Tokens.AccessToken)
	if listGroupsRes.Code != http.StatusOK {
		t.Fatalf("expected member group list 200, got %d", listGroupsRes.Code)
	}
	groups := decodeBody[deviceGroupsBody](t, listGroupsRes)
	if len(groups.Groups) != 1 || groups.Groups[0].ID != group.Group.ID || groups.Pagination.Total != 1 {
		t.Fatalf("unexpected groups response: %+v", groups)
	}

	tempGroupRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/device-groups", map[string]any{
		"name": "Temporary",
	}, owner.Tokens.AccessToken)
	if tempGroupRes.Code != http.StatusCreated {
		t.Fatalf("expected temp group create 201, got %d", tempGroupRes.Code)
	}
	tempGroup := decodeBody[deviceGroupBody](t, tempGroupRes)
	deleteGroupRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/device-groups/"+tempGroup.Group.ID, nil, owner.Tokens.AccessToken)
	if deleteGroupRes.Code != http.StatusNoContent {
		t.Fatalf("expected group delete 204, got %d", deleteGroupRes.Code)
	}
	deleteMissingGroupRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/device-groups/"+tempGroup.Group.ID, nil, owner.Tokens.AccessToken)
	if deleteMissingGroupRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing group delete 404, got %d", deleteMissingGroupRes.Code)
	}
	updateMissingGroupRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/device-groups/"+tempGroup.Group.ID, map[string]any{
		"name": "Missing",
	}, owner.Tokens.AccessToken)
	if updateMissingGroupRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing group update 404, got %d", updateMissingGroupRes.Code)
	}

	adminAddDeviceRes := performJSON(env.router, http.MethodPut, "/v1/orgs/"+owner.Organization.ID+"/device-groups/"+group.Group.ID+"/devices/"+device.Device.ID, nil, admin.Tokens.AccessToken)
	if adminAddDeviceRes.Code != http.StatusNoContent {
		t.Fatalf("expected admin add device to group 204, got %d: %s", adminAddDeviceRes.Code, adminAddDeviceRes.Body.String())
	}
	missingGroupAddRes := performJSON(env.router, http.MethodPut, "/v1/orgs/"+owner.Organization.ID+"/device-groups/"+tempGroup.Group.ID+"/devices/"+device.Device.ID, nil, owner.Tokens.AccessToken)
	if missingGroupAddRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing group assignment 404, got %d", missingGroupAddRes.Code)
	}
	duplicateAddRes := performJSON(env.router, http.MethodPut, "/v1/orgs/"+owner.Organization.ID+"/device-groups/"+group.Group.ID+"/devices/"+device.Device.ID, nil, owner.Tokens.AccessToken)
	if duplicateAddRes.Code != http.StatusNoContent {
		t.Fatalf("expected duplicate group assignment to be idempotent 204, got %d", duplicateAddRes.Code)
	}
	addDisabledRes := performJSON(env.router, http.MethodPut, "/v1/orgs/"+owner.Organization.ID+"/device-groups/"+group.Group.ID+"/devices/"+disabledDevice.Device.ID, nil, owner.Tokens.AccessToken)
	if addDisabledRes.Code != http.StatusNoContent {
		t.Fatalf("expected disabled registry device to remain selectable 204, got %d: %s", addDisabledRes.Code, addDisabledRes.Body.String())
	}

	memberListDevicesRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/device-groups/"+group.Group.ID+"/devices", nil, member.Tokens.AccessToken)
	if memberListDevicesRes.Code != http.StatusOK {
		t.Fatalf("expected member group device list 200, got %d: %s", memberListDevicesRes.Code, memberListDevicesRes.Body.String())
	}
	groupDevices := decodeBody[devicesBody](t, memberListDevicesRes)
	if groupDevices.Pagination.Total != 2 {
		t.Fatalf("expected group selection to include enabled and disabled devices, got %+v", groupDevices.Pagination)
	}

	removeDeviceRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/device-groups/"+group.Group.ID+"/devices/"+disabledDevice.Device.ID, nil, admin.Tokens.AccessToken)
	if removeDeviceRes.Code != http.StatusNoContent {
		t.Fatalf("expected admin remove device from group 204, got %d", removeDeviceRes.Code)
	}
	repeatedRemoveDeviceRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/device-groups/"+group.Group.ID+"/devices/"+disabledDevice.Device.ID, nil, owner.Tokens.AccessToken)
	if repeatedRemoveDeviceRes.Code != http.StatusNoContent {
		t.Fatalf("expected repeated group removal to be idempotent 204, got %d", repeatedRemoveDeviceRes.Code)
	}

	memberAddDeviceRes := performJSON(env.router, http.MethodPut, "/v1/orgs/"+owner.Organization.ID+"/device-groups/"+group.Group.ID+"/devices/"+device.Device.ID, nil, member.Tokens.AccessToken)
	if memberAddDeviceRes.Code != http.StatusForbidden {
		t.Fatalf("expected member group assignment 403, got %d", memberAddDeviceRes.Code)
	}

	outsiderGroupRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/device-groups/"+group.Group.ID, nil, outsider.Tokens.AccessToken)
	if outsiderGroupRes.Code != http.StatusNotFound {
		t.Fatalf("expected outsider group lookup 404, got %d", outsiderGroupRes.Code)
	}
	crossOrgGroupRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+member.Organization.ID+"/device-groups/"+group.Group.ID, nil, member.Tokens.AccessToken)
	if crossOrgGroupRes.Code != http.StatusNotFound {
		t.Fatalf("expected cross-organization group lookup 404, got %d", crossOrgGroupRes.Code)
	}

	tagRes := performJSON(env.router, http.MethodPut, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/tags/lobby", nil, owner.Tokens.AccessToken)
	if tagRes.Code != http.StatusOK {
		t.Fatalf("expected tag add 200, got %d: %s", tagRes.Code, tagRes.Body.String())
	}
	duplicateTagRes := performJSON(env.router, http.MethodPut, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/tags/lobby", nil, owner.Tokens.AccessToken)
	if duplicateTagRes.Code != http.StatusOK {
		t.Fatalf("expected duplicate tag to be idempotent 200, got %d", duplicateTagRes.Code)
	}
	memberTagRes := performJSON(env.router, http.MethodPut, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/tags/member-tag", nil, member.Tokens.AccessToken)
	if memberTagRes.Code != http.StatusForbidden {
		t.Fatalf("expected member tag write 403, got %d", memberTagRes.Code)
	}

	tagsRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/tags", nil, member.Tokens.AccessToken)
	if tagsRes.Code != http.StatusOK {
		t.Fatalf("expected member tag list 200, got %d", tagsRes.Code)
	}
	tags := decodeBody[deviceTagsBody](t, tagsRes)
	if len(tags.Tags) != 1 || tags.Tags[0].Tag != "lobby" || tags.Pagination.Total != 1 {
		t.Fatalf("unexpected tags response: %+v", tags)
	}

	deleteTagRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/tags/lobby", nil, admin.Tokens.AccessToken)
	if deleteTagRes.Code != http.StatusNoContent {
		t.Fatalf("expected admin tag delete 204, got %d", deleteTagRes.Code)
	}
	tagsAfterDeleteRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/tags", nil, owner.Tokens.AccessToken)
	if tagsAfterDeleteRes.Code != http.StatusOK {
		t.Fatalf("expected tag list after delete 200, got %d", tagsAfterDeleteRes.Code)
	}
	tagsAfterDelete := decodeBody[deviceTagsBody](t, tagsAfterDeleteRes)
	if tagsAfterDelete.Pagination.Total != 0 {
		t.Fatalf("expected deleted tag to be absent, got %+v", tagsAfterDelete)
	}
	missingDeviceTagRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/00000000-0000-0000-0000-000000000000/tags/lobby", nil, owner.Tokens.AccessToken)
	if missingDeviceTagRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing device tag delete 404, got %d", missingDeviceTagRes.Code)
	}
}

func TestIntegrationOwnerCanUpdateOrganization(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	admin := registerUser(t, env.router, "admin@example.com", "Admin Org")
	member := registerUser(t, env.router, "member@example.com", "Member Org")

	for _, user := range []struct {
		email string
		role  string
	}{
		{email: "admin@example.com", role: "admin"},
		{email: "member@example.com", role: "member"},
	} {
		res := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
			"email": user.email,
			"role":  user.role,
		}, owner.Tokens.AccessToken)
		if res.Code != http.StatusCreated {
			t.Fatalf("expected add %s member 201, got %d", user.role, res.Code)
		}
	}

	adminUpdateRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID, map[string]any{
		"name": "Admin Rename",
	}, admin.Tokens.AccessToken)
	if adminUpdateRes.Code != http.StatusForbidden {
		t.Fatalf("expected admin organization update 403, got %d", adminUpdateRes.Code)
	}

	memberUpdateRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID, map[string]any{
		"name": "Member Rename",
	}, member.Tokens.AccessToken)
	if memberUpdateRes.Code != http.StatusForbidden {
		t.Fatalf("expected member organization update 403, got %d", memberUpdateRes.Code)
	}

	blankUpdateRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID, map[string]any{
		"name": "   ",
	}, owner.Tokens.AccessToken)
	if blankUpdateRes.Code != http.StatusBadRequest {
		t.Fatalf("expected blank organization update 400, got %d", blankUpdateRes.Code)
	}

	updateRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID, map[string]any{
		"name": "Renamed Org",
	}, owner.Tokens.AccessToken)
	if updateRes.Code != http.StatusOK {
		t.Fatalf("expected owner organization update 200, got %d: %s", updateRes.Code, updateRes.Body.String())
	}
	body := decodeBody[organizationBody](t, updateRes)
	if body.Organization.Name != "Renamed Org" || body.Organization.Role != "owner" {
		t.Fatalf("unexpected organization update response: %+v", body.Organization)
	}

	crossOrgUpdateRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+admin.Organization.ID, map[string]any{
		"name": "Cross Org Rename",
	}, owner.Tokens.AccessToken)
	if crossOrgUpdateRes.Code != http.StatusNotFound {
		t.Fatalf("expected cross-organization update 404, got %d", crossOrgUpdateRes.Code)
	}
}

func TestIntegrationListPaginationMetadata(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	secondOrgRes := performJSON(env.router, http.MethodPost, "/v1/orgs", map[string]any{
		"name": "Second Org",
	}, owner.Tokens.AccessToken)
	if secondOrgRes.Code != http.StatusCreated {
		t.Fatalf("expected second org 201, got %d", secondOrgRes.Code)
	}

	orgsRes := performJSON(env.router, http.MethodGet, "/v1/orgs?limit=1&offset=1", nil, owner.Tokens.AccessToken)
	if orgsRes.Code != http.StatusOK {
		t.Fatalf("expected org list 200, got %d", orgsRes.Code)
	}
	orgsBody := decodeBody[organizationsBody](t, orgsRes)
	if len(orgsBody.Organizations) != 1 || orgsBody.Pagination.Limit != 1 || orgsBody.Pagination.Offset != 1 || orgsBody.Pagination.Total != 2 {
		t.Fatalf("unexpected org pagination response: %+v", orgsBody)
	}

	member := registerUser(t, env.router, "member@example.com", "Member Org")
	addMemberRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "member@example.com",
		"role":  "member",
	}, owner.Tokens.AccessToken)
	if addMemberRes.Code != http.StatusCreated {
		t.Fatalf("expected add member 201, got %d", addMemberRes.Code)
	}
	_ = member

	membersRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/members?limit=1&offset=1", nil, owner.Tokens.AccessToken)
	if membersRes.Code != http.StatusOK {
		t.Fatalf("expected member list 200, got %d", membersRes.Code)
	}
	membersBody := decodeBody[membersBody](t, membersRes)
	if len(membersBody.Members) != 1 || membersBody.Pagination.Limit != 1 || membersBody.Pagination.Offset != 1 || membersBody.Pagination.Total != 2 {
		t.Fatalf("unexpected member pagination response: %+v", membersBody)
	}

	for i, serial := range []string{"PAGE-1", "PAGE-2", "PAGE-3"} {
		res := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("page-device-"+serial, serial), owner.Tokens.AccessToken)
		if res.Code != http.StatusCreated {
			t.Fatalf("expected device %d create 201, got %d", i, res.Code)
		}
	}
	devicesRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices?limit=2&offset=1", nil, owner.Tokens.AccessToken)
	if devicesRes.Code != http.StatusOK {
		t.Fatalf("expected device list 200, got %d", devicesRes.Code)
	}
	devicesBody := decodeBody[devicesBody](t, devicesRes)
	if len(devicesBody.Devices) != 2 || devicesBody.Pagination.Limit != 2 || devicesBody.Pagination.Offset != 1 || devicesBody.Pagination.Total != 3 {
		t.Fatalf("unexpected device pagination response: %+v", devicesBody)
	}
}

func TestIntegrationMigrationsAreIdempotent(t *testing.T) {
	env := newIntegrationEnv(t)

	if err := database.Migrate(context.Background(), env.db); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationCleanupRefreshTokensRemovesExpiredAndRevokedRows(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	now := time.Now().UTC()
	for _, token := range []struct {
		hash      string
		expiresAt time.Time
		revokedAt *time.Time
	}{
		{hash: "expired", expiresAt: now.Add(-time.Hour)},
		{hash: "revoked", expiresAt: now.Add(time.Hour), revokedAt: &[]time.Time{now.Add(-time.Minute)}[0]},
		{hash: "active", expiresAt: now.Add(time.Hour)},
	} {
		_, err := env.db.Exec(context.Background(), `
			INSERT INTO refresh_tokens (user_id, token_hash, expires_at, revoked_at)
			VALUES ($1, $2, $3, $4)
		`, owner.User.ID, token.hash, token.expiresAt, token.revokedAt)
		if err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := store.New(env.db).CleanupRefreshTokens(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("expected 2 cleaned tokens, got %d", deleted)
	}

	var remaining int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM refresh_tokens WHERE token_hash = 'active'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("expected active token to remain, got %d", remaining)
	}
}

func TestIntegrationStoreRefreshTokenHelpers(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	tokenHash := auth.HashToken("store-refresh-token")
	if err := store.New(env.db).SaveRefreshToken(context.Background(), owner.User.ID, tokenHash, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	userID, err := store.New(env.db).RefreshTokenActive(context.Background(), tokenHash)
	if err != nil {
		t.Fatal(err)
	}
	if userID != owner.User.ID {
		t.Fatalf("expected active refresh token user %s, got %s", owner.User.ID, userID)
	}

	if err := store.New(env.db).RevokeUserRefreshTokens(context.Background(), owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.New(env.db).RefreshTokenActive(context.Background(), tokenHash); err == nil {
		t.Fatal("expected revoked refresh token to be inactive")
	}
}

func TestIntegrationLastOwnerCannotBeRemovedOrDowngraded(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")

	downgradeRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+owner.User.ID, map[string]any{
		"role": "admin",
	}, owner.Tokens.AccessToken)
	if downgradeRes.Code != http.StatusConflict {
		t.Fatalf("expected last owner downgrade 409, got %d", downgradeRes.Code)
	}

	removeRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/members/"+owner.User.ID, nil, owner.Tokens.AccessToken)
	if removeRes.Code != http.StatusConflict {
		t.Fatalf("expected last owner remove 409, got %d", removeRes.Code)
	}

	disableRes := performJSON(env.router, http.MethodPatch, "/v1/orgs/"+owner.Organization.ID+"/members/"+owner.User.ID+"/disable", nil, owner.Tokens.AccessToken)
	if disableRes.Code != http.StatusConflict {
		t.Fatalf("expected last owner disable 409, got %d", disableRes.Code)
	}

	_, err := env.db.Exec(context.Background(), `
		UPDATE organization_members SET role = 'admin'
		WHERE organization_id = $1 AND user_id = $2
	`, owner.Organization.ID, owner.User.ID)
	if err == nil {
		t.Fatal("expected direct SQL downgrade of last owner to fail")
	}

	_, err = env.db.Exec(context.Background(), `
		DELETE FROM organization_members
		WHERE organization_id = $1 AND user_id = $2
	`, owner.Organization.ID, owner.User.ID)
	if err == nil {
		t.Fatal("expected direct SQL deletion of last owner to fail")
	}

	disabledOwner := registerUser(t, env.router, "disabled-owner@example.com", "Disabled Owner Org")
	activeOwner := registerUser(t, env.router, "active-owner@example.com", "Active Owner Org")
	addActiveOwnerRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+disabledOwner.Organization.ID+"/members", map[string]any{
		"email": "active-owner@example.com",
		"role":  "owner",
	}, disabledOwner.Tokens.AccessToken)
	if addActiveOwnerRes.Code != http.StatusCreated {
		t.Fatalf("expected add active owner 201, got %d: %s", addActiveOwnerRes.Code, addActiveOwnerRes.Body.String())
	}
	deleteDisabledOwnerRes := performJSON(env.router, http.MethodDelete, "/v1/me", nil, disabledOwner.Tokens.AccessToken)
	if deleteDisabledOwnerRes.Code != http.StatusNoContent {
		t.Fatalf("expected disable original owner 204, got %d: %s", deleteDisabledOwnerRes.Code, deleteDisabledOwnerRes.Body.String())
	}

	_, err = env.db.Exec(context.Background(), `
		UPDATE organization_members SET role = 'admin'
		WHERE organization_id = $1 AND user_id = $2
	`, disabledOwner.Organization.ID, activeOwner.User.ID)
	if err == nil {
		t.Fatal("expected direct SQL downgrade of only active owner to fail")
	}

	_, err = env.db.Exec(context.Background(), `
		DELETE FROM organization_members
		WHERE organization_id = $1 AND user_id = $2
	`, disabledOwner.Organization.ID, activeOwner.User.ID)
	if err == nil {
		t.Fatal("expected direct SQL deletion of only active owner to fail")
	}
}

func TestIntegrationRejectsBlankNames(t *testing.T) {
	env := newIntegrationEnv(t)

	blankOrgRes := performJSON(env.router, http.MethodPost, "/v1/auth/register", map[string]any{
		"email":             "blank@example.com",
		"password":          "password123",
		"organization_name": "   ",
	}, "")
	if blankOrgRes.Code != http.StatusBadRequest {
		t.Fatalf("expected blank organization name 400, got %d", blankOrgRes.Code)
	}

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	blankDeviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", map[string]any{
		"name":     "   ",
		"category": "ip_camera",
	}, owner.Tokens.AccessToken)
	if blankDeviceRes.Code != http.StatusBadRequest {
		t.Fatalf("expected blank device name 400, got %d", blankDeviceRes.Code)
	}
}

func TestIntegrationDatabaseRejectsInvalidCoreData(t *testing.T) {
	env := newIntegrationEnv(t)

	if _, err := env.db.Exec(context.Background(), `
		INSERT INTO organizations (name) VALUES ('   ')
	`); err == nil {
		t.Fatal("expected database to reject blank organization name")
	}

	if _, err := env.db.Exec(context.Background(), `
		INSERT INTO users (email, password_hash) VALUES ('Upper@Example.com', 'hash')
	`); err == nil {
		t.Fatal("expected database to reject non-normalized email")
	}

	if _, err := env.db.Exec(context.Background(), `
		INSERT INTO organizations (name) VALUES ('Ownerless Org')
	`); err == nil {
		t.Fatal("expected database to reject organization without owner")
	}

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	if _, err := env.db.Exec(context.Background(), `
		INSERT INTO devices (organization_id, name, category) VALUES ($1, '   ', 'generic')
	`, owner.Organization.ID); err == nil {
		t.Fatal("expected database to reject blank device name")
	}
}

func TestIntegrationDatabaseMaintainsUpdatedAt(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")

	var updatedAt time.Time
	if err := env.db.QueryRow(context.Background(), `
		UPDATE organizations
		SET name = 'Updated Org', updated_at = '2000-01-01T00:00:00Z'
		WHERE id = $1
		RETURNING updated_at
	`, owner.Organization.ID).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	if updatedAt.Year() == 2000 {
		t.Fatalf("expected organization updated_at trigger to override manual timestamp, got %s", updatedAt)
	}

	var userUpdatedAt time.Time
	if err := env.db.QueryRow(context.Background(), `
		UPDATE users
		SET display_name = 'Updated User', updated_at = '2000-01-01T00:00:00Z'
		WHERE id = $1
		RETURNING updated_at
	`, owner.User.ID).Scan(&userUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if userUpdatedAt.Year() == 2000 {
		t.Fatalf("expected user updated_at trigger to override manual timestamp, got %s", userUpdatedAt)
	}
}

func TestIntegrationProvisioningEndpoints(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	admin := registerUser(t, env.router, "admin@example.com", "Admin Org")
	member := registerUser(t, env.router, "member@example.com", "Member Org")
	outsider := registerUser(t, env.router, "outsider@example.com", "Outsider Org")

	for _, membership := range []struct {
		email string
		role  string
	}{
		{email: "admin@example.com", role: "admin"},
		{email: "member@example.com", role: "member"},
	} {
		res := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
			"email": membership.email,
			"role":  membership.role,
		}, owner.Tokens.AccessToken)
		if res.Code != http.StatusCreated {
			t.Fatalf("expected add member %s 201, got %d: %s", membership.email, res.Code, res.Body.String())
		}
	}

	deviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("provision-device", "PROVISION-001"), owner.Tokens.AccessToken)
	if deviceRes.Code != http.StatusCreated {
		t.Fatalf("expected create device 201, got %d: %s", deviceRes.Code, deviceRes.Body.String())
	}
	device := decodeBody[deviceBody](t, deviceRes)

	memberProvisionRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-device-1",
		"activity_id":       "activity-1",
		"clip_public_key":   "clip-key-1",
	}, member.Tokens.AccessToken)
	if memberProvisionRes.Code != http.StatusForbidden {
		t.Fatalf("expected member provision 403, got %d", memberProvisionRes.Code)
	}

	rawClaimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-device-1",
		"activity_id":       "activity-1",
		"clip_public_key":   "clip-key-1",
		"serial_number":     "PROVISION-001",
	}, owner.Tokens.AccessToken)
	if rawClaimRes.Code != http.StatusBadRequest {
		t.Fatalf("expected raw claim material 400, got %d: %s", rawClaimRes.Code, rawClaimRes.Body.String())
	}

	unknownFieldRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-device-1",
		"activity_id":       "activity-1",
		"clip_public_key":   "clip-key-1",
		"future_claim_key":  "PROVISION-001",
	}, owner.Tokens.AccessToken)
	if unknownFieldRes.Code != http.StatusBadRequest {
		t.Fatalf("expected unknown provision field 400, got %d: %s", unknownFieldRes.Code, unknownFieldRes.Body.String())
	}

	provisionRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-device-1",
		"activity_id":       "activity-1",
		"clip_public_key":   "clip-key-1",
		"operation_id":      "provision-op-1",
	}, owner.Tokens.AccessToken)
	if provisionRes.Code != http.StatusCreated {
		t.Fatalf("expected provision 201, got %d: %s", provisionRes.Code, provisionRes.Body.String())
	}
	provisioned := decodeBody[operationBody](t, provisionRes)
	if provisioned.Operation.OperationID != "provision-op-1" {
		t.Fatalf("unexpected operation id: %+v", provisioned.Operation)
	}
	if provisioned.Operation.Status != "pending" {
		t.Fatalf("expected pending operation status, got %+v", provisioned.Operation)
	}

	reusedRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-device-1",
		"activity_id":       "activity-1",
		"clip_public_key":   "clip-key-1",
		"operation_id":      "provision-op-1",
	}, owner.Tokens.AccessToken)
	if reusedRes.Code != http.StatusOK {
		t.Fatalf("expected idempotent provision 200, got %d: %s", reusedRes.Code, reusedRes.Body.String())
	}
	reused := decodeBody[operationBody](t, reusedRes)
	if reused.Operation.MessageID != provisioned.Operation.MessageID {
		t.Fatalf("expected reused provision to keep message id, got first=%s second=%s", provisioned.Operation.MessageID, reused.Operation.MessageID)
	}

	conflictRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-device-1",
		"activity_id":       "activity-2",
		"clip_public_key":   "clip-key-1",
		"operation_id":      "provision-op-1",
	}, owner.Tokens.AccessToken)
	if conflictRes.Code != http.StatusConflict {
		t.Fatalf("expected conflicting provision 409, got %d: %s", conflictRes.Code, conflictRes.Body.String())
	}

	var operationCount int
	if err := env.db.QueryRow(context.Background(), `
		SELECT count(*) FROM device_operations WHERE operation_id = 'provision-op-1'
	`).Scan(&operationCount); err != nil {
		t.Fatal(err)
	}
	if operationCount != 1 {
		t.Fatalf("expected one provision operation row, got %d", operationCount)
	}

	var outboxCount int
	if err := env.db.QueryRow(context.Background(), `
		SELECT count(*) FROM device_message_outbox WHERE operation_id = 'provision-op-1'
	`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 1 {
		t.Fatalf("expected one outbox row, got %d", outboxCount)
	}

	provisionMessage, err := store.New(env.db).GetLatestOutboxMessageByOperationID(context.Background(), "provision-op-1")
	if err != nil {
		t.Fatal(err)
	}
	provisionPayload := validateAccountCommandEnvelope(t, provisionMessage)
	provisionCommand, ok := provisionPayload.(*channel.DeviceProvisionRequestedPayload)
	if !ok {
		t.Fatalf("expected provision payload type, got %T", provisionPayload)
	}
	if provisionCommand.ActivityID != "activity-1" || provisionCommand.ClipPublicKey != "clip-key-1" || provisionCommand.VideoCloudDevid != "video-device-1" {
		t.Fatalf("unexpected provision command payload: %+v", provisionCommand)
	}

	memberStateRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provisioning", nil, member.Tokens.AccessToken)
	if memberStateRes.Code != http.StatusOK {
		t.Fatalf("expected member provisioning state 200, got %d: %s", memberStateRes.Code, memberStateRes.Body.String())
	}
	memberState := decodeBody[provisioningBody](t, memberStateRes)
	if memberState.Operation == nil || memberState.Operation.OperationID != "provision-op-1" {
		t.Fatalf("unexpected provisioning state operation: %+v", memberState.Operation)
	}
	if got := memberState.VideoMetadata[model.DeviceMetadataVideoCloudDevid]; got != "video-device-1" {
		t.Fatalf("expected pending devid in provisioning state, got %+v", got)
	}
	if got := memberState.VideoMetadata[model.DeviceMetadataVideoCloudActivityID]; got != "activity-1" {
		t.Fatalf("expected pending activity id in provisioning state, got %+v", got)
	}
	if got := memberState.VideoMetadata[model.DeviceMetadataVideoCloudClipPublicKey]; got != "clip-key-1" {
		t.Fatalf("expected pending clip public key in provisioning state, got %+v", got)
	}
	if got := memberState.VideoMetadata[model.DeviceMetadataVideoCloudActivationStatus]; got != string(model.VideoCloudActivationStatusPending) {
		t.Fatalf("expected pending activation status in provisioning state, got %+v", got)
	}
	if memberState.Readiness.State != model.DeviceReadinessStateActivationPending {
		t.Fatalf("expected pending readiness state, got %+v", memberState.Readiness)
	}
	if memberState.Readiness.ProductState != model.ProductReadinessStateCloudActivationPending {
		t.Fatalf("expected pending product readiness state, got %+v", memberState.Readiness)
	}
	if memberState.Readiness.Sources.DeviceStatus != model.DeviceStatusUnknown ||
		memberState.Readiness.Sources.ProvisioningOperationStatus == nil ||
		*memberState.Readiness.Sources.ProvisioningOperationStatus != model.DeviceOperationStatusPending ||
		memberState.Readiness.Sources.VideoCloudActivationStatus == nil ||
		*memberState.Readiness.Sources.VideoCloudActivationStatus != string(model.VideoCloudActivationStatusPending) {
		t.Fatalf("expected readiness sources to identify pending lifecycle facts, got %+v", memberState.Readiness.Sources)
	}

	pendingDevice, err := store.New(env.db).GetDevice(context.Background(), owner.Organization.ID, device.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pendingDevice.Status != model.DeviceStatusUnknown {
		t.Fatalf("expected accepted provisioning not to set device online, got %s", pendingDevice.Status)
	}

	outsiderStateRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provisioning", nil, outsider.Tokens.AccessToken)
	if outsiderStateRes.Code != http.StatusNotFound {
		t.Fatalf("expected cross-org provisioning state 404, got %d", outsiderStateRes.Code)
	}

	disableProvisionedRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID, nil, owner.Tokens.AccessToken)
	if disableProvisionedRes.Code != http.StatusNoContent {
		t.Fatalf("expected disable provisioned device 204, got %d: %s", disableProvisionedRes.Code, disableProvisionedRes.Body.String())
	}

	reusedDisabledRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-device-1",
		"activity_id":       "activity-1",
		"clip_public_key":   "clip-key-1",
		"operation_id":      "provision-op-1",
	}, owner.Tokens.AccessToken)
	if reusedDisabledRes.Code != http.StatusOK {
		t.Fatalf("expected disabled-device idempotent provision 200, got %d: %s", reusedDisabledRes.Code, reusedDisabledRes.Body.String())
	}
	reusedDisabled := decodeBody[operationBody](t, reusedDisabledRes)
	if reusedDisabled.Operation.MessageID != provisioned.Operation.MessageID {
		t.Fatalf("expected disabled-device retry to keep message id, got first=%s second=%s", provisioned.Operation.MessageID, reusedDisabled.Operation.MessageID)
	}

	disabledDeviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("disabled-device", "PROVISION-002"), owner.Tokens.AccessToken)
	if disabledDeviceRes.Code != http.StatusCreated {
		t.Fatalf("expected disabled fixture device 201, got %d: %s", disabledDeviceRes.Code, disabledDeviceRes.Body.String())
	}
	disabledDevice := decodeBody[deviceBody](t, disabledDeviceRes)
	deleteDisabledRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+disabledDevice.Device.ID, nil, owner.Tokens.AccessToken)
	if deleteDisabledRes.Code != http.StatusNoContent {
		t.Fatalf("expected disable fixture device 204, got %d", deleteDisabledRes.Code)
	}

	provisionDisabledRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+disabledDevice.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-device-disabled",
		"activity_id":       "activity-disabled",
		"clip_public_key":   "clip-key-disabled",
	}, owner.Tokens.AccessToken)
	if provisionDisabledRes.Code != http.StatusConflict {
		t.Fatalf("expected disabled device provision 409, got %d: %s", provisionDisabledRes.Code, provisionDisabledRes.Body.String())
	}

	adminDeviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("admin-provision-device", "PROVISION-003"), owner.Tokens.AccessToken)
	if adminDeviceRes.Code != http.StatusCreated {
		t.Fatalf("expected admin fixture device 201, got %d: %s", adminDeviceRes.Code, adminDeviceRes.Body.String())
	}
	adminDevice := decodeBody[deviceBody](t, adminDeviceRes)

	adminProvisionRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+adminDevice.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-device-admin",
		"activity_id":       "activity-admin",
		"clip_public_key":   "clip-key-admin",
	}, admin.Tokens.AccessToken)
	if adminProvisionRes.Code != http.StatusCreated {
		t.Fatalf("expected admin provision 201, got %d: %s", adminProvisionRes.Code, adminProvisionRes.Body.String())
	}
}

func TestIntegrationProvisioningStateReturnsRegistryOnlyReadiness(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "registry-owner@example.com", "Registry Owner Org")
	member := registerUser(t, env.router, "registry-member@example.com", "Registry Member Org")

	addMemberRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": "registry-member@example.com",
		"role":  "member",
	}, owner.Tokens.AccessToken)
	if addMemberRes.Code != http.StatusCreated {
		t.Fatalf("expected add member 201, got %d: %s", addMemberRes.Code, addMemberRes.Body.String())
	}

	deviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("registry-only-device", "REGISTRY-001"), owner.Tokens.AccessToken)
	if deviceRes.Code != http.StatusCreated {
		t.Fatalf("expected create device 201, got %d: %s", deviceRes.Code, deviceRes.Body.String())
	}
	device := decodeBody[deviceBody](t, deviceRes)

	stateRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provisioning", nil, member.Tokens.AccessToken)
	if stateRes.Code != http.StatusOK {
		t.Fatalf("expected registry-only provisioning state 200, got %d: %s", stateRes.Code, stateRes.Body.String())
	}
	state := decodeBody[registryOnlyProvisioningBody](t, stateRes)
	if state.Operation != nil {
		t.Fatalf("expected registry-only operation to be nil, got %+v", state.Operation)
	}
	if state.Readiness.State != model.DeviceReadinessStateActivationPending {
		t.Fatalf("expected registry-only readiness activation_pending, got %+v", state.Readiness)
	}
	if state.Readiness.ProductState != model.ProductReadinessStateRegistered {
		t.Fatalf("expected registry-only product state registered, got %+v", state.Readiness)
	}
	if state.Readiness.Sources.ProvisioningOperationStatus != nil {
		t.Fatalf("expected registry-only provisioning operation source to be nil, got %+v", state.Readiness.Sources)
	}

	disabledDeviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("disabled-registry-device", "REGISTRY-002"), owner.Tokens.AccessToken)
	if disabledDeviceRes.Code != http.StatusCreated {
		t.Fatalf("expected create disabled fixture 201, got %d: %s", disabledDeviceRes.Code, disabledDeviceRes.Body.String())
	}
	disabledDevice := decodeBody[deviceBody](t, disabledDeviceRes)
	deleteDisabledRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+disabledDevice.Device.ID, nil, owner.Tokens.AccessToken)
	if deleteDisabledRes.Code != http.StatusNoContent {
		t.Fatalf("expected disable registry fixture 204, got %d: %s", deleteDisabledRes.Code, deleteDisabledRes.Body.String())
	}
	disabledStateRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices/"+disabledDevice.Device.ID+"/provisioning", nil, owner.Tokens.AccessToken)
	if disabledStateRes.Code != http.StatusOK {
		t.Fatalf("expected disabled registry-only provisioning state 200, got %d: %s", disabledStateRes.Code, disabledStateRes.Body.String())
	}
	disabledState := decodeBody[registryOnlyProvisioningBody](t, disabledStateRes)
	if disabledState.Operation != nil {
		t.Fatalf("expected disabled registry-only operation to be nil, got %+v", disabledState.Operation)
	}
	if disabledState.Readiness.State != model.DeviceReadinessStateDisabled {
		t.Fatalf("expected disabled registry-only readiness disabled, got %+v", disabledState.Readiness)
	}
	if disabledState.Readiness.ProductState != model.ProductReadinessStateRegistered {
		t.Fatalf("expected disabled registry-only product state registered, got %+v", disabledState.Readiness)
	}

	missingStateRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices/00000000-0000-0000-0000-000000000000/provisioning", nil, owner.Tokens.AccessToken)
	if missingStateRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing device provisioning state 404, got %d: %s", missingStateRes.Code, missingStateRes.Body.String())
	}
}

func TestIntegrationClaimResolveEndpoint(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()
	claims := store.New(env.db)

	owner := registerUser(t, env.router, "claim-owner@example.com", "Claim Owner Org")
	admin := registerUser(t, env.router, "claim-admin@example.com", "Claim Admin Org")
	member := registerUser(t, env.router, "claim-member@example.com", "Claim Member Org")
	otherOwner := registerUser(t, env.router, "claim-other@example.com", "Claim Other Org")

	for _, membership := range []struct {
		email string
		role  string
	}{
		{email: "claim-admin@example.com", role: "admin"},
		{email: "claim-member@example.com", role: "member"},
	} {
		res := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
			"email": membership.email,
			"role":  membership.role,
		}, owner.Tokens.AccessToken)
		if res.Code != http.StatusCreated {
			t.Fatalf("expected add member %s 201, got %d: %s", membership.email, res.Code, res.Body.String())
		}
	}

	seedClaimToken := func(rawToken, videoDevid string, expiresAt time.Time, orgID *string, category model.DeviceCategory) {
		t.Helper()
		if _, err := claims.CreateDeviceClaimToken(ctx, store.DeviceClaimTokenCreateInput{
			OrganizationID:  orgID,
			TokenHash:       auth.HashToken(rawToken),
			Category:        category,
			VideoCloudDevid: videoDevid,
			ActivityID:      "activity-" + videoDevid,
			ClipPublicKey:   "clip-key-" + videoDevid,
			ExpiresAt:       expiresAt,
			Now:             time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	ownerOrgID := owner.Organization.ID
	seedClaimToken("claim-token-owner", "claim-video-owner", time.Now().Add(time.Hour), &ownerOrgID, model.DeviceCategoryIPCamera)
	claimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "claim-token-owner",
		"device_name": "Front Door Camera",
	}, owner.Tokens.AccessToken)
	if claimRes.Code != http.StatusCreated {
		t.Fatalf("expected claim resolve 201, got %d: %s", claimRes.Code, claimRes.Body.String())
	}
	claimBody := decodeBody[claimResolveBody](t, claimRes)
	if claimBody.ClaimID == "" || claimBody.Device.ID == "" {
		t.Fatalf("expected claim id and device id, got %+v", claimBody)
	}
	if claimBody.Device.Name != "Front Door Camera" {
		t.Fatalf("expected resolved device name, got %+v", claimBody.Device)
	}
	if claimBody.ProvisionInput.VideoCloudDevid != "claim-video-owner" ||
		claimBody.ProvisionInput.ActivityID != "activity-claim-video-owner" ||
		claimBody.ProvisionInput.ClipPublicKey != "clip-key-claim-video-owner" {
		t.Fatalf("unexpected provision input: %+v", claimBody.ProvisionInput)
	}

	var operationCount int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM device_operations WHERE device_id = $1`, claimBody.Device.ID).Scan(&operationCount); err != nil {
		t.Fatal(err)
	}
	if operationCount != 0 {
		t.Fatalf("claim resolve must not create provisioning operations, got %d", operationCount)
	}
	var outboxCount int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM device_message_outbox WHERE partition_key = $1`, claimBody.Device.ID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 0 {
		t.Fatalf("claim resolve must not publish outbox messages, got %d", outboxCount)
	}

	seedClaimToken("claim-token-admin", "claim-video-admin", time.Now().Add(time.Hour), &ownerOrgID, model.DeviceCategoryIPCamera)
	adminClaimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "claim-token-admin",
		"device_name": "Admin Camera",
	}, admin.Tokens.AccessToken)
	if adminClaimRes.Code != http.StatusCreated {
		t.Fatalf("expected admin claim resolve 201, got %d: %s", adminClaimRes.Code, adminClaimRes.Body.String())
	}

	seedClaimToken("claim-token-member", "claim-video-member", time.Now().Add(time.Hour), &ownerOrgID, model.DeviceCategoryIPCamera)
	memberClaimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "claim-token-member",
		"device_name": "Member Camera",
	}, member.Tokens.AccessToken)
	if memberClaimRes.Code != http.StatusForbidden {
		t.Fatalf("expected member claim resolve 403, got %d: %s", memberClaimRes.Code, memberClaimRes.Body.String())
	}

	invalidClaimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "missing-token",
		"device_name": "Missing Camera",
	}, owner.Tokens.AccessToken)
	assertErrorCode(t, invalidClaimRes, http.StatusNotFound, "invalid_claim_token")

	seedClaimToken("claim-token-expired", "claim-video-expired", time.Now().Add(-time.Hour), &ownerOrgID, model.DeviceCategoryIPCamera)
	expiredClaimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "claim-token-expired",
		"device_name": "Expired Camera",
	}, owner.Tokens.AccessToken)
	assertErrorCode(t, expiredClaimRes, http.StatusBadRequest, "expired_claim_token")

	alreadyClaimedRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "claim-token-owner",
		"device_name": "Front Door Again",
	}, owner.Tokens.AccessToken)
	assertErrorCode(t, alreadyClaimedRes, http.StatusConflict, "already_claimed")

	seedClaimToken("claim-token-cross-org", "claim-video-cross-org", time.Now().Add(time.Hour), &ownerOrgID, model.DeviceCategoryIPCamera)
	crossOrgClaimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+otherOwner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "claim-token-cross-org",
		"device_name": "Cross Org Camera",
	}, otherOwner.Tokens.AccessToken)
	assertErrorCode(t, crossOrgClaimRes, http.StatusForbidden, "forbidden")

	seedClaimToken("claim-token-unsupported", "claim-video-unsupported", time.Now().Add(time.Hour), &ownerOrgID, model.DeviceCategoryMQTT)
	unsupportedClaimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "claim-token-unsupported",
		"device_name": "MQTT Device",
	}, owner.Tokens.AccessToken)
	assertErrorCode(t, unsupportedClaimRes, http.StatusBadRequest, "unsupported_device_category")
}

func TestIntegrationDeactivateEndpointUsesProjectedVideoMetadata(t *testing.T) {
	env := newIntegrationEnv(t)

	owner := registerUser(t, env.router, "owner@example.com", "Owner Org")
	admin := registerUser(t, env.router, "admin@example.com", "Admin Org")
	member := registerUser(t, env.router, "member@example.com", "Member Org")
	outsider := registerUser(t, env.router, "outsider@example.com", "Outsider Org")
	for _, membership := range []struct {
		email string
		role  string
	}{
		{email: "admin@example.com", role: "admin"},
		{email: "member@example.com", role: "member"},
	} {
		res := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
			"email": membership.email,
			"role":  membership.role,
		}, owner.Tokens.AccessToken)
		if res.Code != http.StatusCreated {
			t.Fatalf("expected add member %s 201, got %d: %s", membership.email, res.Code, res.Body.String())
		}
	}

	deviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("deactivate-device", "DEACTIVATE-001"), owner.Tokens.AccessToken)
	if deviceRes.Code != http.StatusCreated {
		t.Fatalf("expected create device 201, got %d: %s", deviceRes.Code, deviceRes.Body.String())
	}
	device := decodeBody[deviceBody](t, deviceRes)

	projected, err := store.New(env.db).ProjectDevice(context.Background(), owner.Organization.ID, device.Device.ID, store.ProvisionSucceededProjection(channel.DeviceProvisionSucceededPayload{
		OrgID:           owner.Organization.ID,
		AccountDeviceID: device.Device.ID,
		VideoCloudDevid: "video-device-1",
		ActivityID:      "activity-1",
		ActivatedAt:     time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if projected.Metadata[model.DeviceMetadataVideoCloudDevid] != "video-device-1" {
		t.Fatalf("expected projected video metadata, got %+v", projected.Metadata)
	}

	memberDeactivateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/deactivate", map[string]any{
		"operation_id": "member-deactivate-op-1",
	}, member.Tokens.AccessToken)
	if memberDeactivateRes.Code != http.StatusForbidden {
		t.Fatalf("expected member deactivate 403, got %d", memberDeactivateRes.Code)
	}

	outsiderDeactivateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/deactivate", map[string]any{
		"operation_id": "outsider-deactivate-op-1",
	}, outsider.Tokens.AccessToken)
	if outsiderDeactivateRes.Code != http.StatusNotFound {
		t.Fatalf("expected cross-org deactivate 404, got %d", outsiderDeactivateRes.Code)
	}

	disableRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID, nil, owner.Tokens.AccessToken)
	if disableRes.Code != http.StatusNoContent {
		t.Fatalf("expected disable device 204, got %d: %s", disableRes.Code, disableRes.Body.String())
	}

	deactivateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/deactivate", map[string]any{
		"operation_id": "deactivate-op-1",
	}, admin.Tokens.AccessToken)
	if deactivateRes.Code != http.StatusCreated {
		t.Fatalf("expected deactivate 201, got %d: %s", deactivateRes.Code, deactivateRes.Body.String())
	}
	deactivated := decodeBody[operationBody](t, deactivateRes)
	if deactivated.Operation.OperationType != "deactivate" {
		t.Fatalf("expected deactivate operation type, got %+v", deactivated.Operation)
	}
	if deactivated.Operation.RequestedBy == nil || *deactivated.Operation.RequestedBy != admin.User.ID {
		t.Fatalf("expected admin requester in operation, got %+v", deactivated.Operation)
	}

	reusedDeactivateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/deactivate", map[string]any{
		"operation_id": "deactivate-op-1",
	}, admin.Tokens.AccessToken)
	if reusedDeactivateRes.Code != http.StatusOK {
		t.Fatalf("expected idempotent deactivate 200, got %d: %s", reusedDeactivateRes.Code, reusedDeactivateRes.Body.String())
	}
	reusedDeactivate := decodeBody[operationBody](t, reusedDeactivateRes)
	if reusedDeactivate.Operation.MessageID != deactivated.Operation.MessageID {
		t.Fatalf("expected reused deactivate to keep message id, got first=%s second=%s", deactivated.Operation.MessageID, reusedDeactivate.Operation.MessageID)
	}

	conflictDeactivateRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/deactivate", map[string]any{
		"operation_id": "deactivate-op-1",
		"reason":       "user_request",
	}, admin.Tokens.AccessToken)
	if conflictDeactivateRes.Code != http.StatusConflict {
		t.Fatalf("expected conflicting deactivate 409, got %d: %s", conflictDeactivateRes.Code, conflictDeactivateRes.Body.String())
	}

	var messageType string
	var partitionKey string
	var payload []byte
	if err := env.db.QueryRow(context.Background(), `
		SELECT message_type, partition_key, payload
		FROM device_message_outbox
		WHERE operation_id = 'deactivate-op-1'
	`).Scan(&messageType, &partitionKey, &payload); err != nil {
		t.Fatal(err)
	}
	if messageType != "DeviceDeactivateRequested" {
		t.Fatalf("expected deactivate outbox message type, got %s", messageType)
	}
	if partitionKey != device.Device.ID {
		t.Fatalf("expected partition key %s, got %s", device.Device.ID, partitionKey)
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["video_cloud_devid"] != "video-device-1" {
		t.Fatalf("expected projected video devid in payload, got %+v", decoded)
	}
	if decoded["reason"] != defaultDeactivationReason {
		t.Fatalf("expected default deactivation reason in payload, got %+v", decoded)
	}

	var outboxCount int
	if err := env.db.QueryRow(context.Background(), `
		SELECT count(*) FROM device_message_outbox WHERE operation_id = 'deactivate-op-1'
	`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 1 {
		t.Fatalf("expected one deactivate outbox row, got %d", outboxCount)
	}

	deactivateMessage, err := store.New(env.db).GetLatestOutboxMessageByOperationID(context.Background(), "deactivate-op-1")
	if err != nil {
		t.Fatal(err)
	}
	deactivatePayload := validateAccountCommandEnvelope(t, deactivateMessage)
	deactivateCommand, ok := deactivatePayload.(*channel.DeviceDeactivateRequestedPayload)
	if !ok {
		t.Fatalf("expected deactivate payload type, got %T", deactivatePayload)
	}
	if deactivateCommand.VideoCloudDevid != "video-device-1" || deactivateCommand.Reason != defaultDeactivationReason {
		t.Fatalf("unexpected deactivate command payload: %+v", deactivateCommand)
	}

	if _, err := env.db.Exec(context.Background(), `
		UPDATE devices
		SET metadata = '{}'::jsonb
		WHERE id = $1
	`, device.Device.ID); err != nil {
		t.Fatal(err)
	}

	reusedMissingMetadataRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/deactivate", map[string]any{
		"operation_id": "deactivate-op-1",
	}, admin.Tokens.AccessToken)
	if reusedMissingMetadataRes.Code != http.StatusOK {
		t.Fatalf("expected missing-metadata idempotent deactivate 200, got %d: %s", reusedMissingMetadataRes.Code, reusedMissingMetadataRes.Body.String())
	}
	reusedMissingMetadata := decodeBody[operationBody](t, reusedMissingMetadataRes)
	if reusedMissingMetadata.Operation.MessageID != deactivated.Operation.MessageID {
		t.Fatalf("expected missing-metadata retry to keep message id, got first=%s second=%s", deactivated.Operation.MessageID, reusedMissingMetadata.Operation.MessageID)
	}

	missingMetadataRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("plain-device", "DEACTIVATE-002"), owner.Tokens.AccessToken)
	if missingMetadataRes.Code != http.StatusCreated {
		t.Fatalf("expected plain device 201, got %d: %s", missingMetadataRes.Code, missingMetadataRes.Body.String())
	}
	plainDevice := decodeBody[deviceBody](t, missingMetadataRes)

	deactivateMissingMetadataRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+plainDevice.Device.ID+"/deactivate", map[string]any{}, owner.Tokens.AccessToken)
	if deactivateMissingMetadataRes.Code != http.StatusConflict {
		t.Fatalf("expected unprojected deactivate 409, got %d: %s", deactivateMissingMetadataRes.Code, deactivateMissingMetadataRes.Body.String())
	}
}

type registerBody struct {
	User struct {
		ID                        string `json:"id"`
		EmailVerified             bool   `json:"email_verified"`
		SignupPendingVerification bool   `json:"signup_pending_verification"`
	} `json:"user"`
	Organization struct {
		ID                    string `json:"id"`
		Tier                  string `json:"tier"`
		EvaluationDeviceQuota int    `json:"evaluation_device_quota"`
	} `json:"organization"`
	Tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	} `json:"tokens"`
}

type signupBody struct {
	User struct {
		ID                        string `json:"id"`
		EmailVerified             bool   `json:"email_verified"`
		SignupPendingVerification bool   `json:"signup_pending_verification"`
	} `json:"user"`
	Organization struct {
		ID                    string `json:"id"`
		Tier                  string `json:"tier"`
		EvaluationDeviceQuota int    `json:"evaluation_device_quota"`
	} `json:"organization"`
}

type userBody struct {
	User struct {
		ID                        string     `json:"id"`
		EmailVerified             bool       `json:"email_verified"`
		SignupPendingVerification bool       `json:"signup_pending_verification"`
		EmailVerifiedAt           *time.Time `json:"email_verified_at"`
	} `json:"user"`
}

type tokenBody struct {
	Tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	} `json:"tokens"`
}

type meBody struct {
	User struct {
		ID            string `json:"id"`
		EmailVerified bool   `json:"email_verified"`
	} `json:"user"`
	Organizations []struct {
		ID string `json:"id"`
	} `json:"organizations"`
}

type deviceBody struct {
	Device struct {
		ID           string  `json:"id"`
		Name         string  `json:"name"`
		SerialNumber *string `json:"serial_number"`
	} `json:"device"`
}

type claimResolveBody struct {
	ClaimID string `json:"claim_id"`
	Device  struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"device"`
	ProvisionInput struct {
		VideoCloudDevid string `json:"video_cloud_devid"`
		ActivityID      string `json:"activity_id"`
		ClipPublicKey   string `json:"clip_public_key"`
	} `json:"provision_input"`
}

type registryOnlyProvisioningBody struct {
	Operation *operationResponse `json:"operation"`
	Readiness struct {
		State        model.DeviceReadinessState  `json:"state"`
		ProductState model.ProductReadinessState `json:"product_state"`
		Sources      struct {
			ProvisioningOperationStatus *model.DeviceOperationStatus `json:"provisioning_operation_status"`
		} `json:"sources"`
	} `json:"readiness"`
}

type devicesBody struct {
	Devices []struct {
		ID string `json:"id"`
	} `json:"devices"`
	Pagination paginationBody `json:"pagination"`
}

type deviceGroupBody struct {
	Group struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DeviceCount *int   `json:"device_count"`
	} `json:"group"`
}

type deviceGroupsBody struct {
	Groups []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DeviceCount *int   `json:"device_count"`
	} `json:"groups"`
	Pagination paginationBody `json:"pagination"`
}

type deviceTagsBody struct {
	Tags []struct {
		DeviceID string `json:"device_id"`
		Tag      string `json:"tag"`
	} `json:"tags"`
	Pagination paginationBody `json:"pagination"`
}

type organizationsBody struct {
	Organizations []struct {
		ID string `json:"id"`
	} `json:"organizations"`
	Pagination paginationBody `json:"pagination"`
}

type organizationBody struct {
	Organization struct {
		ID                    string `json:"id"`
		Name                  string `json:"name"`
		Role                  string `json:"role"`
		Tier                  string `json:"tier"`
		EvaluationDeviceQuota int    `json:"evaluation_device_quota"`
	} `json:"organization"`
}

type quotaRaiseRequestBody struct {
	QuotaRaiseRequest struct {
		ID             string `json:"id"`
		Status         string `json:"status"`
		RequestedQuota int    `json:"requested_quota"`
	} `json:"quota_raise_request"`
}

type quotaRaiseDecisionBody struct {
	QuotaRaiseRequest struct {
		ID             string `json:"id"`
		Status         string `json:"status"`
		RequestedQuota int    `json:"requested_quota"`
	} `json:"quota_raise_request"`
	Organization struct {
		ID                    string `json:"id"`
		Tier                  string `json:"tier"`
		EvaluationDeviceQuota int    `json:"evaluation_device_quota"`
	} `json:"organization"`
}

type evalTierMetricsBody struct {
	Signups struct {
		EvaluationCreated          int64   `json:"evaluation_created"`
		CommercialCreated          int64   `json:"commercial_created"`
		VerificationCompleted      int64   `json:"verification_completed"`
		VerificationCompletionRate float64 `json:"verification_completion_rate"`
	} `json:"signups"`
	QuotaRaiseRequests struct {
		Pending  int64 `json:"pending"`
		Approved int64 `json:"approved"`
		Declined int64 `json:"declined"`
	} `json:"quota_raise_requests"`
	EvaluationQuotaUsage []struct {
		OrganizationID        string  `json:"organization_id"`
		OrganizationName      string  `json:"organization_name"`
		ActiveDevices         int     `json:"active_devices"`
		EvaluationDeviceQuota int     `json:"evaluation_device_quota"`
		Utilization           float64 `json:"utilization"`
	} `json:"evaluation_quota_usage"`
}

type membersBody struct {
	Members []struct {
		UserID     string     `json:"user_id"`
		DisabledAt *time.Time `json:"disabled_at"`
	} `json:"members"`
	Pagination paginationBody `json:"pagination"`
}

type memberBody struct {
	Member struct {
		UserID     string     `json:"user_id"`
		Role       string     `json:"role"`
		DisabledAt *time.Time `json:"disabled_at"`
	} `json:"member"`
}

type paginationBody struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}

func registerUser(t *testing.T, router *gin.Engine, email, orgName string) registerBody {
	t.Helper()
	res := performJSON(router, http.MethodPost, "/v1/auth/register", map[string]any{
		"email":             email,
		"password":          "password123",
		"display_name":      email,
		"organization_name": orgName,
	}, "")
	if res.Code != http.StatusCreated {
		t.Fatalf("expected register 201, got %d: %s", res.Code, res.Body.String())
	}
	return decodeBody[registerBody](t, res)
}

func latestAuthToken(t *testing.T, sink *recordingAuthTokenSink, email, purpose string) string {
	t.Helper()
	for i := len(sink.deliveries) - 1; i >= 0; i-- {
		delivery := sink.deliveries[i]
		if delivery.Email == email && delivery.Purpose == purpose {
			if delivery.Token == "" || delivery.ExpiresAt.IsZero() {
				t.Fatalf("unexpected empty token delivery: %+v", delivery)
			}
			return delivery.Token
		}
	}
	t.Fatalf("missing %s token delivery for %s in %+v", purpose, email, sink.deliveries)
	return ""
}

func devicePayload(name, serial string) map[string]any {
	return map[string]any{
		"name":          name,
		"category":      "ip_camera",
		"serial_number": serial,
		"metadata": map[string]any{
			"location": "lab",
		},
	}
}

func performJSON(router *gin.Engine, method, path string, body any, accessToken string) *httptest.ResponseRecorder {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	return performRaw(router, method, path, payload, accessToken)
}

func performRaw(router *gin.Engine, method, path string, payload []byte, accessToken string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	return res
}

func decodeBody[T any](t *testing.T, res *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v: %s", err, res.Body.String())
	}
	return out
}

func assertErrorCode(t *testing.T, res *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if res.Code != status {
		t.Fatalf("expected status %d, got %d: %s", status, res.Code, res.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v: %s", err, res.Body.String())
	}
	if body.Error.Code != code {
		t.Fatalf("expected error code %q, got %q: %s", code, body.Error.Code, res.Body.String())
	}
}

func validateAccountCommandEnvelope(t *testing.T, message model.DeviceMessageOutbox) channel.Payload {
	t.Helper()

	payload, err := json.Marshal(message.Payload)
	if err != nil {
		t.Fatalf("marshal outbox payload: %v", err)
	}

	envelope := channel.Envelope{
		MessageID:     message.MessageID,
		CorrelationID: message.CorrelationID,
		OperationID:   message.OperationID,
		SourceService: channel.ServiceAccountManager,
		TargetService: channel.ServiceRealtekVideoCloud,
		MessageType:   channel.MessageType(message.MessageType),
		SchemaVersion: message.SchemaVersion,
		PartitionKey:  message.PartitionKey,
		OccurredAt:    message.CreatedAt.UTC(),
		Payload:       payload,
	}

	decoded, err := envelope.ValidateAndDecode(message.Stream)
	if err != nil {
		t.Fatalf("validate outbox envelope: %v", err)
	}
	return decoded
}
