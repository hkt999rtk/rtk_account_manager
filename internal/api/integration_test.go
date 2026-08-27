package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/emaildelivery"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
	"rtk_account_manager/internal/testutil"
)

type integrationEnv struct {
	router           *gin.Engine
	server           *Server
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
				TRUNCATE email_outbox, chipset_information_providers, acl_audit_events, external_group_mappings, role_assignments, oidc_login_states, user_identities, identity_providers, auth_tokens, quota_raise_requests, device_user_bindings, brand_cloud_end_users, end_user_refresh_tokens, end_user_identities, device_claims, device_claim_tokens, device_item_profiles, device_message_inbox, device_message_outbox, device_operations, app_certificates, brand_cloud_refresh_tokens, refresh_tokens, device_tags, device_group_members, device_groups, devices, brand_cloud_memberships, organization_members, brand_cloud_users, end_users, organizations, users
		RESTART IDENTITY CASCADE
	`); err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	authService := auth.NewService("test-access-secret", "test-refresh-secret", time.Minute, time.Hour)
	tokenSink := &recordingAuthTokenSink{}
	notificationSink := &recordingQuotaRaiseNotificationSink{}
	server := NewWithAuthTokenAndNotificationSink(store.New(db), authService, tokenSink, notificationSink)
	server.signupLimiter = nil
	return integrationEnv{
		router:           server.Router(),
		server:           server,
		db:               db,
		tokenSink:        tokenSink,
		notificationSink: notificationSink,
	}
}

func TestIntegrationSignupQueuesEncryptedEmailWithoutCallingSMTP(t *testing.T) {
	env := newIntegrationEnv(t)
	repository, ok := env.server.store.(*store.Store)
	if !ok {
		t.Fatal("integration store is not the concrete store")
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = 4
	}
	cipher, err := emaildelivery.NewCipher(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	repository.ConfigureEmailOutboxCipher(cipher)
	env.server.ConfigureEmailOutbox(repository)

	response := performJSON(env.router, http.MethodPost, "/v1/auth/signup", map[string]any{
		"email": "queued-signup@example.com",
	}, "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("signup status = %d: %s", response.Code, response.Body.String())
	}
	if len(env.tokenSink.deliveries) != 0 {
		t.Fatalf("synchronous sink unexpectedly called: %+v", env.tokenSink.deliveries)
	}
	var tokenCount, outboxCount int
	if err := env.db.QueryRow(context.Background(), `
		SELECT
			(SELECT count(*) FROM auth_tokens WHERE purpose = 'email_verification'),
			(SELECT count(*) FROM email_outbox WHERE message_type = 'email_verification')
	`).Scan(&tokenCount, &outboxCount); err != nil {
		t.Fatal(err)
	}
	if tokenCount != 1 || outboxCount != 1 {
		t.Fatalf("token=%d outbox=%d, want 1/1", tokenCount, outboxCount)
	}
}

func TestIntegrationPlatformAdminCreatesAndActivatesBrandOwnerByEmail(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()
	admin := registerUser(t, env.router, "load-owner-admin@example.com", "Load Owner Admin")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	repository := env.server.store.(*store.Store)
	key := make([]byte, 32)
	for i := range key {
		key[i] = 9
	}
	cipher, err := emaildelivery.NewCipher(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	repository.ConfigureEmailOutboxCipher(cipher)
	env.server.ConfigureEmailOutbox(repository)
	brandRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds", map[string]any{
		"name": "RTK-LOAD-CANARY-run-b01", "tenant_slug": "load-run-b01",
	}, admin.Tokens.AccessToken)
	if brandRes.Code != http.StatusCreated {
		t.Fatalf("brand create status = %d: %s", brandRes.Code, brandRes.Body.String())
	}
	brand := decodeBody[brandCloudBody](t, brandRes)
	createRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users", map[string]any{
		"email": "imap-test01+load-run-b01@realtekconnect.com", "display_name": "Load Owner",
		"role": "owner", "activation_mode": "email",
	}, admin.Tokens.AccessToken)
	if createRes.Code != http.StatusCreated {
		t.Fatalf("email owner create status = %d: %s", createRes.Code, createRes.Body.String())
	}
	created := decodeBody[brandCloudUserBody](t, createRes)
	if created.BrandCloudUser.EmailVerified || !created.BrandCloudUser.SignupPendingVerification {
		t.Fatalf("email owner did not start pending: %+v", created)
	}
	duplicateRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users", map[string]any{
		"email": "imap-test01+load-run-b01@realtekconnect.com", "display_name": "Load Owner",
		"role": "owner", "activation_mode": "email",
	}, admin.Tokens.AccessToken)
	if duplicateRes.Code != http.StatusConflict {
		t.Fatalf("duplicate email activation status = %d, want 409", duplicateRes.Code)
	}
	beforeLogin := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/load-run-b01/auth/login", map[string]any{
		"email": "imap-test01+load-run-b01@realtekconnect.com", "password": "not-activated",
	}, "")
	if beforeLogin.Code != http.StatusUnauthorized {
		t.Fatalf("pending owner login status = %d, want 401", beforeLogin.Code)
	}
	var nonce, ciphertext []byte
	if err := env.db.QueryRow(ctx, `
		SELECT payload_nonce, payload_ciphertext
		FROM email_outbox
		WHERE message_type = 'brand_cloud_user_activation'
	`).Scan(&nonce, &ciphertext); err != nil {
		t.Fatal(err)
	}
	payload, err := cipher.Decrypt(nonce, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if payload.TenantSlug != "load-run-b01" || payload.RecipientEmail != "imap-test01+load-run-b01@realtekconnect.com" || payload.Token == "" {
		t.Fatalf("activation outbox payload = %+v", payload)
	}
	wrongTenant := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/wrong-tenant/auth/activate", map[string]any{
		"token": payload.Token, "password": "activated-password123",
	}, "")
	if wrongTenant.Code != http.StatusBadRequest {
		t.Fatalf("wrong tenant activation status = %d, want 400", wrongTenant.Code)
	}
	activateRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/load-run-b01/auth/activate", map[string]any{
		"token": payload.Token, "password": "activated-password123",
	}, "")
	if activateRes.Code != http.StatusOK {
		t.Fatalf("owner activation status = %d: %s", activateRes.Code, activateRes.Body.String())
	}
	activated := decodeBody[tokenBody](t, activateRes)
	if !activated.User.EmailVerified || activated.User.SignupPendingVerification || activated.Tokens.AccessToken == "" {
		t.Fatalf("activated owner response = %+v", activated)
	}
	replay := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/load-run-b01/auth/activate", map[string]any{
		"token": payload.Token, "password": "another-password123",
	}, "")
	if replay.Code != http.StatusBadRequest || strings.Contains(replay.Body.String(), payload.Token) {
		t.Fatalf("replay status/body = %d/%s", replay.Code, replay.Body.String())
	}
	passwordLogin := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/load-run-b01/auth/login", map[string]any{
		"email": "imap-test01+load-run-b01@realtekconnect.com", "password": "activated-password123",
	}, "")
	if passwordLogin.Code != http.StatusOK {
		t.Fatalf("activated owner password login status = %d: %s", passwordLogin.Code, passwordLogin.Body.String())
	}

	expiredCreate := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users", map[string]any{
		"email": "imap-test01+load-run-b02@realtekconnect.com", "display_name": "Expired Load Owner",
		"role": "owner", "activation_mode": "email",
	}, admin.Tokens.AccessToken)
	if expiredCreate.Code != http.StatusCreated {
		t.Fatalf("expired owner create status = %d: %s", expiredCreate.Code, expiredCreate.Body.String())
	}
	if err := env.db.QueryRow(ctx, `
		SELECT payload_nonce, payload_ciphertext
		FROM email_outbox
		WHERE message_type = 'brand_cloud_user_activation'
		ORDER BY created_at DESC
		LIMIT 1
	`).Scan(&nonce, &ciphertext); err != nil {
		t.Fatal(err)
	}
	expiredPayload, err := cipher.Decrypt(nonce, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE auth_tokens SET expires_at = now() - interval '1 minute' WHERE token_hash = $1`, auth.HashToken(expiredPayload.Token)); err != nil {
		t.Fatal(err)
	}
	expiredActivate := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/load-run-b01/auth/activate", map[string]any{
		"token": expiredPayload.Token, "password": "expired-password123",
	}, "")
	if expiredActivate.Code != http.StatusBadRequest {
		t.Fatalf("expired activation status = %d, want 400", expiredActivate.Code)
	}
}

func TestIntegrationOutboxQueuesEveryPlatformAuthEmail(t *testing.T) {
	env := newIntegrationEnv(t)
	registered := registerUser(t, env.router, "queued-auth@example.com", "Queued Auth Org")
	repository, ok := env.server.store.(*store.Store)
	if !ok {
		t.Fatal("integration store is not the concrete store")
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = 5
	}
	cipher, err := emaildelivery.NewCipher(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	repository.ConfigureEmailOutboxCipher(cipher)
	env.server.ConfigureEmailOutbox(repository)
	synchronousDeliveries := len(env.tokenSink.deliveries)

	for _, request := range []struct {
		path string
	}{
		{path: "/v1/auth/resend-verification"},
		{path: "/v1/auth/sign-in"},
		{path: "/v1/auth/forgot-password"},
	} {
		response := performJSON(env.router, http.MethodPost, request.path, map[string]any{"email": registered.User.Email}, "")
		if response.Code != http.StatusAccepted {
			t.Fatalf("%s status = %d: %s", request.path, response.Code, response.Body.String())
		}
	}
	if len(env.tokenSink.deliveries) != synchronousDeliveries {
		t.Fatalf("synchronous sink received outbox deliveries: before=%d after=%d", synchronousDeliveries, len(env.tokenSink.deliveries))
	}

	rows, err := env.db.Query(context.Background(), `
		SELECT message_type, count(*), bool_and(payload_nonce IS NOT NULL AND payload_ciphertext IS NOT NULL)
		FROM email_outbox
		GROUP BY message_type
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var messageType string
		var count int
		var encrypted bool
		if err := rows.Scan(&messageType, &count, &encrypted); err != nil {
			t.Fatal(err)
		}
		if !encrypted {
			t.Fatalf("%s outbox payload is not encrypted", messageType)
		}
		got[messageType] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, messageType := range []string{"email_verification", "login_activation", "password_reset"} {
		if got[messageType] != 1 {
			t.Fatalf("%s outbox count = %d, want 1; all=%v", messageType, got[messageType], got)
		}
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

func TestIntegrationLoginAppCertificateCSRRequired(t *testing.T) {
	env := newIntegrationEnv(t)
	registerUser(t, env.router, "app-cert-required@example.com", "App Cert Required Org")

	loginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "app-cert-required@example.com",
		"password": "password123",
	}, "")
	if loginRes.Code != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", loginRes.Code, loginRes.Body.String())
	}
	body := decodeBody[tokenBody](t, loginRes)
	if body.AppCertificate.Status != "csr_required" {
		t.Fatalf("app certificate status = %q", body.AppCertificate.Status)
	}
	if body.AppCertificate.CertificatePEM != "" {
		t.Fatal("csr_required response must not include certificate material")
	}
}

func TestIntegrationLoginAppCertificateRejectsUnavailableIssuerAndInvalidCSR(t *testing.T) {
	env := newIntegrationEnv(t)
	registered := registerUser(t, env.router, "app-cert-invalid@example.com", "App Cert Invalid Org")

	csrRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":       "app-cert-invalid@example.com",
		"password":    "password123",
		"app_csr_pem": generateTestCSR(t, "app-user:"+registered.User.ID),
	}, "")
	assertErrorCode(t, csrRes, http.StatusServiceUnavailable, "app_certificate_issuer_unavailable")

	env.server.ConfigureAppCertificateIssuer(&fakeAppCertificateIssuer{})
	wrongSubjectRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":       "app-cert-invalid@example.com",
		"password":    "password123",
		"app_csr_pem": generateTestCSR(t, "app-user:someone-else"),
	}, "")
	assertErrorCode(t, wrongSubjectRes, http.StatusBadRequest, "app_certificate_csr_invalid")
}

func TestIntegrationLoginWithAppCSRStoresCertificateAndReusesIt(t *testing.T) {
	env := newIntegrationEnv(t)
	registered := registerUser(t, env.router, "app-cert-issued@example.com", "App Cert Issued Org")
	issuer := &fakeAppCertificateIssuer{}
	env.server.ConfigureAppCertificateIssuer(issuer)

	subject := "app-user:" + registered.User.ID
	csrPEM := generateTestCSR(t, subject)
	loginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":       "app-cert-issued@example.com",
		"password":    "password123",
		"app_csr_pem": csrPEM,
	}, "")
	if loginRes.Code != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", loginRes.Code, loginRes.Body.String())
	}
	body := decodeBody[tokenBody](t, loginRes)
	if body.AppCertificate.Status != "issued" || body.AppCertificate.Subject != subject {
		t.Fatalf("app certificate response = %+v", body.AppCertificate)
	}
	if body.AppCertificate.CertificatePEM == "" || body.AppCertificate.FingerprintSHA256 == "" {
		t.Fatalf("missing certificate material: %+v", body.AppCertificate)
	}
	if len(issuer.calls) != 1 {
		t.Fatalf("issuer calls = %d", len(issuer.calls))
	}
	if issuer.calls[0].UserID != registered.User.ID || strings.TrimSpace(issuer.calls[0].CSRPem) == "" {
		t.Fatalf("issuer request = %+v", issuer.calls[0])
	}

	reuseRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "app-cert-issued@example.com",
		"password": "password123",
	}, "")
	if reuseRes.Code != http.StatusOK {
		t.Fatalf("expected reuse login 200, got %d: %s", reuseRes.Code, reuseRes.Body.String())
	}
	reuseBody := decodeBody[tokenBody](t, reuseRes)
	if reuseBody.AppCertificate.FingerprintSHA256 != body.AppCertificate.FingerprintSHA256 {
		t.Fatalf("reuse fingerprint = %q, want %q", reuseBody.AppCertificate.FingerprintSHA256, body.AppCertificate.FingerprintSHA256)
	}
	if len(issuer.calls) != 1 {
		t.Fatalf("issuer should not be called for valid existing cert, calls = %d", len(issuer.calls))
	}

	if _, err := env.db.Exec(context.Background(), `
		UPDATE app_certificates
		SET not_after = now() - interval '1 hour'
		WHERE user_id = $1
	`, registered.User.ID); err != nil {
		t.Fatal(err)
	}
	expiredCSR := generateTestCSR(t, subject)
	expiredRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":       "app-cert-issued@example.com",
		"password":    "password123",
		"app_csr_pem": expiredCSR,
	}, "")
	if expiredRes.Code != http.StatusOK {
		t.Fatalf("expected expired cert reissue login 200, got %d: %s", expiredRes.Code, expiredRes.Body.String())
	}
	expiredBody := decodeBody[tokenBody](t, expiredRes)
	if expiredBody.AppCertificate.FingerprintSHA256 == body.AppCertificate.FingerprintSHA256 {
		t.Fatal("expired cert reissue reused the old certificate fingerprint")
	}
	if len(issuer.calls) != 2 {
		t.Fatalf("issuer should be called for expired cert reissue, calls = %d", len(issuer.calls))
	}

	if _, err := env.db.Exec(context.Background(), `
		UPDATE app_certificates
		SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL
	`, registered.User.ID); err != nil {
		t.Fatal(err)
	}
	revokedCSR := generateTestCSR(t, subject)
	revokedRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":       "app-cert-issued@example.com",
		"password":    "password123",
		"app_csr_pem": revokedCSR,
	}, "")
	if revokedRes.Code != http.StatusOK {
		t.Fatalf("expected revoked cert reissue login 200, got %d: %s", revokedRes.Code, revokedRes.Body.String())
	}
	revokedBody := decodeBody[tokenBody](t, revokedRes)
	if revokedBody.AppCertificate.FingerprintSHA256 == expiredBody.AppCertificate.FingerprintSHA256 {
		t.Fatal("revoked cert reissue reused the revoked certificate fingerprint")
	}
	if len(issuer.calls) != 3 {
		t.Fatalf("issuer should be called for revoked cert reissue, calls = %d", len(issuer.calls))
	}
}

func TestIntegrationInternalAppTokenAuthorization(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	unconfiguredRes := performJSON(env.router, http.MethodPost, "/v1/internal/app-token-authorizations", map[string]any{
		"user_id": "00000000-0000-0000-0000-000000000000",
		"devid":   "video-app-authz-1",
	}, "internal-authz-token")
	if unconfiguredRes.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected unconfigured internal token 503, got %d: %s", unconfiguredRes.Code, unconfiguredRes.Body.String())
	}
	env.server.ConfigureInternalAuthToken("internal-authz-token")
	owner := registerUser(t, env.router, "app-authz-owner@example.com", "App Authz Owner Org")
	outsider := registerUser(t, env.router, "app-authz-outsider@example.com", "App Authz Outsider Org")
	admin := registerUser(t, env.router, "app-authz-platform-admin@example.com", "App Authz Platform Admin Org")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}

	createRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", map[string]any{
		"name":          "app-authz-camera",
		"category":      "ip_camera",
		"serial_number": "APP-AUTHZ-001",
		"metadata": map[string]any{
			model.DeviceMetadataVideoCloudDevid: "video-app-authz-1",
		},
	}, owner.Tokens.AccessToken)
	if createRes.Code != http.StatusCreated {
		t.Fatalf("expected device create 201, got %d: %s", createRes.Code, createRes.Body.String())
	}

	allowedRes := performJSON(env.router, http.MethodPost, "/v1/internal/app-token-authorizations", map[string]any{
		"user_id": owner.User.ID,
		"devid":   "video-app-authz-1",
	}, "internal-authz-token")
	if allowedRes.Code != http.StatusOK {
		t.Fatalf("expected owner authorization 200, got %d: %s", allowedRes.Code, allowedRes.Body.String())
	}
	repository, ok := env.server.store.(*store.Store)
	if !ok {
		t.Fatal("integration store is not the concrete store")
	}
	now := time.Now().UTC()
	for index, fingerprint := range []string{"app-authz-fingerprint-old", "app-authz-fingerprint-current"} {
		if _, err := repository.CreateAppCertificate(ctx, store.AppCertificateCreateInput{
			UserID: owner.User.ID, SubjectType: "platform_user", SubjectID: owner.User.ID,
			Subject: "app-user:" + owner.User.ID, CSRSHA256: fmt.Sprintf("app-authz-csr-%d", index),
			CertificatePEM: fmt.Sprintf("app-authz-cert-%d", index), CertificateChainPEM: "app-authz-chain",
			FingerprintSHA256: fingerprint, SerialNumber: fmt.Sprintf("app-authz-serial-%d", index),
			IssuerRequestID: fmt.Sprintf("app-authz-request-%d", index), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		fingerprintRes := performJSON(env.router, http.MethodPost, "/v1/internal/app-token-authorizations", map[string]any{
			"user_id": owner.User.ID, "devid": "video-app-authz-1", "certificate_fingerprint_sha256": fingerprint,
		}, "internal-authz-token")
		if fingerprintRes.Code != http.StatusOK {
			t.Fatalf("expected active certificate authorization 200, got %d: %s", fingerprintRes.Code, fingerprintRes.Body.String())
		}
	}
	revokedFingerprintRes := performJSON(env.router, http.MethodPost, "/v1/internal/app-token-authorizations", map[string]any{
		"user_id": owner.User.ID, "devid": "video-app-authz-1", "certificate_fingerprint_sha256": "app-authz-fingerprint-old",
	}, "internal-authz-token")
	if revokedFingerprintRes.Code != http.StatusForbidden {
		t.Fatalf("expected revoked certificate authorization 403, got %d: %s", revokedFingerprintRes.Code, revokedFingerprintRes.Body.String())
	}

	brand := createBrandCloudForTest(t, env, admin.Tokens.AccessToken, "App Authz Brand", "app-authz-brand")
	brandUserRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users", map[string]any{
		"email":    "app-authz-brand@example.com",
		"password": "brand-password123",
		"role":     "admin",
	}, admin.Tokens.AccessToken)
	if brandUserRes.Code != http.StatusCreated {
		t.Fatalf("expected brand cloud user create 201, got %d: %s", brandUserRes.Code, brandUserRes.Body.String())
	}
	brandUser := decodeBody[brandCloudUserBody](t, brandUserRes)
	brandLoginRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/app-authz-brand/auth/login", map[string]any{
		"email":    "app-authz-brand@example.com",
		"password": "brand-password123",
	}, "")
	if brandLoginRes.Code != http.StatusOK {
		t.Fatalf("expected brand login 200, got %d: %s", brandLoginRes.Code, brandLoginRes.Body.String())
	}
	brandLogin := decodeBody[brandCloudLoginBody](t, brandLoginRes)
	brandDeviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+brand.BrandCloud.ID+"/devices", map[string]any{
		"name":     "app-authz-brand-camera",
		"category": "ip_camera",
		"metadata": map[string]any{
			model.DeviceMetadataVideoCloudDevid: "video-app-authz-brand-1",
		},
	}, brandLogin.Tokens.AccessToken)
	if brandDeviceRes.Code != http.StatusCreated {
		t.Fatalf("expected brand device create 201, got %d: %s", brandDeviceRes.Code, brandDeviceRes.Body.String())
	}
	brandAllowedRes := performJSON(env.router, http.MethodPost, "/v1/internal/app-token-authorizations", map[string]any{
		"subject_type":        "brand_cloud_user",
		"brand_cloud_user_id": brandUser.BrandCloudUser.ID,
		"devid":               "video-app-authz-brand-1",
	}, "internal-authz-token")
	if brandAllowedRes.Code != http.StatusOK {
		t.Fatalf("expected brand-cloud user authorization 200, got %d: %s", brandAllowedRes.Code, brandAllowedRes.Body.String())
	}
	brandMissingRes := performJSON(env.router, http.MethodPost, "/v1/internal/app-token-authorizations", map[string]any{
		"subject_type":        "brand_cloud_user",
		"brand_cloud_user_id": brandUser.BrandCloudUser.ID,
		"devid":               "missing-brand-video-device",
	}, "internal-authz-token")
	if brandMissingRes.Code != http.StatusForbidden {
		t.Fatalf("expected missing brand-cloud device authorization 403, got %d: %s", brandMissingRes.Code, brandMissingRes.Body.String())
	}

	outsiderRes := performJSON(env.router, http.MethodPost, "/v1/internal/app-token-authorizations", map[string]any{
		"user_id": outsider.User.ID,
		"devid":   "video-app-authz-1",
	}, "internal-authz-token")
	if outsiderRes.Code != http.StatusForbidden {
		t.Fatalf("expected outsider authorization 403, got %d: %s", outsiderRes.Code, outsiderRes.Body.String())
	}

	missingDeviceRes := performJSON(env.router, http.MethodPost, "/v1/internal/app-token-authorizations", map[string]any{
		"user_id": owner.User.ID,
		"devid":   "missing-video-device",
	}, "internal-authz-token")
	if missingDeviceRes.Code != http.StatusForbidden {
		t.Fatalf("expected missing device authorization 403, got %d: %s", missingDeviceRes.Code, missingDeviceRes.Body.String())
	}

	wrongTokenRes := performJSON(env.router, http.MethodPost, "/v1/internal/app-token-authorizations", map[string]any{
		"user_id": owner.User.ID,
		"devid":   "video-app-authz-1",
	}, "wrong-token")
	if wrongTokenRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected wrong internal token 401, got %d: %s", wrongTokenRes.Code, wrongTokenRes.Body.String())
	}
	missingUserRes := performJSON(env.router, http.MethodPost, "/v1/internal/app-token-authorizations", map[string]any{
		"devid": "video-app-authz-1",
	}, "internal-authz-token")
	if missingUserRes.Code != http.StatusBadRequest {
		t.Fatalf("expected missing user_id 400, got %d: %s", missingUserRes.Code, missingUserRes.Body.String())
	}
	unsupportedSubjectRes := performJSON(env.router, http.MethodPost, "/v1/internal/app-token-authorizations", map[string]any{
		"subject_type": "service_account",
		"devid":        "video-app-authz-1",
	}, "internal-authz-token")
	if unsupportedSubjectRes.Code != http.StatusBadRequest {
		t.Fatalf("expected unsupported subject type 400, got %d: %s", unsupportedSubjectRes.Code, unsupportedSubjectRes.Body.String())
	}
	missingEndUserRes := performJSON(env.router, http.MethodPost, "/v1/internal/app-token-authorizations", map[string]any{
		"subject_type": "end_user",
		"devid":        "video-app-authz-1",
	}, "internal-authz-token")
	if missingEndUserRes.Code != http.StatusBadRequest {
		t.Fatalf("expected missing end_user_id 400, got %d: %s", missingEndUserRes.Code, missingEndUserRes.Body.String())
	}
}

func TestIntegrationInternalDeviceProvisioningResult(t *testing.T) {
	env := newIntegrationEnv(t)
	env.server.ConfigureInternalAuthToken("internal-provision-token")
	owner := registerUser(t, env.router, "internal-provision-owner@example.com", "Internal Provision Org")

	deviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("internal-provision-device", "INTERNAL-PROVISION-001"), owner.Tokens.AccessToken)
	if deviceRes.Code != http.StatusCreated {
		t.Fatalf("expected device create 201, got %d: %s", deviceRes.Code, deviceRes.Body.String())
	}
	device := decodeBody[deviceBody](t, deviceRes)

	provisionRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provision", map[string]any{
		"video_cloud_devid": "video-internal-provision-1",
		"activity_id":       "activity-internal-provision-1",
		"clip_public_key":   "clip-internal-provision-1",
		"operation_id":      "internal-provision-op-1",
	}, owner.Tokens.AccessToken)
	if provisionRes.Code != http.StatusCreated {
		t.Fatalf("expected provision 201, got %d: %s", provisionRes.Code, provisionRes.Body.String())
	}

	activatedAt := time.Now().UTC().Truncate(time.Second)
	resultRes := performJSON(env.router, http.MethodPost, "/v1/internal/device-provisioning-results", map[string]any{
		"operation_id":      "internal-provision-op-1",
		"org_id":            owner.Organization.ID,
		"account_device_id": device.Device.ID,
		"video_cloud_devid": "video-internal-provision-1",
		"activity_id":       "activity-internal-provision-1",
		"activated_at":      activatedAt.Format(time.RFC3339),
	}, "internal-provision-token")
	if resultRes.Code != http.StatusOK {
		t.Fatalf("expected internal provisioning result 200, got %d: %s", resultRes.Code, resultRes.Body.String())
	}

	stateRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provisioning", nil, owner.Tokens.AccessToken)
	if stateRes.Code != http.StatusOK {
		t.Fatalf("expected provisioning state 200, got %d: %s", stateRes.Code, stateRes.Body.String())
	}
	state := decodeBody[provisioningBody](t, stateRes)
	if state.Operation == nil || state.Operation.Status != model.DeviceOperationStatusSucceeded {
		t.Fatalf("expected succeeded operation, got %+v", state.Operation)
	}
	if state.Readiness.State != model.DeviceReadinessStateTransportPending || state.Readiness.ProductState != model.ProductReadinessStateActivated {
		t.Fatalf("expected transport-pending activated state, got %+v", state.Readiness)
	}
	if got := state.VideoMetadata[model.DeviceMetadataVideoCloudActivationStatus]; got != string(model.VideoCloudActivationStatusActivated) {
		t.Fatalf("expected activated metadata, got %+v", state.VideoMetadata)
	}

	unauthorizedRes := performJSON(env.router, http.MethodPost, "/v1/internal/device-provisioning-results", map[string]any{
		"operation_id":      "internal-provision-op-1",
		"org_id":            owner.Organization.ID,
		"account_device_id": device.Device.ID,
		"video_cloud_devid": "video-internal-provision-1",
		"activity_id":       "activity-internal-provision-1",
	}, "wrong-token")
	if unauthorizedRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected wrong token 401, got %d: %s", unauthorizedRes.Code, unauthorizedRes.Body.String())
	}

	mismatchRes := performJSON(env.router, http.MethodPost, "/v1/internal/device-provisioning-results", map[string]any{
		"operation_id":      "internal-provision-op-1",
		"org_id":            owner.Organization.ID,
		"account_device_id": "00000000-0000-0000-0000-000000000000",
		"video_cloud_devid": "video-internal-provision-1",
		"activity_id":       "activity-internal-provision-1",
	}, "internal-provision-token")
	if mismatchRes.Code != http.StatusConflict {
		t.Fatalf("expected mismatch 409, got %d: %s", mismatchRes.Code, mismatchRes.Body.String())
	}

	missingOperationRes := performJSON(env.router, http.MethodPost, "/v1/internal/device-provisioning-results", map[string]any{
		"operation_id":      "missing-internal-provision-op",
		"org_id":            owner.Organization.ID,
		"account_device_id": device.Device.ID,
		"video_cloud_devid": "video-internal-provision-1",
		"activity_id":       "activity-internal-provision-1",
	}, "internal-provision-token")
	if missingOperationRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing operation 404, got %d: %s", missingOperationRes.Code, missingOperationRes.Body.String())
	}

	invalidRes := performJSON(env.router, http.MethodPost, "/v1/internal/device-provisioning-results", map[string]any{
		"operation_id":      "internal-provision-op-1",
		"org_id":            owner.Organization.ID,
		"account_device_id": device.Device.ID,
		"video_cloud_devid": "video-internal-provision-1",
	}, "internal-provision-token")
	if invalidRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid payload 400, got %d: %s", invalidRes.Code, invalidRes.Body.String())
	}
}

func TestIntegrationOIDCProviderLoginAndCallback(t *testing.T) {
	env := newIntegrationEnv(t)
	fake := newAPIOIDCTestServer(t)
	defer fake.close()
	configureOIDCTestServer(t, env.server, fake, true)

	registered := registerUser(t, env.router, "oidc-user@example.com", "OIDC Org")
	targetOrg := registerUser(t, env.router, "oidc-target@example.com", "OIDC Target Org")
	targetOrgID := targetOrg.Organization.ID
	if _, err := store.New(env.db).CreateExternalGroupMapping(context.Background(), store.ExternalGroupMappingCreateInput{
		ProviderID:     "keycloak",
		ExternalGroup:  "/installers",
		RoleName:       "installer",
		ScopeType:      store.ScopeTypeOrganization,
		OrganizationID: &targetOrgID,
		Now:            time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	providersRes := performJSON(env.router, http.MethodGet, "/v1/auth/oidc/providers", nil, "")
	if providersRes.Code != http.StatusOK {
		t.Fatalf("expected OIDC providers 200, got %d: %s", providersRes.Code, providersRes.Body.String())
	}
	var providersBody struct {
		Providers []struct {
			ProviderID string   `json:"provider_id"`
			Name       string   `json:"name"`
			Type       string   `json:"type"`
			IssuerURL  string   `json:"issuer_url"`
			Scopes     []string `json:"scopes"`
			Enabled    bool     `json:"enabled"`
		} `json:"providers"`
	}
	providersBody = decodeBody[struct {
		Providers []struct {
			ProviderID string   `json:"provider_id"`
			Name       string   `json:"name"`
			Type       string   `json:"type"`
			IssuerURL  string   `json:"issuer_url"`
			Scopes     []string `json:"scopes"`
			Enabled    bool     `json:"enabled"`
		} `json:"providers"`
	}](t, providersRes)
	if len(providersBody.Providers) != 1 || providersBody.Providers[0].ProviderID != "keycloak" || !providersBody.Providers[0].Enabled {
		t.Fatalf("unexpected providers response: %+v", providersBody)
	}

	loginRes := performJSON(env.router, http.MethodGet, "/v1/auth/oidc/keycloak/login", nil, "")
	if loginRes.Code != http.StatusFound {
		t.Fatalf("expected OIDC login redirect, got %d: %s", loginRes.Code, loginRes.Body.String())
	}
	location := loginRes.Header().Get("Location")
	authURL, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	state := authURL.Query().Get("state")
	nonce := authURL.Query().Get("nonce")
	if state == "" || nonce == "" || authURL.Path != "/authorize" {
		t.Fatalf("unexpected authorization redirect: %s", location)
	}
	assertOIDCStateStoredAsHash(t, env.db, state, nonce)

	fake.idToken = fake.signToken(t, apiOIDCTokenFixture{
		Subject:       "subject-1",
		Email:         "OIDC-User@Example.com",
		EmailVerified: true,
		Nonce:         nonce,
		Groups:        []string{"/installers", "/unmapped"},
	})
	callbackRes := performJSON(env.router, http.MethodGet, "/v1/auth/oidc/keycloak/callback?code=auth-code&state="+url.QueryEscape(state), nil, "")
	if callbackRes.Code != http.StatusOK {
		t.Fatalf("expected OIDC callback 200, got %d: %s", callbackRes.Code, callbackRes.Body.String())
	}
	callbackBody := decodeBody[struct {
		User struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
	}](t, callbackRes)
	if callbackBody.User.ID != registered.User.ID || callbackBody.User.Email != "oidc-user@example.com" || callbackBody.Tokens.AccessToken == "" || callbackBody.Tokens.RefreshToken == "" {
		t.Fatalf("unexpected callback body: %+v", callbackBody)
	}

	var identityCount int
	if err := env.db.QueryRow(context.Background(), `
		SELECT count(*) FROM user_identities WHERE user_id = $1 AND subject = 'subject-1'
	`, registered.User.ID).Scan(&identityCount); err != nil {
		t.Fatal(err)
	}
	if identityCount != 1 {
		t.Fatalf("expected exactly one linked identity, got %d", identityCount)
	}
	canClaimMappedOrg, err := store.New(env.db).HasPermission(context.Background(), registered.User.ID, targetOrg.Organization.ID, "claim.resolve")
	if err != nil {
		t.Fatal(err)
	}
	if !canClaimMappedOrg {
		t.Fatal("expected mapped OIDC group to grant scoped installer permission")
	}
	canManageMappedOrg, err := store.New(env.db).HasPermission(context.Background(), registered.User.ID, targetOrg.Organization.ID, "registry_device.manage")
	if err != nil {
		t.Fatal(err)
	}
	if canManageMappedOrg {
		t.Fatal("mapped installer group must not grant unmapped registry manage permission")
	}

	replayRes := performJSON(env.router, http.MethodGet, "/v1/auth/oidc/keycloak/callback?code=auth-code&state="+url.QueryEscape(state), nil, "")
	assertErrorCode(t, replayRes, http.StatusBadRequest, "invalid_oidc_state")

	localLoginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "oidc-user@example.com",
		"password": "password123",
	}, "")
	if localLoginRes.Code != http.StatusOK {
		t.Fatalf("expected local login to remain available, got %d: %s", localLoginRes.Code, localLoginRes.Body.String())
	}
}

func TestIntegrationOIDCCallbackRejectsUnknownDisabledAndUnverifiedUsers(t *testing.T) {
	t.Run("unknown without auto link", func(t *testing.T) {
		env := newIntegrationEnv(t)
		fake := newAPIOIDCTestServer(t)
		defer fake.close()
		configureOIDCTestServer(t, env.server, fake, false)

		state, nonce := startOIDCTestLogin(t, env.router)
		fake.idToken = fake.signToken(t, apiOIDCTokenFixture{Subject: "unknown-subject", Email: "missing@example.com", EmailVerified: true, Nonce: nonce})
		res := performJSON(env.router, http.MethodGet, "/v1/auth/oidc/keycloak/callback?code=auth-code&state="+url.QueryEscape(state), nil, "")
		assertErrorCode(t, res, http.StatusForbidden, "user_not_provisioned")
	})

	t.Run("disabled linked user", func(t *testing.T) {
		env := newIntegrationEnv(t)
		fake := newAPIOIDCTestServer(t)
		defer fake.close()
		configureOIDCTestServer(t, env.server, fake, false)
		registered := registerUser(t, env.router, "disabled-oidc@example.com", "Disabled OIDC Org")
		provider := seedOIDCTestProvider(t, env.db, fake)
		if _, err := store.New(env.db).CreateUserIdentity(context.Background(), store.UserIdentityCreateInput{
			UserID:        registered.User.ID,
			ProviderID:    provider.ID,
			IssuerURL:     fake.server.URL,
			Subject:       "disabled-subject",
			Email:         "disabled-oidc@example.com",
			EmailVerified: true,
			Claims:        map[string]any{"sub": "disabled-subject"},
			Now:           time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(context.Background(), `UPDATE users SET disabled_at = now(), updated_at = now() WHERE id = $1`, registered.User.ID); err != nil {
			t.Fatal(err)
		}
		state, nonce := seedOIDCTestState(t, env.db, provider.ID)
		fake.idToken = fake.signToken(t, apiOIDCTokenFixture{Subject: "disabled-subject", Email: "disabled-oidc@example.com", EmailVerified: true, Nonce: nonce})
		res := performJSON(env.router, http.MethodGet, "/v1/auth/oidc/keycloak/callback?code=auth-code&state="+url.QueryEscape(state), nil, "")
		assertErrorCode(t, res, http.StatusForbidden, "user_not_provisioned")
	})

	t.Run("unverified email", func(t *testing.T) {
		env := newIntegrationEnv(t)
		fake := newAPIOIDCTestServer(t)
		defer fake.close()
		configureOIDCTestServer(t, env.server, fake, true)
		state, nonce := startOIDCTestLogin(t, env.router)
		fake.idToken = fake.signToken(t, apiOIDCTokenFixture{Subject: "subject-2", Email: "unverified@example.com", EmailVerified: false, Nonce: nonce})
		res := performJSON(env.router, http.MethodGet, "/v1/auth/oidc/keycloak/callback?code=auth-code&state="+url.QueryEscape(state), nil, "")
		assertErrorCode(t, res, http.StatusBadRequest, "unverified_oidc_email")
	})
}

func TestIntegrationOIDCDisabledDiscoveryAndLogin(t *testing.T) {
	env := newIntegrationEnv(t)

	providersRes := performJSON(env.router, http.MethodGet, "/v1/auth/oidc/providers", nil, "")
	if providersRes.Code != http.StatusOK {
		t.Fatalf("expected disabled discovery 200, got %d", providersRes.Code)
	}
	var providers struct {
		Providers []any `json:"providers"`
	}
	providers = decodeBody[struct {
		Providers []any `json:"providers"`
	}](t, providersRes)
	if len(providers.Providers) != 0 {
		t.Fatalf("expected no providers when disabled, got %+v", providers)
	}

	loginRes := performJSON(env.router, http.MethodGet, "/v1/auth/oidc/keycloak/login", nil, "")
	assertErrorCode(t, loginRes, http.StatusBadRequest, "oidc_disabled")
}

func TestIntegrationCurrentUserOIDCIdentityManagement(t *testing.T) {
	env := newIntegrationEnv(t)
	fake := newAPIOIDCTestServer(t)
	defer fake.close()
	configureOIDCTestServer(t, env.server, fake, true)
	owner := registerUser(t, env.router, "identity-owner@example.com", "Identity Owner Org")
	other := registerUser(t, env.router, "identity-other@example.com", "Identity Other Org")

	state, nonce := startOIDCTestLogin(t, env.router)
	fake.idToken = fake.signToken(t, apiOIDCTokenFixture{
		Subject:       "identity-subject",
		Email:         "identity-owner@example.com",
		EmailVerified: true,
		Nonce:         nonce,
	})
	callbackRes := performJSON(env.router, http.MethodGet, "/v1/auth/oidc/keycloak/callback?code=auth-code&state="+url.QueryEscape(state), nil, "")
	if callbackRes.Code != http.StatusOK {
		t.Fatalf("expected callback 200, got %d: %s", callbackRes.Code, callbackRes.Body.String())
	}
	callbackBody := decodeBody[tokenBody](t, callbackRes)

	listRes := performJSON(env.router, http.MethodGet, "/v1/me/identities", nil, callbackBody.Tokens.AccessToken)
	if listRes.Code != http.StatusOK {
		t.Fatalf("expected identities list 200, got %d: %s", listRes.Code, listRes.Body.String())
	}
	var listBody struct {
		Identities []model.UserIdentity `json:"identities"`
	}
	listBody = decodeBody[struct {
		Identities []model.UserIdentity `json:"identities"`
	}](t, listRes)
	if len(listBody.Identities) != 1 || listBody.Identities[0].Subject != "identity-subject" || listBody.Identities[0].Email != "identity-owner@example.com" {
		t.Fatalf("unexpected identities list: %+v", listBody)
	}
	identityID := listBody.Identities[0].ID

	otherDeleteRes := performJSON(env.router, http.MethodDelete, "/v1/me/identities/"+identityID, nil, other.Tokens.AccessToken)
	assertErrorCode(t, otherDeleteRes, http.StatusNotFound, "not_found")

	deleteRes := performJSON(env.router, http.MethodDelete, "/v1/me/identities/"+identityID, nil, callbackBody.Tokens.AccessToken)
	if deleteRes.Code != http.StatusNoContent {
		t.Fatalf("expected identity delete 204, got %d: %s", deleteRes.Code, deleteRes.Body.String())
	}

	listAfterDeleteRes := performJSON(env.router, http.MethodGet, "/v1/me/identities", nil, callbackBody.Tokens.AccessToken)
	if listAfterDeleteRes.Code != http.StatusOK {
		t.Fatalf("expected identities list after delete 200, got %d: %s", listAfterDeleteRes.Code, listAfterDeleteRes.Body.String())
	}
	listAfterDelete := decodeBody[struct {
		Identities []model.UserIdentity `json:"identities"`
	}](t, listAfterDeleteRes)
	if len(listAfterDelete.Identities) != 0 {
		t.Fatalf("expected no identities after delete, got %+v", listAfterDelete)
	}

	configureOIDCTestServer(t, env.server, fake, false)
	state, nonce = startOIDCTestLogin(t, env.router)
	fake.idToken = fake.signToken(t, apiOIDCTokenFixture{
		Subject:       "identity-subject",
		Email:         "identity-owner@example.com",
		EmailVerified: true,
		Nonce:         nonce,
	})
	unlinkedCallbackRes := performJSON(env.router, http.MethodGet, "/v1/auth/oidc/keycloak/callback?code=auth-code&state="+url.QueryEscape(state), nil, "")
	assertErrorCode(t, unlinkedCallbackRes, http.StatusForbidden, "user_not_provisioned")

	localLoginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    owner.User.Email,
		"password": "password123",
	}, "")
	if localLoginRes.Code != http.StatusOK {
		t.Fatalf("expected local password login to survive identity unlink, got %d: %s", localLoginRes.Code, localLoginRes.Body.String())
	}
}

func TestIntegrationDisabledUserCannotManageOIDCIdentities(t *testing.T) {
	env := newIntegrationEnv(t)
	fake := newAPIOIDCTestServer(t)
	defer fake.close()
	configureOIDCTestServer(t, env.server, fake, true)
	registered := registerUser(t, env.router, "disabled-identities@example.com", "Disabled Identities Org")
	provider := seedOIDCTestProvider(t, env.db, fake)
	if _, err := store.New(env.db).CreateUserIdentity(context.Background(), store.UserIdentityCreateInput{
		UserID:        registered.User.ID,
		ProviderID:    provider.ID,
		IssuerURL:     fake.server.URL,
		Subject:       "disabled-identities-subject",
		Email:         registered.User.Email,
		EmailVerified: true,
		Claims:        map[string]any{"sub": "disabled-identities-subject"},
		Now:           time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(context.Background(), `UPDATE users SET disabled_at = now(), updated_at = now() WHERE id = $1`, registered.User.ID); err != nil {
		t.Fatal(err)
	}
	listRes := performJSON(env.router, http.MethodGet, "/v1/me/identities", nil, registered.Tokens.AccessToken)
	if listRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled user identities list 401, got %d: %s", listRes.Code, listRes.Body.String())
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
		"token":        verificationToken,
		"new_password": "password123",
	}, "")
	if verifyRes.Code != http.StatusOK {
		t.Fatalf("expected verify email 200, got %d: %s", verifyRes.Code, verifyRes.Body.String())
	}
	verified := decodeBody[userBody](t, verifyRes)
	if !verified.User.EmailVerified || verified.User.EmailVerifiedAt == nil {
		t.Fatalf("expected verified user response, got %+v", verified.User)
	}
	if verified.Tokens.AccessToken == "" || verified.Tokens.RefreshToken == "" {
		t.Fatalf("expected email verification to issue initial tokens")
	}
	reuseVerifyRes := performJSON(env.router, http.MethodPost, "/v1/auth/verify-email", map[string]any{
		"token":        verificationToken,
		"new_password": "password123",
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
		"token":        resendToken,
		"new_password": "password123",
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
		"token":        expiredVerificationToken,
		"new_password": "password123",
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

	signInUnknownRes := performJSON(env.router, http.MethodPost, "/v1/auth/sign-in", map[string]any{
		"email": "unknown-signin@example.com",
	}, "")
	if signInUnknownRes.Code != http.StatusAccepted {
		t.Fatalf("expected unknown sign-in to stay enumeration-safe 202, got %d", signInUnknownRes.Code)
	}
	createdUnknownLogin, err := accountStore.CreateLoginActivationTokenForEmail(context.Background(), "missing-login@example.com", auth.HashToken("missing-login"), time.Now().Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if createdUnknownLogin {
		t.Fatal("expected unknown login email not to create a token")
	}
	signInRes := performJSON(env.router, http.MethodPost, "/v1/auth/sign-in", map[string]any{
		"email": "verify@example.com",
	}, "")
	if signInRes.Code != http.StatusAccepted {
		t.Fatalf("expected sign-in 202, got %d: %s", signInRes.Code, signInRes.Body.String())
	}
	loginActivationToken := latestAuthToken(t, env.tokenSink, "verify@example.com", "login_activation")
	activateLoginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login/activate", map[string]any{
		"token": loginActivationToken,
	}, "")
	if activateLoginRes.Code != http.StatusOK {
		t.Fatalf("expected login activation 200, got %d: %s", activateLoginRes.Code, activateLoginRes.Body.String())
	}
	activatedLogin := decodeBody[tokenBody](t, activateLoginRes)
	if activatedLogin.Tokens.AccessToken == "" || activatedLogin.Tokens.RefreshToken == "" {
		t.Fatalf("expected login activation tokens, got %+v", activatedLogin.Tokens)
	}
	replayLoginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login/activate", map[string]any{
		"token": loginActivationToken,
	}, "")
	if replayLoginRes.Code != http.StatusBadRequest {
		t.Fatalf("expected replayed login token 400, got %d", replayLoginRes.Code)
	}
	expiredLoginToken := "expired-login-token"
	if err := accountStore.CreateLoginActivationToken(context.Background(), registered.User.ID, auth.HashToken(expiredLoginToken), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	expiredLoginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login/activate", map[string]any{
		"token": expiredLoginToken,
	}, "")
	if expiredLoginRes.Code != http.StatusBadRequest {
		t.Fatalf("expected expired login token 400, got %d", expiredLoginRes.Code)
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
		"token":        disabledVerificationToken,
		"new_password": "password123",
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

func TestIntegrationEmailSignInValidationPaths(t *testing.T) {
	env := newIntegrationEnv(t)
	accountStore := store.New(env.db)
	ctx := context.Background()

	malformedSignInRes := performJSON(env.router, http.MethodPost, "/v1/auth/sign-in", "not-json", "")
	if malformedSignInRes.Code != http.StatusBadRequest {
		t.Fatalf("expected malformed sign-in 400, got %d: %s", malformedSignInRes.Code, malformedSignInRes.Body.String())
	}
	missingActivationTokenRes := performJSON(env.router, http.MethodPost, "/v1/auth/login/activate", map[string]any{
		"token": "   ",
	}, "")
	if missingActivationTokenRes.Code != http.StatusBadRequest {
		t.Fatalf("expected blank login activation token 400, got %d: %s", missingActivationTokenRes.Code, missingActivationTokenRes.Body.String())
	}
	invalidActivationTokenRes := performJSON(env.router, http.MethodPost, "/v1/auth/login/activate", map[string]any{
		"token": "not-a-valid-login-token",
	}, "")
	if invalidActivationTokenRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid login activation token 400, got %d: %s", invalidActivationTokenRes.Code, invalidActivationTokenRes.Body.String())
	}

	malformedBrandSignInRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/missing/auth/sign-in", "not-json", "")
	if malformedBrandSignInRes.Code != http.StatusBadRequest {
		t.Fatalf("expected malformed brand sign-in 400, got %d: %s", malformedBrandSignInRes.Code, malformedBrandSignInRes.Body.String())
	}
	invalidBrandActivationTokenRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/missing/auth/login/activate", map[string]any{
		"token": "not-a-valid-brand-token",
	}, "")
	if invalidBrandActivationTokenRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid brand activation token 400, got %d: %s", invalidBrandActivationTokenRes.Code, invalidBrandActivationTokenRes.Body.String())
	}

	rateLimited := registerUser(t, env.router, "signin-rate-limit@example.com", "Sign In Rate Limit Org")
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified = true, email_verified_at = now(), signup_pending_verification = false WHERE id = $1`, rateLimited.User.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := accountStore.CreateLoginActivationToken(ctx, rateLimited.User.ID, auth.HashToken("signin-rate-limit-"+strconv.Itoa(i)), time.Now().Add(30*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	rateLimitedSignInRes := performJSON(env.router, http.MethodPost, "/v1/auth/sign-in", map[string]any{
		"email": "signin-rate-limit@example.com",
	}, "")
	if rateLimitedSignInRes.Code != http.StatusAccepted {
		t.Fatalf("expected rate-limited sign-in to stay enumeration-safe 202, got %d", rateLimitedSignInRes.Code)
	}
}

func TestIntegrationSignupEvaluationQuotaAndRaiseWorkflow(t *testing.T) {
	env := newIntegrationEnv(t)

	registered := registerUser(t, env.router, "eval@example.com", "Eval Org")
	markEvaluationOrg(t, env, registered.Organization.ID, 5)
	orgID := registered.Organization.ID
	accessToken := registered.Tokens.AccessToken

	for i := 0; i < 5; i++ {
		res := performJSON(env.router, http.MethodPost, "/v1/orgs/"+orgID+"/devices", devicePayload("eval-device-"+strconv.Itoa(i), "EVAL-"+strconv.Itoa(i)), accessToken)
		if res.Code != http.StatusCreated {
			t.Fatalf("expected device %d create 201, got %d: %s", i, res.Code, res.Body.String())
		}
	}
	quotaExceededRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+orgID+"/devices", devicePayload("eval-device-5", "EVAL-5"), accessToken)
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

	raiseReqRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+orgID+"/quota-raise-requests", map[string]any{
		"requested_quota": 8,
		"use_case":        "pilot expansion",
		"contact_info": map[string]any{
			"email": "buyer@example.com",
		},
	}, accessToken)
	if raiseReqRes.Code != http.StatusCreated {
		t.Fatalf("expected quota raise request 201, got %d: %s", raiseReqRes.Code, raiseReqRes.Body.String())
	}
	raiseReqBody := decodeBody[quotaRaiseRequestBody](t, raiseReqRes)
	if raiseReqBody.QuotaRaiseRequest.Status != string(model.QuotaRaiseRequestStatusPending) {
		t.Fatalf("expected pending quota raise request, got %+v", raiseReqBody.QuotaRaiseRequest)
	}

	nonAdminApproveRes := performJSON(env.router, http.MethodPost, "/v1/admin/quota-raise-requests/"+raiseReqBody.QuotaRaiseRequest.ID+"/approve", map[string]any{
		"approved_quota": 500,
	}, accessToken)
	if nonAdminApproveRes.Code != http.StatusForbidden {
		t.Fatalf("expected non-admin approval attempt 403, got %d: %s", nonAdminApproveRes.Code, nonAdminApproveRes.Body.String())
	}

	admin := registerUser(t, env.router, "platform-admin@example.com", "Admin Org")
	if _, err := env.db.Exec(context.Background(), `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	nonAdminListRes := performJSON(env.router, http.MethodGet, "/v1/admin/quota-raise-requests", nil, accessToken)
	if nonAdminListRes.Code != http.StatusForbidden {
		t.Fatalf("expected non-admin quota list 403, got %d: %s", nonAdminListRes.Code, nonAdminListRes.Body.String())
	}
	pendingListRes := performJSON(env.router, http.MethodGet, "/v1/admin/quota-raise-requests?status=pending&limit=1&offset=0", nil, admin.Tokens.AccessToken)
	if pendingListRes.Code != http.StatusOK {
		t.Fatalf("expected pending quota list 200, got %d: %s", pendingListRes.Code, pendingListRes.Body.String())
	}
	pendingList := decodeBody[quotaRaiseRequestsBody](t, pendingListRes)
	if pendingList.Pagination.Total != 1 || len(pendingList.QuotaRaiseRequests) != 1 || pendingList.QuotaRaiseRequests[0].ID != raiseReqBody.QuotaRaiseRequest.ID {
		t.Fatalf("expected pending quota list to include request, got %+v", pendingList)
	}
	showReqRes := performJSON(env.router, http.MethodGet, "/v1/admin/quota-raise-requests/"+raiseReqBody.QuotaRaiseRequest.ID, nil, admin.Tokens.AccessToken)
	if showReqRes.Code != http.StatusOK {
		t.Fatalf("expected quota show 200, got %d: %s", showReqRes.Code, showReqRes.Body.String())
	}
	showReqBody := decodeBody[quotaRaiseRequestBody](t, showReqRes)
	if showReqBody.QuotaRaiseRequest.ID != raiseReqBody.QuotaRaiseRequest.ID {
		t.Fatalf("expected quota show to return request, got %+v", showReqBody)
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

	declineReqRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+orgID+"/quota-raise-requests", map[string]any{
		"requested_quota": 12,
		"use_case":        "contract exit",
		"contact_info": map[string]any{
			"email": "buyer@example.com",
		},
	}, accessToken)
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

	approvedListRes := performJSON(env.router, http.MethodGet, "/v1/admin/quota-raise-requests?status=approved", nil, admin.Tokens.AccessToken)
	if approvedListRes.Code != http.StatusOK {
		t.Fatalf("expected approved quota list 200, got %d: %s", approvedListRes.Code, approvedListRes.Body.String())
	}
	approvedList := decodeBody[quotaRaiseRequestsBody](t, approvedListRes)
	if approvedList.Pagination.Total != 1 || len(approvedList.QuotaRaiseRequests) != 1 || approvedList.QuotaRaiseRequests[0].Status != string(model.QuotaRaiseRequestStatusApproved) {
		t.Fatalf("expected one approved quota request, got %+v", approvedList)
	}
	invalidStatusRes := performJSON(env.router, http.MethodGet, "/v1/admin/quota-raise-requests?status=unknown", nil, admin.Tokens.AccessToken)
	if invalidStatusRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid status 400, got %d: %s", invalidStatusRes.Code, invalidStatusRes.Body.String())
	}

	auditListRes := performJSON(env.router, http.MethodGet, "/v1/admin/audit-events?event_type=quota_raise_approved", nil, admin.Tokens.AccessToken)
	if auditListRes.Code != http.StatusOK {
		t.Fatalf("expected audit list 200, got %d: %s", auditListRes.Code, auditListRes.Body.String())
	}
	auditList := decodeBody[auditEventsBody](t, auditListRes)
	if auditList.Pagination.Total != 1 || len(auditList.AuditEvents) != 1 || auditList.AuditEvents[0].EventType != "quota_raise_approved" {
		t.Fatalf("expected one quota_raise_approved audit event, got %+v", auditList)
	}
	auditSubjectListRes := performJSON(env.router, http.MethodGet, "/v1/admin/audit-events?subject_type=quota_raise_request&limit=2", nil, admin.Tokens.AccessToken)
	if auditSubjectListRes.Code != http.StatusOK {
		t.Fatalf("expected audit subject list 200, got %d: %s", auditSubjectListRes.Code, auditSubjectListRes.Body.String())
	}
	auditSubjectList := decodeBody[auditEventsBody](t, auditSubjectListRes)
	if auditSubjectList.Pagination.Total != 4 || len(auditSubjectList.AuditEvents) != 2 {
		t.Fatalf("expected paginated quota_raise_request audit events, got %+v", auditSubjectList)
	}

	metricsRes := performJSON(env.router, http.MethodGet, "/v1/admin/metrics", nil, admin.Tokens.AccessToken)
	if metricsRes.Code != http.StatusOK {
		t.Fatalf("expected admin metrics 200, got %d: %s", metricsRes.Code, metricsRes.Body.String())
	}
	metricsBody := decodeBody[evalTierMetricsBody](t, metricsRes)
	if metricsBody.Signups.EvaluationCreated != 0 || metricsBody.Signups.VerificationCompleted != 0 || metricsBody.Signups.VerificationCompletionRate != 0 {
		t.Fatalf("expected no legacy signup metrics in quota workflow, got %+v", metricsBody.Signups)
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

	postApprovalRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+orgID+"/devices", devicePayload("eval-device-5", "EVAL-5"), accessToken)
	if postApprovalRes.Code != http.StatusCreated {
		t.Fatalf("expected device create after approval 201, got %d: %s", postApprovalRes.Code, postApprovalRes.Body.String())
	}
}

func TestIntegrationDeveloperSignupUsesEmailAndRejectsLegacyFields(t *testing.T) {
	env := newIntegrationEnv(t)

	legacyRes := performJSON(env.router, http.MethodPost, "/v1/auth/signup", map[string]any{
		"email":    "legacy-developer@example.com",
		"password": "password123",
	}, "")
	if legacyRes.Code != http.StatusBadRequest {
		t.Fatalf("expected legacy signup fields to be rejected, got %d: %s", legacyRes.Code, legacyRes.Body.String())
	}

	fallbackRes := performJSON(env.router, http.MethodPost, "/v1/auth/signup", map[string]any{
		"email": "Fallback-Developer@Example.COM",
	}, "")
	if fallbackRes.Code != http.StatusAccepted {
		t.Fatalf("expected fallback developer signup 202, got %d: %s", fallbackRes.Code, fallbackRes.Body.String())
	}
	fallback := decodeBody[developerSignupBody](t, fallbackRes)
	if fallback.BrandCloud.Name != "fallback-developer@example.com" {
		t.Fatalf("expected normalized email Brand Cloud fallback, got %+v", fallback.BrandCloud)
	}
}

func TestIntegrationDeveloperSignupCreatesDefaultBrandCloudAndDeveloperCanCreateWithinLimit(t *testing.T) {
	env := newIntegrationEnv(t)

	signupRes := performJSON(env.router, http.MethodPost, "/v1/auth/signup", map[string]any{
		"email": "developer-owner@example.com",
	}, "")
	if signupRes.Code != http.StatusAccepted {
		t.Fatalf("expected developer signup 202, got %d: %s", signupRes.Code, signupRes.Body.String())
	}
	signup := decodeBody[developerSignupBody](t, signupRes)
	if signup.BrandCloud.ID == "" || signup.BrandCloud.Name != "developer-owner@example.com" || signup.BrandCloud.OrganizationKind != "brand_cloud" {
		t.Fatalf("expected default developer brand cloud, got %+v", signup.BrandCloud)
	}
	if signup.User.DeveloperCloudLimit != 8 || !signup.User.SignupPendingVerification {
		t.Fatalf("expected developer limit and pending verification, got %+v", signup.User)
	}

	duplicateSignupRes := performJSON(env.router, http.MethodPost, "/v1/auth/signup", map[string]any{
		"email": "developer-owner@example.com",
	}, "")
	if duplicateSignupRes.Code != http.StatusConflict {
		t.Fatalf("expected duplicate developer signup 409, got %d: %s", duplicateSignupRes.Code, duplicateSignupRes.Body.String())
	}

	expiredToken := latestAuthToken(t, env.tokenSink, "developer-owner@example.com", "email_verification")
	validStatusRes := performJSON(env.router, http.MethodPost, "/v1/auth/verify-email/status", map[string]any{"token": expiredToken}, "")
	if validStatusRes.Code != http.StatusOK || !strings.Contains(validStatusRes.Body.String(), `"status":"valid"`) {
		t.Fatalf("expected valid verification token status, got %d: %s", validStatusRes.Code, validStatusRes.Body.String())
	}
	if _, err := env.db.Exec(t.Context(), `UPDATE auth_tokens SET expires_at = now() - interval '1 minute' WHERE token_hash = $1`, auth.HashToken(expiredToken)); err != nil {
		t.Fatal(err)
	}
	expiredStatusRes := performJSON(env.router, http.MethodPost, "/v1/auth/verify-email/status", map[string]any{"token": expiredToken}, "")
	if expiredStatusRes.Code != http.StatusOK || !strings.Contains(expiredStatusRes.Body.String(), `"status":"expired"`) {
		t.Fatalf("expected expired verification token status, got %d: %s", expiredStatusRes.Code, expiredStatusRes.Body.String())
	}
	restartedSignupRes := performJSON(env.router, http.MethodPost, "/v1/auth/signup", map[string]any{
		"email": "developer-owner@example.com",
	}, "")
	if restartedSignupRes.Code != http.StatusAccepted {
		t.Fatalf("expected expired pending signup restart 202, got %d: %s", restartedSignupRes.Code, restartedSignupRes.Body.String())
	}
	restartedToken := latestAuthToken(t, env.tokenSink, "developer-owner@example.com", "email_verification")
	if restartedToken == expiredToken {
		t.Fatal("expected signup restart to issue a new verification token")
	}

	env.server.authTokenSink = failingAuthTokenSink{}
	deliveryFailureRes := performJSON(env.router, http.MethodPost, "/v1/auth/signup", map[string]any{
		"email": "delivery-failure-developer@example.com",
	}, "")
	if deliveryFailureRes.Code != http.StatusInternalServerError {
		t.Fatalf("expected token delivery failure 500, got %d: %s", deliveryFailureRes.Code, deliveryFailureRes.Body.String())
	}
	env.server.authTokenSink = env.tokenSink

	verifyToken := latestAuthToken(t, env.tokenSink, "developer-owner@example.com", "email_verification")
	verifyRes := performJSON(env.router, http.MethodPost, "/v1/auth/verify-email", map[string]any{
		"token": verifyToken, "new_password": "password123",
	}, "")
	if verifyRes.Code != http.StatusOK {
		t.Fatalf("expected verify 200, got %d: %s", verifyRes.Code, verifyRes.Body.String())
	}
	loginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "developer-owner@example.com",
		"password": "password123",
	}, "")
	if loginRes.Code != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", loginRes.Code, loginRes.Body.String())
	}
	login := decodeBody[tokenBody](t, loginRes)

	listRes := performJSON(env.router, http.MethodGet, "/v1/developer/brand-clouds", nil, login.Tokens.AccessToken)
	if listRes.Code != http.StatusOK {
		t.Fatalf("expected developer brand cloud list 200, got %d: %s", listRes.Code, listRes.Body.String())
	}
	list := decodeBody[brandCloudsBody](t, listRes)
	if list.Pagination.Total != 1 || len(list.BrandClouds) != 1 || list.BrandClouds[0].ID != signup.BrandCloud.ID {
		t.Fatalf("expected default cloud in list, got %+v", list)
	}

	createRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds", map[string]any{
		"name":        "Second Developer Cloud",
		"tenant_slug": "second-developer-cloud",
	}, login.Tokens.AccessToken)
	if createRes.Code != http.StatusCreated {
		t.Fatalf("expected developer brand cloud create 201, got %d: %s", createRes.Code, createRes.Body.String())
	}
	created := decodeBody[brandCloudBody](t, createRes)
	if created.BrandCloud.Name != "Second Developer Cloud" || created.BrandCloud.OrganizationKind != "brand_cloud" {
		t.Fatalf("unexpected developer brand cloud: %+v", created.BrandCloud)
	}

	detailRes := performJSON(env.router, http.MethodGet, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID, nil, login.Tokens.AccessToken)
	if detailRes.Code != http.StatusOK {
		t.Fatalf("expected developer brand cloud detail 200, got %d: %s", detailRes.Code, detailRes.Body.String())
	}
	invalidInviteRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members/invitations", map[string]any{
		"email": "invalid-role@example.com", "role": "root",
	}, login.Tokens.AccessToken)
	if invalidInviteRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid developer role 400, got %d: %s", invalidInviteRes.Code, invalidInviteRes.Body.String())
	}
	lastOwnerUpdateRes := performJSON(env.router, http.MethodPatch, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members/"+signup.User.ID, map[string]any{"role": "member"}, login.Tokens.AccessToken)
	if lastOwnerUpdateRes.Code != http.StatusConflict {
		t.Fatalf("expected last owner downgrade 409, got %d: %s", lastOwnerUpdateRes.Code, lastOwnerUpdateRes.Body.String())
	}
	lastOwnerDisableRes := performJSON(env.router, http.MethodPatch, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members/"+signup.User.ID+"/disable", nil, login.Tokens.AccessToken)
	if lastOwnerDisableRes.Code != http.StatusConflict {
		t.Fatalf("expected last owner disable 409, got %d: %s", lastOwnerDisableRes.Code, lastOwnerDisableRes.Body.String())
	}
	lastOwnerRemoveRes := performJSON(env.router, http.MethodDelete, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members/"+signup.User.ID, nil, login.Tokens.AccessToken)
	if lastOwnerRemoveRes.Code != http.StatusConflict {
		t.Fatalf("expected last owner removal 409, got %d: %s", lastOwnerRemoveRes.Code, lastOwnerRemoveRes.Body.String())
	}
	missingEnableRes := performJSON(env.router, http.MethodPatch, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members/00000000-0000-0000-0000-000000000000/enable", nil, login.Tokens.AccessToken)
	if missingEnableRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing member enable 404, got %d: %s", missingEnableRes.Code, missingEnableRes.Body.String())
	}

	member := verifiedDeveloperForTest(t, env, "developer-member@example.com")
	inviteRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members/invitations", map[string]any{
		"email": "developer-member@example.com",
		"role":  "member",
	}, login.Tokens.AccessToken)
	if inviteRes.Code != http.StatusAccepted {
		t.Fatalf("expected member invitation 202, got %d: %s", inviteRes.Code, inviteRes.Body.String())
	}
	if !bytes.Contains(inviteRes.Body.Bytes(), []byte(`"status":"pending"`)) || bytes.Contains(inviteRes.Body.Bytes(), []byte(`"member":`)) {
		t.Fatalf("expected pending invitation without membership, got %s", inviteRes.Body.String())
	}
	duplicateInviteRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members/invitations", map[string]any{
		"email": "developer-member@example.com",
		"role":  "member",
	}, login.Tokens.AccessToken)
	if duplicateInviteRes.Code != http.StatusAccepted {
		t.Fatalf("expected matching pending invitation to be idempotent, got %d: %s", duplicateInviteRes.Code, duplicateInviteRes.Body.String())
	}
	conflictingInviteRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members/invitations", map[string]any{
		"email": "developer-member@example.com",
		"role":  "admin",
	}, login.Tokens.AccessToken)
	if conflictingInviteRes.Code != http.StatusConflict {
		t.Fatalf("expected pending invitation role conflict 409, got %d: %s", conflictingInviteRes.Code, conflictingInviteRes.Body.String())
	}
	memberDetailRes := performJSON(env.router, http.MethodGet, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID, nil, member.AccessToken)
	if memberDetailRes.Code != http.StatusNotFound {
		t.Fatalf("membership must not exist before acceptance, got %d: %s", memberDetailRes.Code, memberDetailRes.Body.String())
	}
	invitationsRes := performJSON(env.router, http.MethodGet, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members/invitations", nil, login.Tokens.AccessToken)
	if invitationsRes.Code != http.StatusOK || !bytes.Contains(invitationsRes.Body.Bytes(), []byte("developer-member@example.com")) {
		t.Fatalf("expected owner invitation list 200, got %d: %s", invitationsRes.Code, invitationsRes.Body.String())
	}
	invitation := decodeBody[developerBrandCloudInvitationResponse](t, inviteRes).Invitation
	invitationToken := latestAuthToken(t, env.tokenSink, "developer-member@example.com", "brand_cloud_membership_invitation")
	resendRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members/invitations/"+invitation.ID+"/resend", nil, login.Tokens.AccessToken)
	if resendRes.Code != http.StatusAccepted {
		t.Fatalf("expected invitation resend 202, got %d: %s", resendRes.Code, resendRes.Body.String())
	}
	rotatedToken := latestAuthToken(t, env.tokenSink, "developer-member@example.com", "brand_cloud_membership_invitation")
	if rotatedToken == invitationToken {
		t.Fatal("expected resend to rotate the invitation token")
	}
	oldTokenRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-cloud-member-invitations/accept", map[string]any{"token": invitationToken}, member.AccessToken)
	if oldTokenRes.Code != http.StatusNotFound {
		t.Fatalf("expected superseded invitation token 404, got %d: %s", oldTokenRes.Code, oldTokenRes.Body.String())
	}
	cancelRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members/invitations/"+invitation.ID+"/cancel", nil, login.Tokens.AccessToken)
	if cancelRes.Code != http.StatusOK {
		t.Fatalf("expected invitation cancel 200, got %d: %s", cancelRes.Code, cancelRes.Body.String())
	}
	canceledTokenRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-cloud-member-invitations/accept", map[string]any{"token": rotatedToken}, member.AccessToken)
	if canceledTokenRes.Code != http.StatusNotFound {
		t.Fatalf("expected canceled invitation token 404, got %d: %s", canceledTokenRes.Code, canceledTokenRes.Body.String())
	}
	reinviteRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members/invitations", map[string]any{
		"email": "developer-member@example.com",
		"role":  "member",
	}, login.Tokens.AccessToken)
	if reinviteRes.Code != http.StatusAccepted {
		t.Fatalf("expected reinvitation after cancel 202, got %d: %s", reinviteRes.Code, reinviteRes.Body.String())
	}
	invitationToken = latestAuthToken(t, env.tokenSink, "developer-member@example.com", "brand_cloud_membership_invitation")
	wrongAcceptRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-cloud-member-invitations/accept", map[string]any{"token": invitationToken}, login.Tokens.AccessToken)
	if wrongAcceptRes.Code != http.StatusNotFound {
		t.Fatalf("expected wrong developer invitation acceptance 404, got %d: %s", wrongAcceptRes.Code, wrongAcceptRes.Body.String())
	}
	acceptRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-cloud-member-invitations/accept", map[string]any{"token": invitationToken}, member.AccessToken)
	if acceptRes.Code != http.StatusOK || !bytes.Contains(acceptRes.Body.Bytes(), []byte(`"role":"member"`)) {
		t.Fatalf("expected invitation acceptance 200, got %d: %s", acceptRes.Code, acceptRes.Body.String())
	}
	replayAcceptRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-cloud-member-invitations/accept", map[string]any{"token": invitationToken}, member.AccessToken)
	if replayAcceptRes.Code != http.StatusNotFound {
		t.Fatalf("expected invitation replay 404, got %d: %s", replayAcceptRes.Code, replayAcceptRes.Body.String())
	}
	memberDetailRes = performJSON(env.router, http.MethodGet, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID, nil, member.AccessToken)
	if memberDetailRes.Code != http.StatusOK || !bytes.Contains(memberDetailRes.Body.Bytes(), []byte(`"role":"member"`)) {
		t.Fatalf("expected member developer cloud detail 200, got %d: %s", memberDetailRes.Code, memberDetailRes.Body.String())
	}
	memberManageRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members/invitations", map[string]any{
		"email": "forbidden-invite@example.com", "role": "member",
	}, member.AccessToken)
	if memberManageRes.Code != http.StatusForbidden {
		t.Fatalf("expected member management 403, got %d: %s", memberManageRes.Code, memberManageRes.Body.String())
	}

	membersRes := performJSON(env.router, http.MethodGet, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members", nil, member.AccessToken)
	if membersRes.Code != http.StatusOK {
		t.Fatalf("expected member list 200, got %d: %s", membersRes.Code, membersRes.Body.String())
	}
	members := decodeBody[membersBody](t, membersRes)
	if members.Pagination.Total != 2 || len(members.Members) != 2 {
		t.Fatalf("expected owner and invited member, got %+v", members)
	}

	updateRes := performJSON(env.router, http.MethodPatch, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members/"+member.UserID, map[string]any{"role": "admin"}, login.Tokens.AccessToken)
	if updateRes.Code != http.StatusOK || decodeBody[memberBody](t, updateRes).Member.Role != "admin" {
		t.Fatalf("expected member role update 200, got %d: %s", updateRes.Code, updateRes.Body.String())
	}
	disableRes := performJSON(env.router, http.MethodPatch, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members/"+member.UserID+"/disable", nil, login.Tokens.AccessToken)
	if disableRes.Code != http.StatusOK || decodeBody[memberBody](t, disableRes).Member.DisabledAt == nil {
		t.Fatalf("expected member disable 200, got %d: %s", disableRes.Code, disableRes.Body.String())
	}
	enableRes := performJSON(env.router, http.MethodPatch, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members/"+member.UserID+"/enable", nil, login.Tokens.AccessToken)
	if enableRes.Code != http.StatusOK || decodeBody[memberBody](t, enableRes).Member.DisabledAt != nil {
		t.Fatalf("expected member enable 200, got %d: %s", enableRes.Code, enableRes.Body.String())
	}
	removeRes := performJSON(env.router, http.MethodDelete, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members/"+member.UserID, nil, login.Tokens.AccessToken)
	if removeRes.Code != http.StatusNoContent {
		t.Fatalf("expected member removal 204, got %d: %s", removeRes.Code, removeRes.Body.String())
	}
	removedMemberListRes := performJSON(env.router, http.MethodGet, "/v1/developer/brand-clouds/"+signup.BrandCloud.ID+"/members", nil, member.AccessToken)
	if removedMemberListRes.Code != http.StatusNotFound {
		t.Fatalf("expected removed member list 404, got %d: %s", removedMemberListRes.Code, removedMemberListRes.Body.String())
	}
}

func TestIntegrationBrandCloudOwnerTransferRequiresEmailTokenAndTargetSession(t *testing.T) {
	env := newIntegrationEnv(t)
	source := verifiedDeveloperForTest(t, env, "source-transfer@example.com")
	target := verifiedDeveloperForTest(t, env, "target-transfer@example.com")

	invalidCreateRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds", map[string]any{
		"name":        "Invalid Slug Cloud",
		"tenant_slug": "!!!",
	}, source.AccessToken)
	if invalidCreateRes.Code != http.StatusConflict {
		t.Fatalf("expected invalid developer cloud slug 409, got %d: %s", invalidCreateRes.Code, invalidCreateRes.Body.String())
	}

	invalidTransferRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds/"+source.BrandCloudID+"/owner-transfer", map[string]any{}, source.AccessToken)
	if invalidTransferRes.Code != http.StatusBadRequest {
		t.Fatalf("expected missing target email 400, got %d: %s", invalidTransferRes.Code, invalidTransferRes.Body.String())
	}

	selfTransferRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds/"+source.BrandCloudID+"/owner-transfer", map[string]any{
		"target_email": "source-transfer@example.com",
	}, source.AccessToken)
	if selfTransferRes.Code != http.StatusConflict {
		t.Fatalf("expected self-transfer 409, got %d: %s", selfTransferRes.Code, selfTransferRes.Body.String())
	}

	missingRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds/"+source.BrandCloudID+"/owner-transfer", map[string]any{
		"target_email": "missing-transfer@example.com",
	}, source.AccessToken)
	if missingRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing target 404, got %d: %s", missingRes.Code, missingRes.Body.String())
	}

	requestRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds/"+source.BrandCloudID+"/owner-transfer", map[string]any{
		"target_email": "target-transfer@example.com",
	}, source.AccessToken)
	if requestRes.Code != http.StatusAccepted {
		t.Fatalf("expected owner transfer request 202, got %d: %s", requestRes.Code, requestRes.Body.String())
	}
	transfer := decodeBody[brandCloudOwnerTransferBody](t, requestRes)
	if transfer.OwnerTransfer.Status != "pending" || transfer.OwnerTransfer.TargetUserID != target.UserID {
		t.Fatalf("unexpected pending transfer: %+v", transfer.OwnerTransfer)
	}
	getTransferRes := performJSON(env.router, http.MethodGet, "/v1/developer/brand-clouds/"+source.BrandCloudID+"/owner-transfer/"+transfer.OwnerTransfer.ID, nil, source.AccessToken)
	if getTransferRes.Code != http.StatusOK || decodeBody[brandCloudOwnerTransferBody](t, getTransferRes).OwnerTransfer.Status != "pending" {
		t.Fatalf("expected pending owner transfer detail 200, got %d: %s", getTransferRes.Code, getTransferRes.Body.String())
	}
	cancelRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds/"+source.BrandCloudID+"/owner-transfer/"+transfer.OwnerTransfer.ID+"/cancel", map[string]any{}, source.AccessToken)
	if cancelRes.Code != http.StatusOK || decodeBody[brandCloudOwnerTransferBody](t, cancelRes).OwnerTransfer.Status != "canceled" {
		t.Fatalf("expected owner transfer cancellation 200, got %d: %s", cancelRes.Code, cancelRes.Body.String())
	}

	requestRes = performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds/"+source.BrandCloudID+"/owner-transfer", map[string]any{
		"target_email": "target-transfer@example.com",
	}, source.AccessToken)
	if requestRes.Code != http.StatusAccepted {
		t.Fatalf("expected replacement owner transfer request 202, got %d: %s", requestRes.Code, requestRes.Body.String())
	}
	transfer = decodeBody[brandCloudOwnerTransferBody](t, requestRes)
	token := latestAuthToken(t, env.tokenSink, "target-transfer@example.com", "brand_cloud_owner_transfer")

	wrongAcceptRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-cloud-owner-transfers/accept", map[string]any{
		"token": token,
	}, source.AccessToken)
	if wrongAcceptRes.Code != http.StatusNotFound {
		t.Fatalf("expected wrong target accept 404, got %d: %s", wrongAcceptRes.Code, wrongAcceptRes.Body.String())
	}

	missingTokenRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-cloud-owner-transfers/accept", map[string]any{}, target.AccessToken)
	if missingTokenRes.Code != http.StatusBadRequest {
		t.Fatalf("expected missing transfer token 400, got %d: %s", missingTokenRes.Code, missingTokenRes.Body.String())
	}

	acceptRes := performJSON(env.router, http.MethodPost, "/v1/developer/brand-cloud-owner-transfers/accept", map[string]any{
		"token": token,
	}, target.AccessToken)
	if acceptRes.Code != http.StatusOK {
		t.Fatalf("expected target accept 200, got %d: %s", acceptRes.Code, acceptRes.Body.String())
	}
	accepted := decodeBody[brandCloudOwnerTransferBody](t, acceptRes)
	if accepted.OwnerTransfer.Status != "accepted" || accepted.OwnerTransfer.AcceptedAt == nil {
		t.Fatalf("expected accepted transfer, got %+v", accepted.OwnerTransfer)
	}

	sourceCloudsRes := performJSON(env.router, http.MethodGet, "/v1/developer/brand-clouds", nil, source.AccessToken)
	targetCloudsRes := performJSON(env.router, http.MethodGet, "/v1/developer/brand-clouds", nil, target.AccessToken)
	sourceClouds := decodeBody[brandCloudsBody](t, sourceCloudsRes)
	targetClouds := decodeBody[brandCloudsBody](t, targetCloudsRes)
	if !brandCloudListHasRole(sourceClouds, source.BrandCloudID, "admin") {
		t.Fatalf("expected source to become admin, got %+v", sourceClouds)
	}
	if !brandCloudListHasRole(targetClouds, source.BrandCloudID, "owner") {
		t.Fatalf("expected target to become owner, got %+v", targetClouds)
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
	if metricsBody.Lifecycle.Outbox.ByStatus == nil || metricsBody.Lifecycle.Inbox.ByStatus == nil || metricsBody.Lifecycle.Operations.ByStatus == nil {
		t.Fatalf("expected lifecycle maps in empty metrics response, got %+v", metricsBody.Lifecycle)
	}
}

func TestIntegrationPlatformAdminMissingBrandResourcesReturnNotFound(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()
	admin := registerUser(t, env.router, "missing-brand-root@example.com", "Missing Brand Root")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	brandID := "00000000-0000-0000-0000-000000000000"
	profileID := "00000000-0000-0000-0000-000000000001"
	userID := "00000000-0000-0000-0000-000000000002"
	profileBody := map[string]any{
		"profile_key": "missing-profile", "display_name": "Missing Profile", "category": "ip_camera",
		"ca_profile": "missing-ca", "issuer_profile": "missing-issuer", "service_options": []string{"video_streaming"},
	}
	userBody := map[string]any{"email": "missing-user@example.com", "password": "password123", "role": "member"}
	tests := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/v1/admin/brand-clouds/" + brandID, nil},
		{http.MethodPatch, "/v1/admin/brand-clouds/" + brandID, map[string]any{"name": "Still Missing"}},
		{http.MethodGet, "/v1/admin/brand-clouds/" + brandID + "/device-item-profiles", nil},
		{http.MethodPost, "/v1/admin/brand-clouds/" + brandID + "/device-item-profiles", profileBody},
		{http.MethodGet, "/v1/admin/brand-clouds/" + brandID + "/device-item-profiles/" + profileID, nil},
		{http.MethodPatch, "/v1/admin/brand-clouds/" + brandID + "/device-item-profiles/" + profileID, map[string]any{"display_name": "Still Missing"}},
		{http.MethodPost, "/v1/admin/brand-clouds/" + brandID + "/device-item-profiles/" + profileID + "/disable", nil},
		{http.MethodGet, "/v1/admin/brand-clouds/" + brandID + "/users", nil},
		{http.MethodPost, "/v1/admin/brand-clouds/" + brandID + "/users", userBody},
		{http.MethodPost, "/v1/admin/brand-clouds/" + brandID + "/users/" + userID + "/disable", nil},
		{http.MethodPost, "/v1/admin/brand-clouds/" + brandID + "/users/" + userID + "/enable", nil},
		{http.MethodPost, "/v1/admin/brand-clouds/" + brandID + "/users/" + userID + "/approve", nil},
		{http.MethodPost, "/v1/admin/brand-clouds/" + brandID + "/users/" + userID + "/app-certificate/revoke", nil},
		{http.MethodDelete, "/v1/admin/brand-clouds/" + brandID + "/users/" + userID, nil},
	}
	for _, tt := range tests {
		res := performJSON(env.router, tt.method, tt.path, tt.body, admin.Tokens.AccessToken)
		want := http.StatusNotFound
		if strings.HasSuffix(tt.path, "/app-certificate/revoke") {
			want = http.StatusOK
		}
		if res.Code != want {
			t.Errorf("%s %s = %d, want %d: %s", tt.method, tt.path, res.Code, want, res.Body.String())
		}
	}
}

func TestIntegrationPrometheusMetricsReportsEmptySnapshot(t *testing.T) {
	env := newIntegrationEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/metrics/prometheus", nil)
	res := httptest.NewRecorder()
	env.router.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected prometheus metrics 200, got %d: %s", res.Code, res.Body.String())
	}
	if got := res.Header().Get("Content-Type"); !strings.Contains(got, "text/plain") {
		t.Fatalf("expected text/plain content type, got %q", got)
	}
	body := res.Body.String()
	for _, want := range []string{
		"rtk_account_manager_up 1\n",
		`rtk_account_manager_eval_signups_total{tier="evaluation"} 0` + "\n",
		`rtk_account_manager_eval_signups_total{tier="commercial"} 0` + "\n",
		"rtk_account_manager_email_verification_completed_total 0\n",
		`rtk_account_manager_quota_raise_requests{status="pending"} 0` + "\n",
		`rtk_account_manager_quota_raise_requests{status="approved"} 0` + "\n",
		`rtk_account_manager_quota_raise_requests{status="declined"} 0` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected prometheus body to contain %q, got:\n%s", want, body)
		}
	}
}

func TestIntegrationPlatformAdminBrandCloudLifecycle(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	admin := registerUser(t, env.router, "brand-root@example.com", "Root Org")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	owner := registerUser(t, env.router, "brand-owner@example.com", "Owner Org")
	nonAdmin := registerUser(t, env.router, "brand-user@example.com", "User Org")

	nonAdminCreateRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds", map[string]any{
		"name": "Realtek Connect+",
	}, nonAdmin.Tokens.AccessToken)
	if nonAdminCreateRes.Code != http.StatusForbidden {
		t.Fatalf("expected non-admin create 403, got %d: %s", nonAdminCreateRes.Code, nonAdminCreateRes.Body.String())
	}

	createRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds", map[string]any{
		"name":     "Realtek Connect+",
		"metadata": map[string]any{"public_name": "Realtek Connect+"},
	}, admin.Tokens.AccessToken)
	if createRes.Code != http.StatusCreated {
		t.Fatalf("expected brand cloud create 201, got %d: %s", createRes.Code, createRes.Body.String())
	}
	created := decodeBody[brandCloudBody](t, createRes)
	if created.BrandCloud.OrganizationKind != "brand_cloud" || created.BrandCloud.Status != "active" {
		t.Fatalf("unexpected brand cloud response: %+v", created.BrandCloud)
	}

	nonAdminGetRes := performJSON(env.router, http.MethodGet, "/v1/admin/brand-clouds/"+created.BrandCloud.ID, nil, nonAdmin.Tokens.AccessToken)
	if nonAdminGetRes.Code != http.StatusForbidden {
		t.Fatalf("expected non-admin brand cloud get 403, got %d: %s", nonAdminGetRes.Code, nonAdminGetRes.Body.String())
	}
	getRes := performJSON(env.router, http.MethodGet, "/v1/admin/brand-clouds/"+created.BrandCloud.ID, nil, admin.Tokens.AccessToken)
	if getRes.Code != http.StatusOK {
		t.Fatalf("expected brand cloud get 200, got %d: %s", getRes.Code, getRes.Body.String())
	}
	got := decodeBody[brandCloudBody](t, getRes)
	if got.BrandCloud.ID != created.BrandCloud.ID || got.BrandCloud.TenantSlug == "" {
		t.Fatalf("unexpected brand cloud get response: %+v", got.BrandCloud)
	}
	missingGetRes := performJSON(env.router, http.MethodGet, "/v1/admin/brand-clouds/00000000-0000-0000-0000-000000000000", nil, admin.Tokens.AccessToken)
	if missingGetRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing brand cloud get 404, got %d: %s", missingGetRes.Code, missingGetRes.Body.String())
	}

	patchRes := performJSON(env.router, http.MethodPatch, "/v1/admin/brand-clouds/"+created.BrandCloud.ID, map[string]any{
		"name":   "Realtek Connect Plus",
		"status": "disabled",
	}, admin.Tokens.AccessToken)
	if patchRes.Code != http.StatusOK {
		t.Fatalf("expected brand cloud patch 200, got %d: %s", patchRes.Code, patchRes.Body.String())
	}
	patched := decodeBody[brandCloudBody](t, patchRes)
	if patched.BrandCloud.Name != "Realtek Connect Plus" || patched.BrandCloud.Status != "disabled" {
		t.Fatalf("unexpected patched brand cloud: %+v", patched.BrandCloud)
	}
	missingPatchRes := performJSON(env.router, http.MethodPatch, "/v1/admin/brand-clouds/00000000-0000-0000-0000-000000000000", map[string]any{
		"name": "Missing Brand Cloud",
	}, admin.Tokens.AccessToken)
	if missingPatchRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing brand cloud patch 404, got %d: %s", missingPatchRes.Code, missingPatchRes.Body.String())
	}

	brandUserRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+created.BrandCloud.ID+"/users", map[string]any{
		"email":    owner.User.Email,
		"password": "brand-owner-password123",
		"role":     "owner",
	}, admin.Tokens.AccessToken)
	if brandUserRes.Code != http.StatusCreated {
		t.Fatalf("expected brand cloud user create 201, got %d: %s", brandUserRes.Code, brandUserRes.Body.String())
	}
	brandUser := decodeBody[brandCloudUserBody](t, brandUserRes)
	memberRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+created.BrandCloud.ID+"/members", map[string]any{
		"brand_cloud_user_id": brandUser.BrandCloudUser.ID,
		"role":                "owner",
	}, admin.Tokens.AccessToken)
	if memberRes.Code != http.StatusCreated {
		t.Fatalf("expected brand cloud member assignment 201, got %d: %s", memberRes.Code, memberRes.Body.String())
	}
	missingMemberRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/00000000-0000-0000-0000-000000000000/members", map[string]any{
		"brand_cloud_user_id": brandUser.BrandCloudUser.ID,
		"role":                "member",
	}, admin.Tokens.AccessToken)
	if missingMemberRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing brand cloud member assignment 404, got %d: %s", missingMemberRes.Code, missingMemberRes.Body.String())
	}

	listRes := performJSON(env.router, http.MethodGet, "/v1/admin/brand-clouds", nil, admin.Tokens.AccessToken)
	if listRes.Code != http.StatusOK {
		t.Fatalf("expected brand cloud list 200, got %d: %s", listRes.Code, listRes.Body.String())
	}
	list := decodeBody[brandCloudsBody](t, listRes)
	if len(list.BrandClouds) != 1 || list.BrandClouds[0].ID != created.BrandCloud.ID || list.BrandClouds[0].OrganizationKind != "brand_cloud" {
		t.Fatalf("unexpected brand cloud list: %+v", list.BrandClouds)
	}

	auditRes := performJSON(env.router, http.MethodGet, "/v1/admin/audit-events?subject_type=brand_cloud", nil, admin.Tokens.AccessToken)
	if auditRes.Code != http.StatusOK {
		t.Fatalf("expected audit list 200, got %d: %s", auditRes.Code, auditRes.Body.String())
	}
	audit := decodeBody[auditEventsBody](t, auditRes)
	if len(audit.AuditEvents) < 3 {
		t.Fatalf("expected create/update/member audit events, got %+v", audit.AuditEvents)
	}
}

func TestIntegrationPlatformAdminDeviceItemProfileLifecycle(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	admin := registerUser(t, env.router, "profile-api-root@example.com", "Profile API Root")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	nonAdmin := registerUser(t, env.router, "profile-api-user@example.com", "Profile API User")
	owner := registerUser(t, env.router, "profile-api-owner@example.com", "Profile API Owner")

	brandRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds", map[string]any{
		"name": "Profile API Brand",
	}, admin.Tokens.AccessToken)
	if brandRes.Code != http.StatusCreated {
		t.Fatalf("expected brand cloud create 201, got %d: %s", brandRes.Code, brandRes.Body.String())
	}
	brand := decodeBody[brandCloudBody](t, brandRes)

	nonAdminRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/device-item-profiles", map[string]any{
		"profile_key":     "api-cam-v1",
		"display_name":    "API Camera V1",
		"category":        "ip_camera",
		"ca_profile":      "brand-ca",
		"issuer_profile":  "issuer-a",
		"service_options": []string{"video_streaming", "video_storage"},
	}, nonAdmin.Tokens.AccessToken)
	if nonAdminRes.Code != http.StatusForbidden {
		t.Fatalf("expected non-admin profile create 403, got %d", nonAdminRes.Code)
	}

	createRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/device-item-profiles", map[string]any{
		"profile_key":       "api-cam-v1",
		"display_name":      "API Camera V1",
		"category":          "ip_camera",
		"manufacturer":      "Realtek",
		"model":             "API-100",
		"metadata_defaults": map[string]any{"region": "tw"},
		"metadata_schema":   map[string]any{"type": "object"},
		"ca_profile":        "brand-ca",
		"issuer_profile":    "issuer-a",
		"service_options":   []string{"video_streaming", "video_storage"},
	}, admin.Tokens.AccessToken)
	if createRes.Code != http.StatusCreated {
		t.Fatalf("expected profile create 201, got %d: %s", createRes.Code, createRes.Body.String())
	}
	created := decodeBody[deviceItemProfileBody](t, createRes)
	if created.DeviceItemProfile.ProfileKey != "api-cam-v1" || created.DeviceItemProfile.CAProfile != "brand-ca" {
		t.Fatalf("unexpected created profile: %+v", created.DeviceItemProfile)
	}

	invalidCreateRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/device-item-profiles", map[string]any{
		"profile_key":     "api-cam-invalid",
		"display_name":    "API Camera Invalid",
		"category":        "thermostat",
		"ca_profile":      "brand-ca",
		"issuer_profile":  "issuer-a",
		"service_options": []string{"video_streaming"},
	}, admin.Tokens.AccessToken)
	if invalidCreateRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid category 400, got %d: %s", invalidCreateRes.Code, invalidCreateRes.Body.String())
	}

	invalidServiceRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/device-item-profiles", map[string]any{
		"profile_key":     "api-cam-invalid-service",
		"display_name":    "API Camera Invalid Service",
		"category":        "ip_camera",
		"ca_profile":      "brand-ca",
		"issuer_profile":  "issuer-a",
		"service_options": []string{"category-derived-acl"},
	}, admin.Tokens.AccessToken)
	if invalidServiceRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid service_options 400, got %d: %s", invalidServiceRes.Code, invalidServiceRes.Body.String())
	}

	listRes := performJSON(env.router, http.MethodGet, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/device-item-profiles", nil, admin.Tokens.AccessToken)
	if listRes.Code != http.StatusOK {
		t.Fatalf("expected profile list 200, got %d: %s", listRes.Code, listRes.Body.String())
	}
	list := decodeBody[deviceItemProfilesBody](t, listRes)
	if list.Pagination.Total != 1 || list.DeviceItemProfiles[0].ID != created.DeviceItemProfile.ID {
		t.Fatalf("unexpected profile list: %+v", list)
	}

	filterRes := performJSON(env.router, http.MethodGet, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/device-item-profiles?status=active", nil, admin.Tokens.AccessToken)
	if filterRes.Code != http.StatusOK {
		t.Fatalf("expected active profile list 200, got %d: %s", filterRes.Code, filterRes.Body.String())
	}
	filtered := decodeBody[deviceItemProfilesBody](t, filterRes)
	if filtered.Pagination.Total != 1 || filtered.DeviceItemProfiles[0].Status != model.DeviceItemProfileStatusActive {
		t.Fatalf("unexpected active profile list: %+v", filtered)
	}

	invalidStatusListRes := performJSON(env.router, http.MethodGet, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/device-item-profiles?status=retired", nil, admin.Tokens.AccessToken)
	if invalidStatusListRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid list status 400, got %d: %s", invalidStatusListRes.Code, invalidStatusListRes.Body.String())
	}

	getRes := performJSON(env.router, http.MethodGet, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/device-item-profiles/"+created.DeviceItemProfile.ID, nil, admin.Tokens.AccessToken)
	if getRes.Code != http.StatusOK {
		t.Fatalf("expected profile get 200, got %d: %s", getRes.Code, getRes.Body.String())
	}
	got := decodeBody[deviceItemProfileBody](t, getRes)
	if got.DeviceItemProfile.ID != created.DeviceItemProfile.ID {
		t.Fatalf("unexpected profile get: %+v", got.DeviceItemProfile)
	}

	missingGetRes := performJSON(env.router, http.MethodGet, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/device-item-profiles/00000000-0000-4000-8000-000000000001", nil, admin.Tokens.AccessToken)
	if missingGetRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing profile 404, got %d: %s", missingGetRes.Code, missingGetRes.Body.String())
	}

	patchRes := performJSON(env.router, http.MethodPatch, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/device-item-profiles/"+created.DeviceItemProfile.ID, map[string]any{
		"display_name":      "API Camera V1 Rev B",
		"status":            "active",
		"category":          "generic",
		"manufacturer":      "Realtek Semiconductor",
		"model":             "API-100B",
		"metadata_defaults": map[string]any{"region": "tw", "sku": "api-100b"},
		"metadata_schema":   map[string]any{"type": "object", "additionalProperties": true},
		"ca_profile":        "brand-ca-b",
		"issuer_profile":    "issuer-b",
		"service_options":   []string{"mqtt", "video_streaming"},
	}, admin.Tokens.AccessToken)
	if patchRes.Code != http.StatusOK {
		t.Fatalf("expected profile patch 200, got %d: %s", patchRes.Code, patchRes.Body.String())
	}
	patched := decodeBody[deviceItemProfileBody](t, patchRes)
	if patched.DeviceItemProfile.DisplayName != "API Camera V1 Rev B" ||
		patched.DeviceItemProfile.Category != model.DeviceCategoryGeneric ||
		patched.DeviceItemProfile.CAProfile != "brand-ca-b" ||
		patched.DeviceItemProfile.IssuerProfile != "issuer-b" ||
		patched.DeviceItemProfile.Model == nil ||
		*patched.DeviceItemProfile.Model != "API-100B" ||
		!equalStringSlices(patched.DeviceItemProfile.ServiceOptions, []string{"mqtt", "video_streaming"}) {
		t.Fatalf("unexpected patched profile: %+v", patched.DeviceItemProfile)
	}

	invalidPatchRes := performJSON(env.router, http.MethodPatch, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/device-item-profiles/"+created.DeviceItemProfile.ID, map[string]any{
		"status": "retired",
	}, admin.Tokens.AccessToken)
	if invalidPatchRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid patch status 400, got %d: %s", invalidPatchRes.Code, invalidPatchRes.Body.String())
	}

	tokenRes := performJSON(env.router, http.MethodPost, "/v1/admin/device-claim-tokens", map[string]any{
		"organization_id":        owner.Organization.ID,
		"claim_token":            "profile-api-claim",
		"device_item_profile_id": created.DeviceItemProfile.ID,
		"video_cloud_devid":      "profile-api-video",
		"activity_id":            "profile-api-activity",
		"clip_public_key":        "profile-api-clip",
		"expires_at":             time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		"metadata":               map[string]any{"batch": "api"},
	}, admin.Tokens.AccessToken)
	if tokenRes.Code != http.StatusCreated {
		t.Fatalf("expected profile-backed claim token 201, got %d: %s", tokenRes.Code, tokenRes.Body.String())
	}
	token := decodeBody[deviceClaimTokenAdminBody](t, tokenRes)
	if token.DeviceClaimToken.DeviceItemProfileID == nil || *token.DeviceClaimToken.DeviceItemProfileID != created.DeviceItemProfile.ID ||
		!equalStringSlices(token.DeviceClaimToken.ServiceOptions, []string{"mqtt", "video_streaming"}) ||
		token.DeviceClaimToken.Metadata["profile_key"] != "api-cam-v1" {
		t.Fatalf("unexpected profile-backed token: %+v", token.DeviceClaimToken)
	}

	mismatchRes := performJSON(env.router, http.MethodPost, "/v1/admin/device-claim-tokens", map[string]any{
		"claim_token":            "profile-api-mismatch",
		"device_item_profile_id": created.DeviceItemProfile.ID,
		"video_cloud_devid":      "profile-api-video-mismatch",
		"activity_id":            "profile-api-activity-mismatch",
		"clip_public_key":        "profile-api-clip-mismatch",
		"service_options":        []string{"mqtt"},
		"expires_at":             time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	}, admin.Tokens.AccessToken)
	if mismatchRes.Code != http.StatusBadRequest {
		t.Fatalf("expected service_options mismatch 400, got %d: %s", mismatchRes.Code, mismatchRes.Body.String())
	}

	disableRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/device-item-profiles/"+created.DeviceItemProfile.ID+"/disable", nil, admin.Tokens.AccessToken)
	if disableRes.Code != http.StatusOK {
		t.Fatalf("expected profile disable 200, got %d: %s", disableRes.Code, disableRes.Body.String())
	}
	disabled := decodeBody[deviceItemProfileBody](t, disableRes)
	if disabled.DeviceItemProfile.Status != model.DeviceItemProfileStatusDisabled {
		t.Fatalf("expected disabled profile, got %+v", disabled.DeviceItemProfile)
	}
}

func TestIntegrationPlatformAdminCreatesProductionRunJWT(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	admin := registerUser(t, env.router, "production-run-api-root@example.com", "Production Run API Root")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	nonAdmin := registerUser(t, env.router, "production-run-api-user@example.com", "Production Run API User")

	brandRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds", map[string]any{
		"name": "Production Run API Brand",
	}, admin.Tokens.AccessToken)
	if brandRes.Code != http.StatusCreated {
		t.Fatalf("expected brand cloud create 201, got %d: %s", brandRes.Code, brandRes.Body.String())
	}
	brand := decodeBody[brandCloudBody](t, brandRes)

	profileRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/device-item-profiles", map[string]any{
		"profile_key":     "prod-api-cam-v1",
		"display_name":    "Production API Camera V1",
		"category":        "ip_camera",
		"ca_profile":      "sku-ca-prod-api",
		"issuer_profile":  "factory-line-a",
		"service_options": []string{"video_streaming"},
	}, admin.Tokens.AccessToken)
	if profileRes.Code != http.StatusCreated {
		t.Fatalf("expected profile create 201, got %d: %s", profileRes.Code, profileRes.Body.String())
	}
	profile := decodeBody[deviceItemProfileBody](t, profileRes)

	validFrom := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	validUntil := validFrom.Add(24 * time.Hour)
	path := "/v1/admin/brand-clouds/" + brand.BrandCloud.ID + "/device-item-profiles/" + profile.DeviceItemProfile.ID + "/production-runs"

	unconfiguredSignerRes := performJSON(env.router, http.MethodPost, path, map[string]any{
		"allowed_quantity": 10,
		"valid_from":       validFrom.Format(time.RFC3339),
		"valid_until":      validUntil.Format(time.RFC3339),
	}, admin.Tokens.AccessToken)
	if unconfiguredSignerRes.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected unconfigured production JWT signer 503, got %d: %s", unconfiguredSignerRes.Code, unconfiguredSignerRes.Body.String())
	}

	env.server.ConfigureProductionJWT("factory-production-secret", "")

	nonAdminRes := performJSON(env.router, http.MethodPost, path, map[string]any{
		"allowed_quantity": 10,
		"valid_from":       validFrom.Format(time.RFC3339),
		"valid_until":      validUntil.Format(time.RFC3339),
	}, nonAdmin.Tokens.AccessToken)
	if nonAdminRes.Code != http.StatusForbidden {
		t.Fatalf("expected non-admin production run create 403, got %d", nonAdminRes.Code)
	}

	invalidQuantityRes := performJSON(env.router, http.MethodPost, path, map[string]any{
		"allowed_quantity": 0,
		"valid_from":       validFrom.Format(time.RFC3339),
		"valid_until":      validUntil.Format(time.RFC3339),
	}, admin.Tokens.AccessToken)
	if invalidQuantityRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid allowed_quantity 400, got %d: %s", invalidQuantityRes.Code, invalidQuantityRes.Body.String())
	}

	invalidPeriodRes := performJSON(env.router, http.MethodPost, path, map[string]any{
		"allowed_quantity": 10,
		"valid_from":       validUntil.Format(time.RFC3339),
		"valid_until":      validFrom.Format(time.RFC3339),
	}, admin.Tokens.AccessToken)
	if invalidPeriodRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid production period 400, got %d: %s", invalidPeriodRes.Code, invalidPeriodRes.Body.String())
	}

	createRes := performJSON(env.router, http.MethodPost, path, map[string]any{
		"factory_id":       "factory-a",
		"batch_id":         "batch-20260617",
		"allowed_quantity": 250,
		"valid_from":       validFrom.Format(time.RFC3339),
		"valid_until":      validUntil.Format(time.RFC3339),
	}, admin.Tokens.AccessToken)
	if createRes.Code != http.StatusCreated {
		t.Fatalf("expected production run create 201, got %d: %s", createRes.Code, createRes.Body.String())
	}
	body := decodeBody[productionRunBody](t, createRes)
	if body.ProductionRun.BrandCloudID != brand.BrandCloud.ID ||
		body.ProductionRun.DeviceItemProfileID != profile.DeviceItemProfile.ID ||
		body.ProductionRun.AllowedQuantity != 250 ||
		body.FactoryJWT == "" ||
		body.TokenType != "Bearer" {
		t.Fatalf("unexpected production run response: %+v", body)
	}

	claims := &productionJWTClaims{}
	token, err := jwt.ParseWithClaims(body.FactoryJWT, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte("factory-production-secret"), nil
	}, jwt.WithAudience("factory-enroll"))
	if err != nil || !token.Valid {
		t.Fatalf("expected valid factory production JWT, token=%v err=%v", token, err)
	}
	if claims.ProductionRunID != body.ProductionRun.ID ||
		claims.BrandCloudID != brand.BrandCloud.ID ||
		claims.DeviceItemProfileID != profile.DeviceItemProfile.ID ||
		claims.ProfileKey != "prod-api-cam-v1" ||
		claims.FactoryID != "factory-a" ||
		claims.BatchID != "batch-20260617" ||
		claims.AllowedQuantity != 250 {
		t.Fatalf("unexpected production JWT claims: %+v", claims)
	}
	var auditPayload string
	if err := env.db.QueryRow(ctx, `
		SELECT payload::text
		FROM audit_events
		WHERE subject_type = 'factory_production_run' AND subject_id = $1
		ORDER BY created_at DESC
		LIMIT 1
	`, body.ProductionRun.ID).Scan(&auditPayload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(auditPayload, body.FactoryJWT) || strings.Contains(auditPayload, "factory-production-secret") {
		t.Fatal("factory production audit payload leaked bearer or signing secret material")
	}
	if _, err := store.New(env.db).AddMember(ctx, brand.BrandCloud.ID, admin.User.Email, model.RoleOwner); err != nil {
		t.Fatalf("add production-run reader membership: %v", err)
	}
	listPath := "/v1/orgs/" + brand.BrandCloud.ID + "/device-item-profiles/" + profile.DeviceItemProfile.ID + "/production-runs"
	listRes := performJSON(env.router, http.MethodGet, listPath, nil, admin.Tokens.AccessToken)
	if listRes.Code != http.StatusOK || !bytes.Contains(listRes.Body.Bytes(), []byte(body.ProductionRun.ID)) {
		t.Fatalf("expected production run list 200 with created run, got %d: %s", listRes.Code, listRes.Body.String())
	}
}

func TestIntegrationPlatformAdminCreatesActiveBrandCloudUser(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	admin := registerUser(t, env.router, "brand-user-root@example.com", "Root Org")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	brandRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds", map[string]any{
		"name":        "RTK",
		"tenant_slug": "rtk-brand",
	}, admin.Tokens.AccessToken)
	if brandRes.Code != http.StatusCreated {
		t.Fatalf("expected brand cloud create 201, got %d: %s", brandRes.Code, brandRes.Body.String())
	}
	brand := decodeBody[brandCloudBody](t, brandRes)

	nonAdmin := registerUser(t, env.router, "brand-user-non-admin@example.com", "Non Admin Org")
	nonAdminRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users", map[string]any{
		"email":        "rtk+001@users.example.com",
		"password":     "initial-password123",
		"display_name": "RTK User 001",
		"role":         "member",
	}, nonAdmin.Tokens.AccessToken)
	if nonAdminRes.Code != http.StatusForbidden {
		t.Fatalf("expected non-admin brand user create 403, got %d: %s", nonAdminRes.Code, nonAdminRes.Body.String())
	}

	createRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users", map[string]any{
		"email":        "RTK+001@Users.Example.Com",
		"password":     "initial-password123",
		"display_name": "RTK User 001",
		"role":         "member",
	}, admin.Tokens.AccessToken)
	if createRes.Code != http.StatusCreated {
		t.Fatalf("expected brand user create 201, got %d: %s", createRes.Code, createRes.Body.String())
	}
	created := decodeBody[brandCloudUserBody](t, createRes)
	if created.Action != "created" || created.BrandCloudUser.Email != "rtk+001@users.example.com" || !created.BrandCloudUser.EmailVerified || created.BrandCloudUser.SignupPendingVerification || created.BrandCloudMember.Role != "member" {
		t.Fatalf("unexpected created brand user response: %+v", created)
	}

	loginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "rtk+001@users.example.com",
		"password": "initial-password123",
	}, "")
	if loginRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected platform login to reject brand-cloud user, got %d: %s", loginRes.Code, loginRes.Body.String())
	}
	brandLoginRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/rtk-brand/auth/login", map[string]any{
		"email":    "rtk+001@users.example.com",
		"password": "initial-password123",
	}, "")
	if brandLoginRes.Code != http.StatusOK {
		t.Fatalf("expected brand-cloud login 200, got %d: %s", brandLoginRes.Code, brandLoginRes.Body.String())
	}
	brandLogin := decodeBody[tokenBody](t, brandLoginRes)
	if brandLogin.AppCertificate.Status != "csr_required" {
		t.Fatalf("brand-cloud app certificate status = %q", brandLogin.AppCertificate.Status)
	}
	brandAsEndUserRes := performJSON(env.router, http.MethodGet, "/v1/app/end-users/me", nil, brandLogin.Tokens.AccessToken)
	if brandAsEndUserRes.Code != http.StatusNotFound {
		t.Fatalf("expected brand-cloud token rejected by APP end-user route, got %d: %s", brandAsEndUserRes.Code, brandAsEndUserRes.Body.String())
	}
	issuer := &fakeAppCertificateIssuer{}
	env.server.ConfigureAppCertificateIssuer(issuer)
	brandCSRRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/rtk-brand/auth/login", map[string]any{
		"email":       "rtk+001@users.example.com",
		"password":    "initial-password123",
		"app_csr_pem": generateTestCSR(t, "app-brand-cloud-user:"+created.BrandCloudUser.ID),
	}, "")
	if brandCSRRes.Code != http.StatusOK {
		t.Fatalf("expected brand-cloud csr login 200, got %d: %s", brandCSRRes.Code, brandCSRRes.Body.String())
	}
	brandCSR := decodeBody[tokenBody](t, brandCSRRes)
	if brandCSR.AppCertificate.Status != "issued" || brandCSR.AppCertificate.FingerprintSHA256 == "" {
		t.Fatalf("brand-cloud app certificate response = %+v", brandCSR.AppCertificate)
	}
	revokeAppCertRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users/"+created.BrandCloudUser.ID+"/app-certificate/revoke", nil, admin.Tokens.AccessToken)
	if revokeAppCertRes.Code != http.StatusOK {
		t.Fatalf("expected app certificate revoke 200, got %d: %s", revokeAppCertRes.Code, revokeAppCertRes.Body.String())
	}
	brandLoginAfterRevokeRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/rtk-brand/auth/login", map[string]any{
		"email":    "rtk+001@users.example.com",
		"password": "initial-password123",
	}, "")
	if brandLoginAfterRevokeRes.Code != http.StatusOK {
		t.Fatalf("expected brand-cloud login after app cert revoke 200, got %d: %s", brandLoginAfterRevokeRes.Code, brandLoginAfterRevokeRes.Body.String())
	}
	brandLoginAfterRevoke := decodeBody[tokenBody](t, brandLoginAfterRevokeRes)
	if brandLoginAfterRevoke.AppCertificate.Status != "csr_required" {
		t.Fatalf("expected csr_required after app cert revoke, got %+v", brandLoginAfterRevoke.AppCertificate)
	}

	if _, err := env.db.Exec(ctx, `UPDATE brand_cloud_users SET disabled_at = now() WHERE id = $1`, created.BrandCloudUser.ID); err != nil {
		t.Fatal(err)
	}
	reassignRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users", map[string]any{
		"email":        "rtk+001@users.example.com",
		"password":     "ignored-password123",
		"display_name": "RTK User 001 Reactivated",
		"role":         "member",
	}, admin.Tokens.AccessToken)
	if reassignRes.Code != http.StatusOK {
		t.Fatalf("expected existing brand user upsert 200, got %d: %s", reassignRes.Code, reassignRes.Body.String())
	}
	reassigned := decodeBody[brandCloudUserBody](t, reassignRes)
	if reassigned.Action != "assigned" || reassigned.BrandCloudUser.DisabledAt != nil || reassigned.BrandCloudUser.DisplayName == nil || *reassigned.BrandCloudUser.DisplayName != "RTK User 001 Reactivated" {
		t.Fatalf("unexpected reassigned brand user response: %+v", reassigned)
	}
	ignoredPasswordLoginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "rtk+001@users.example.com",
		"password": "ignored-password123",
	}, "")
	if ignoredPasswordLoginRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected ignored password login 401, got %d: %s", ignoredPasswordLoginRes.Code, ignoredPasswordLoginRes.Body.String())
	}

	rotateRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users", map[string]any{
		"email":           "rtk+001@users.example.com",
		"password":        "rotated-password123",
		"role":            "member",
		"rotate_password": true,
	}, admin.Tokens.AccessToken)
	if rotateRes.Code != http.StatusOK {
		t.Fatalf("expected rotate existing brand user 200, got %d: %s", rotateRes.Code, rotateRes.Body.String())
	}
	rotatedLoginRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/rtk-brand/auth/login", map[string]any{
		"email":    "rtk+001@users.example.com",
		"password": "rotated-password123",
	}, "")
	if rotatedLoginRes.Code != http.StatusOK {
		t.Fatalf("expected rotated brand-cloud password login 200, got %d: %s", rotatedLoginRes.Code, rotatedLoginRes.Body.String())
	}

	listRes := performJSON(env.router, http.MethodGet, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users?q=rtk%2B001", nil, admin.Tokens.AccessToken)
	if listRes.Code != http.StatusOK {
		t.Fatalf("expected brand user list 200, got %d: %s", listRes.Code, listRes.Body.String())
	}
	listed := decodeBody[brandCloudUsersBody](t, listRes)
	if listed.Pagination.Total != 1 || len(listed.BrandCloudUsers) != 1 || listed.BrandCloudUsers[0].ID != created.BrandCloudUser.ID {
		t.Fatalf("expected created brand user in list, got %+v", listed)
	}

	if _, err := env.db.Exec(ctx, `
		UPDATE brand_cloud_users
		SET email_verified = false, email_verified_at = NULL, signup_pending_verification = true, updated_at = now()
		WHERE id = $1
	`, created.BrandCloudUser.ID); err != nil {
		t.Fatal(err)
	}
	pendingLoginRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/rtk-brand/auth/login", map[string]any{
		"email":    "rtk+001@users.example.com",
		"password": "rotated-password123",
	}, "")
	if pendingLoginRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected pending brand-cloud user login 401, got %d: %s", pendingLoginRes.Code, pendingLoginRes.Body.String())
	}
	pendingListRes := performJSON(env.router, http.MethodGet, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users?status=pending_verification", nil, admin.Tokens.AccessToken)
	if pendingListRes.Code != http.StatusOK {
		t.Fatalf("expected pending brand user list 200, got %d: %s", pendingListRes.Code, pendingListRes.Body.String())
	}
	pendingList := decodeBody[brandCloudUsersBody](t, pendingListRes)
	if pendingList.Pagination.Total != 1 || len(pendingList.BrandCloudUsers) != 1 || !pendingList.BrandCloudUsers[0].SignupPendingVerification {
		t.Fatalf("expected pending brand user in list, got %+v", pendingList)
	}
	approveRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users/"+created.BrandCloudUser.ID+"/approve", nil, admin.Tokens.AccessToken)
	if approveRes.Code != http.StatusOK {
		t.Fatalf("expected brand user approve 200, got %d: %s", approveRes.Code, approveRes.Body.String())
	}
	approved := decodeBody[brandCloudUserStateBody](t, approveRes)
	if !approved.BrandCloudUser.EmailVerified || approved.BrandCloudUser.SignupPendingVerification || approved.BrandCloudUser.DisabledAt != nil {
		t.Fatalf("expected approved brand user, got %+v", approved.BrandCloudUser)
	}
	approvedLoginRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/rtk-brand/auth/login", map[string]any{
		"email":    "rtk+001@users.example.com",
		"password": "rotated-password123",
	}, "")
	if approvedLoginRes.Code != http.StatusOK {
		t.Fatalf("expected approved brand-cloud user login 200, got %d: %s", approvedLoginRes.Code, approvedLoginRes.Body.String())
	}

	disableRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users/"+created.BrandCloudUser.ID+"/disable", nil, admin.Tokens.AccessToken)
	if disableRes.Code != http.StatusOK {
		t.Fatalf("expected brand user disable 200, got %d: %s", disableRes.Code, disableRes.Body.String())
	}
	disabled := decodeBody[brandCloudUserStateBody](t, disableRes)
	if disabled.BrandCloudUser.DisabledAt == nil {
		t.Fatalf("expected disabled brand user, got %+v", disabled.BrandCloudUser)
	}
	disabledLoginRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/rtk-brand/auth/login", map[string]any{
		"email":    "rtk+001@users.example.com",
		"password": "rotated-password123",
	}, "")
	if disabledLoginRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled brand-cloud user login 401, got %d: %s", disabledLoginRes.Code, disabledLoginRes.Body.String())
	}
	disabledListRes := performJSON(env.router, http.MethodGet, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users?status=disabled", nil, admin.Tokens.AccessToken)
	if disabledListRes.Code != http.StatusOK {
		t.Fatalf("expected disabled brand user list 200, got %d: %s", disabledListRes.Code, disabledListRes.Body.String())
	}
	disabledList := decodeBody[brandCloudUsersBody](t, disabledListRes)
	if disabledList.Pagination.Total != 1 || len(disabledList.BrandCloudUsers) != 1 || disabledList.BrandCloudUsers[0].DisabledAt == nil {
		t.Fatalf("expected disabled brand user in disabled list, got %+v", disabledList)
	}

	enableRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users/"+created.BrandCloudUser.ID+"/enable", nil, admin.Tokens.AccessToken)
	if enableRes.Code != http.StatusOK {
		t.Fatalf("expected brand user enable 200, got %d: %s", enableRes.Code, enableRes.Body.String())
	}
	enabled := decodeBody[brandCloudUserStateBody](t, enableRes)
	if enabled.BrandCloudUser.DisabledAt != nil {
		t.Fatalf("expected enabled brand user, got %+v", enabled.BrandCloudUser)
	}
	enabledLoginRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/rtk-brand/auth/login", map[string]any{
		"email":    "rtk+001@users.example.com",
		"password": "rotated-password123",
	}, "")
	if enabledLoginRes.Code != http.StatusOK {
		t.Fatalf("expected enabled brand-cloud user login 200, got %d: %s", enabledLoginRes.Code, enabledLoginRes.Body.String())
	}

	deleteRes := performJSON(env.router, http.MethodDelete, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users/"+created.BrandCloudUser.ID, nil, admin.Tokens.AccessToken)
	if deleteRes.Code != http.StatusNoContent {
		t.Fatalf("expected brand user delete 204, got %d: %s", deleteRes.Code, deleteRes.Body.String())
	}
	deletedLoginRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/rtk-brand/auth/login", map[string]any{
		"email":    "rtk+001@users.example.com",
		"password": "rotated-password123",
	}, "")
	if deletedLoginRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected soft-deleted brand-cloud user login 401, got %d: %s", deletedLoginRes.Code, deletedLoginRes.Body.String())
	}

	auditRes := performJSON(env.router, http.MethodGet, "/v1/admin/audit-events?subject_type=brand_cloud", nil, admin.Tokens.AccessToken)
	if auditRes.Code != http.StatusOK {
		t.Fatalf("expected audit event list 200, got %d: %s", auditRes.Code, auditRes.Body.String())
	}
	audit := decodeBody[auditEventsBody](t, auditRes)
	if !hasAuditEventBody(audit.AuditEvents, "brand_cloud_user_created") || !hasAuditEventBody(audit.AuditEvents, "brand_cloud_user_assigned") {
		t.Fatalf("expected brand cloud user audit events, got %+v", audit.AuditEvents)
	}
	userAuditRes := performJSON(env.router, http.MethodGet, "/v1/admin/audit-events?subject_type=brand_cloud_user", nil, admin.Tokens.AccessToken)
	if userAuditRes.Code != http.StatusOK {
		t.Fatalf("expected brand cloud user audit event list 200, got %d: %s", userAuditRes.Code, userAuditRes.Body.String())
	}
	userAudit := decodeBody[auditEventsBody](t, userAuditRes)
	if !hasAuditEventBody(userAudit.AuditEvents, "brand_cloud_user_approved") || !hasAuditEventBody(userAudit.AuditEvents, "brand_cloud_user_disabled") || !hasAuditEventBody(userAudit.AuditEvents, "brand_cloud_user_enabled") || !hasAuditEventBody(userAudit.AuditEvents, "brand_cloud_user_deleted") {
		t.Fatalf("expected brand cloud user lifecycle audit events, got %+v", userAudit.AuditEvents)
	}
	for _, event := range userAudit.AuditEvents {
		if event.ActorUserID == nil || *event.ActorUserID != admin.User.ID ||
			event.OrganizationID == nil || *event.OrganizationID != brand.BrandCloud.ID ||
			event.SubjectType != "brand_cloud_user" || event.SubjectID != created.BrandCloudUser.ID {
			t.Fatalf("brand-cloud user audit actor or subject is misattributed: %+v", event)
		}
	}
}

func TestIntegrationBrandScopedUsersLoginAndAuthorizeByTenantSlug(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	admin := registerUser(t, env.router, "brand-scope-admin@example.com", "Brand Scope Admin")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}

	acmeRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds", map[string]any{
		"name":        "Acme Brand",
		"tenant_slug": "acme",
	}, admin.Tokens.AccessToken)
	if acmeRes.Code != http.StatusCreated {
		t.Fatalf("expected acme brand create 201, got %d: %s", acmeRes.Code, acmeRes.Body.String())
	}
	acme := decodeBody[brandCloudBody](t, acmeRes)
	if acme.BrandCloud.TenantSlug != "acme" {
		t.Fatalf("expected tenant slug acme, got %+v", acme.BrandCloud)
	}

	contosoRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds", map[string]any{
		"name":        "Contoso Brand",
		"tenant_slug": "contoso",
	}, admin.Tokens.AccessToken)
	if contosoRes.Code != http.StatusCreated {
		t.Fatalf("expected contoso brand create 201, got %d: %s", contosoRes.Code, contosoRes.Body.String())
	}
	contoso := decodeBody[brandCloudBody](t, contosoRes)

	for _, brand := range []brandCloudBody{acme, contoso} {
		createRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users", map[string]any{
			"email":        "shared@users.example.com",
			"password":     "brand-password123",
			"display_name": "Shared User",
			"role":         "member",
		}, admin.Tokens.AccessToken)
		if createRes.Code != http.StatusCreated {
			t.Fatalf("expected brand user create 201 for %s, got %d: %s", brand.BrandCloud.TenantSlug, createRes.Code, createRes.Body.String())
		}
		created := decodeBody[brandCloudUserBody](t, createRes)
		if created.BrandCloudUser.ID == "" || created.BrandCloudUser.BrandCloudID != brand.BrandCloud.ID || created.BrandCloudMember.Role != "member" {
			t.Fatalf("unexpected brand-scoped user response for %s: %+v", brand.BrandCloud.TenantSlug, created)
		}
	}

	platformLoginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    "shared@users.example.com",
		"password": "brand-password123",
	}, "")
	if platformLoginRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected platform login to reject brand-cloud user, got %d: %s", platformLoginRes.Code, platformLoginRes.Body.String())
	}

	wrongSlugRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/acme/auth/login", map[string]any{
		"email":    "shared@users.example.com",
		"password": "wrong-password123",
	}, "")
	if wrongSlugRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected wrong password login 401, got %d: %s", wrongSlugRes.Code, wrongSlugRes.Body.String())
	}

	acmeLoginRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/acme/auth/login", map[string]any{
		"email":    "shared@users.example.com",
		"password": "brand-password123",
	}, "")
	if acmeLoginRes.Code != http.StatusOK {
		t.Fatalf("expected acme brand login 200, got %d: %s", acmeLoginRes.Code, acmeLoginRes.Body.String())
	}
	acmeLogin := decodeBody[brandCloudLoginBody](t, acmeLoginRes)
	acmeClaims, err := env.server.auth.ParseAccessToken(acmeLogin.Tokens.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if acmeClaims.SubjectType != auth.SubjectTypeBrandCloudUser || acmeClaims.UserID != acmeLogin.User.ID || acmeClaims.BrandCloudID != acme.BrandCloud.ID || acmeClaims.TenantSlug != "acme" || acmeClaims.BrandCloudUserID == "" {
		t.Fatalf("unexpected acme access token claims: %+v", acmeClaims)
	}

	acmeSignInRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/acme/auth/sign-in", map[string]any{
		"email": "shared@users.example.com",
	}, "")
	if acmeSignInRes.Code != http.StatusAccepted {
		t.Fatalf("expected acme sign-in 202, got %d: %s", acmeSignInRes.Code, acmeSignInRes.Body.String())
	}
	acmeLoginActivationToken := latestAuthToken(t, env.tokenSink, "shared@users.example.com", "login_activation")
	tenantMismatchRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/contoso/auth/login/activate", map[string]any{
		"token": acmeLoginActivationToken,
	}, "")
	if tenantMismatchRes.Code != http.StatusBadRequest {
		t.Fatalf("expected tenant-mismatched login activation 400, got %d: %s", tenantMismatchRes.Code, tenantMismatchRes.Body.String())
	}
	acmeActivateRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/acme/auth/login/activate", map[string]any{
		"token": acmeLoginActivationToken,
	}, "")
	if acmeActivateRes.Code != http.StatusOK {
		t.Fatalf("expected acme login activation 200 after mismatch attempt, got %d: %s", acmeActivateRes.Code, acmeActivateRes.Body.String())
	}
	acmeActivated := decodeBody[brandCloudLoginBody](t, acmeActivateRes)
	acmeActivatedClaims, err := env.server.auth.ParseAccessToken(acmeActivated.Tokens.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if acmeActivatedClaims.SubjectType != auth.SubjectTypeBrandCloudUser || acmeActivatedClaims.TenantSlug != "acme" || acmeActivatedClaims.BrandCloudID != acme.BrandCloud.ID {
		t.Fatalf("unexpected acme activation claims: %+v", acmeActivatedClaims)
	}
	replayBrandActivationRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/acme/auth/login/activate", map[string]any{
		"token": acmeLoginActivationToken,
	}, "")
	if replayBrandActivationRes.Code != http.StatusBadRequest {
		t.Fatalf("expected replayed brand activation token 400, got %d", replayBrandActivationRes.Code)
	}

	contosoLoginRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/contoso/auth/login", map[string]any{
		"email":    "shared@users.example.com",
		"password": "brand-password123",
	}, "")
	if contosoLoginRes.Code != http.StatusOK {
		t.Fatalf("expected contoso brand login 200, got %d: %s", contosoLoginRes.Code, contosoLoginRes.Body.String())
	}
	contosoLogin := decodeBody[brandCloudLoginBody](t, contosoLoginRes)
	contosoClaims, err := env.server.auth.ParseAccessToken(contosoLogin.Tokens.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if contosoClaims.UserID != contosoLogin.User.ID || contosoClaims.BrandCloudUserID == acmeClaims.BrandCloudUserID || contosoClaims.BrandCloudID != contoso.BrandCloud.ID || contosoClaims.TenantSlug != "contoso" {
		t.Fatalf("expected distinct contoso subject, acme=%+v contoso=%+v", acmeClaims, contosoClaims)
	}

	meRes := performJSON(env.router, http.MethodGet, "/v1/brand-clouds/acme/me", nil, acmeLogin.Tokens.AccessToken)
	if meRes.Code != http.StatusOK {
		t.Fatalf("expected brand me 200, got %d: %s", meRes.Code, meRes.Body.String())
	}
	wrongTenantMeRes := performJSON(env.router, http.MethodGet, "/v1/brand-clouds/contoso/me", nil, acmeLogin.Tokens.AccessToken)
	if wrongTenantMeRes.Code != http.StatusNotFound {
		t.Fatalf("expected cross-tenant brand me 404, got %d: %s", wrongTenantMeRes.Code, wrongTenantMeRes.Body.String())
	}

	listOwnDevicesRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+acme.BrandCloud.ID+"/devices", nil, acmeLogin.Tokens.AccessToken)
	if listOwnDevicesRes.Code != http.StatusOK {
		t.Fatalf("expected own brand org access 200, got %d: %s", listOwnDevicesRes.Code, listOwnDevicesRes.Body.String())
	}
	listOtherDevicesRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+contoso.BrandCloud.ID+"/devices", nil, acmeLogin.Tokens.AccessToken)
	if listOtherDevicesRes.Code != http.StatusNotFound {
		t.Fatalf("expected cross-brand org access 404, got %d: %s", listOtherDevicesRes.Code, listOtherDevicesRes.Body.String())
	}

	refreshRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/acme/auth/refresh", map[string]any{
		"refresh_token": acmeLogin.Tokens.RefreshToken,
	}, "")
	if refreshRes.Code != http.StatusOK {
		t.Fatalf("expected brand refresh 200, got %d: %s", refreshRes.Code, refreshRes.Body.String())
	}
	missingBrandRefreshRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/acme/auth/refresh", map[string]any{
		"refresh_token": "",
	}, "")
	if missingBrandRefreshRes.Code != http.StatusBadRequest {
		t.Fatalf("expected missing brand refresh token 400, got %d: %s", missingBrandRefreshRes.Code, missingBrandRefreshRes.Body.String())
	}
	tenantMismatchRefreshRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/contoso/auth/refresh", map[string]any{
		"refresh_token": acmeLogin.Tokens.RefreshToken,
	}, "")
	if tenantMismatchRefreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected tenant-mismatched brand refresh 401, got %d: %s", tenantMismatchRefreshRes.Code, tenantMismatchRefreshRes.Body.String())
	}
	platformRefreshRes := performJSON(env.router, http.MethodPost, "/v1/auth/refresh", map[string]any{
		"refresh_token": acmeLogin.Tokens.RefreshToken,
	}, "")
	if platformRefreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected platform refresh to reject brand token, got %d: %s", platformRefreshRes.Code, platformRefreshRes.Body.String())
	}
	refreshed := decodeBody[brandCloudLoginBody](t, refreshRes)
	wrongTenantLogoutRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/contoso/auth/logout", map[string]any{
		"refresh_token": refreshed.Tokens.RefreshToken,
	}, refreshed.Tokens.AccessToken)
	if wrongTenantLogoutRes.Code != http.StatusNotFound {
		t.Fatalf("expected cross-tenant brand logout 404, got %d: %s", wrongTenantLogoutRes.Code, wrongTenantLogoutRes.Body.String())
	}
	missingBrandLogoutRefreshRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/acme/auth/logout", map[string]any{
		"refresh_token": "",
	}, refreshed.Tokens.AccessToken)
	if missingBrandLogoutRefreshRes.Code != http.StatusBadRequest {
		t.Fatalf("expected missing brand logout refresh token 400, got %d: %s", missingBrandLogoutRefreshRes.Code, missingBrandLogoutRefreshRes.Body.String())
	}
	logoutRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/acme/auth/logout", map[string]any{
		"refresh_token": refreshed.Tokens.RefreshToken,
	}, refreshed.Tokens.AccessToken)
	if logoutRes.Code != http.StatusOK {
		t.Fatalf("expected brand logout 200, got %d: %s", logoutRes.Code, logoutRes.Body.String())
	}
	revokedRefreshRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/acme/auth/refresh", map[string]any{
		"refresh_token": refreshed.Tokens.RefreshToken,
	}, "")
	if revokedRefreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected revoked brand refresh 401, got %d: %s", revokedRefreshRes.Code, revokedRefreshRes.Body.String())
	}
}

func TestIntegrationAppEndUserLoginDoesNotCreateBrandLinkAndIssuesGlobalSubject(t *testing.T) {
	env := newIntegrationEnv(t)

	loginRes := performJSON(env.router, http.MethodPost, "/v1/app/end-users/auth/login", map[string]any{
		"email":    "consumer@example.com",
		"password": "consumer-password123",
	}, "")
	if loginRes.Code != http.StatusOK {
		t.Fatalf("expected app end-user login 200, got %d: %s", loginRes.Code, loginRes.Body.String())
	}
	login := decodeBody[endUserLoginBody](t, loginRes)
	if login.EndUser.ID == "" || login.EndUser.Email != "consumer@example.com" {
		t.Fatalf("unexpected end user login body: %+v", login)
	}
	if login.AppCertificate.Status != "csr_required" {
		t.Fatalf("app certificate status = %q", login.AppCertificate.Status)
	}
	claims, err := env.server.auth.ParseAccessToken(login.Tokens.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if claims.SubjectType != auth.SubjectTypeEndUser || claims.EndUserID != login.EndUser.ID || claims.Subject != "end_user:"+login.EndUser.ID || claims.UserID != "" {
		t.Fatalf("unexpected end-user claims: %+v", claims)
	}
	meRes := performJSON(env.router, http.MethodGet, "/v1/app/end-users/me", nil, login.Tokens.AccessToken)
	if meRes.Code != http.StatusOK {
		t.Fatalf("expected app end-user me 200, got %d: %s", meRes.Code, meRes.Body.String())
	}
	me := decodeBody[endUserMeBody](t, meRes)
	if me.EndUser.ID != login.EndUser.ID || me.EndUser.Email != "consumer@example.com" {
		t.Fatalf("unexpected end-user me body: %+v", me)
	}
	refreshRes := performJSON(env.router, http.MethodPost, "/v1/app/end-users/auth/refresh", map[string]any{
		"refresh_token": login.Tokens.RefreshToken,
	}, "")
	if refreshRes.Code != http.StatusOK {
		t.Fatalf("expected app end-user refresh 200, got %d: %s", refreshRes.Code, refreshRes.Body.String())
	}
	refreshed := decodeBody[tokenBody](t, refreshRes)
	logoutRes := performJSON(env.router, http.MethodPost, "/v1/app/end-users/auth/logout", map[string]any{
		"refresh_token": refreshed.Tokens.RefreshToken,
	}, refreshed.Tokens.AccessToken)
	if logoutRes.Code != http.StatusOK {
		t.Fatalf("expected app end-user logout 200, got %d: %s", logoutRes.Code, logoutRes.Body.String())
	}
	revokedRefreshRes := performJSON(env.router, http.MethodPost, "/v1/app/end-users/auth/refresh", map[string]any{
		"refresh_token": refreshed.Tokens.RefreshToken,
	}, "")
	if revokedRefreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected revoked app end-user refresh 401, got %d: %s", revokedRefreshRes.Code, revokedRefreshRes.Body.String())
	}
	missingRefreshRes := performJSON(env.router, http.MethodPost, "/v1/app/end-users/auth/refresh", map[string]any{
		"refresh_token": "",
	}, "")
	if missingRefreshRes.Code != http.StatusBadRequest {
		t.Fatalf("expected missing app end-user refresh token 400, got %d: %s", missingRefreshRes.Code, missingRefreshRes.Body.String())
	}
	missingLogoutRefreshRes := performJSON(env.router, http.MethodPost, "/v1/app/end-users/auth/logout", map[string]any{
		"refresh_token": "",
	}, login.Tokens.AccessToken)
	if missingLogoutRefreshRes.Code != http.StatusBadRequest {
		t.Fatalf("expected missing app end-user logout refresh token 400, got %d: %s", missingLogoutRefreshRes.Code, missingLogoutRefreshRes.Body.String())
	}
	wrongPasswordRes := performJSON(env.router, http.MethodPost, "/v1/app/end-users/auth/login", map[string]any{
		"email":    "consumer@example.com",
		"password": "wrong-password123",
	}, "")
	if wrongPasswordRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected wrong end-user password 401, got %d: %s", wrongPasswordRes.Code, wrongPasswordRes.Body.String())
	}
	platform := registerUser(t, env.router, "end-user-platform-token@example.com", "End User Platform Token Org")
	platformRefreshRes := performJSON(env.router, http.MethodPost, "/v1/app/end-users/auth/refresh", map[string]any{
		"refresh_token": platform.Tokens.RefreshToken,
	}, "")
	if platformRefreshRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected platform refresh token rejected by end-user refresh 401, got %d: %s", platformRefreshRes.Code, platformRefreshRes.Body.String())
	}
	platformMeRes := performJSON(env.router, http.MethodGet, "/v1/app/end-users/me", nil, platform.Tokens.AccessToken)
	if platformMeRes.Code != http.StatusNotFound {
		t.Fatalf("expected platform token rejected by end-user me 404, got %d: %s", platformMeRes.Code, platformMeRes.Body.String())
	}
	platformLogoutRes := performJSON(env.router, http.MethodPost, "/v1/app/end-users/auth/logout", map[string]any{
		"refresh_token": platform.Tokens.RefreshToken,
	}, platform.Tokens.AccessToken)
	if platformLogoutRes.Code != http.StatusNotFound {
		t.Fatalf("expected platform token rejected by end-user logout 404, got %d: %s", platformLogoutRes.Code, platformLogoutRes.Body.String())
	}
	platformResolveRes := performJSON(env.router, http.MethodPost, "/v1/app/devices/claim/resolve", map[string]any{
		"claim_token": "unused-platform-token",
		"device_name": "Unused Platform Device",
	}, platform.Tokens.AccessToken)
	if platformResolveRes.Code != http.StatusNotFound {
		t.Fatalf("expected platform token rejected by end-user claim resolve 404, got %d: %s", platformResolveRes.Code, platformResolveRes.Body.String())
	}
	missingClaimRes := performJSON(env.router, http.MethodPost, "/v1/app/devices/claim/resolve", map[string]any{
		"device_name": "Missing Claim Token",
	}, login.Tokens.AccessToken)
	if missingClaimRes.Code != http.StatusBadRequest {
		t.Fatalf("expected missing claim token 400, got %d: %s", missingClaimRes.Code, missingClaimRes.Body.String())
	}
	missingDeviceNameRes := performJSON(env.router, http.MethodPost, "/v1/app/devices/claim/resolve", map[string]any{
		"claim_token": "unused-missing-device-name",
		"device_name": "",
	}, login.Tokens.AccessToken)
	if missingDeviceNameRes.Code != http.StatusBadRequest {
		t.Fatalf("expected missing device name 400, got %d: %s", missingDeviceNameRes.Code, missingDeviceNameRes.Body.String())
	}
	invalidClaimRes := performJSON(env.router, http.MethodPost, "/v1/app/devices/claim/resolve", map[string]any{
		"claim_token": "invalid-end-user-claim",
		"device_name": "Invalid Claim Device",
	}, login.Tokens.AccessToken)
	if invalidClaimRes.Code != http.StatusNotFound {
		t.Fatalf("expected invalid claim token 404, got %d: %s", invalidClaimRes.Code, invalidClaimRes.Body.String())
	}
	var linkCount int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*)::int FROM brand_cloud_end_users`).Scan(&linkCount); err != nil {
		t.Fatal(err)
	}
	if linkCount != 0 {
		t.Fatalf("login should not create brand links, got %d", linkCount)
	}

	issuer := &fakeAppCertificateIssuer{}
	env.server.ConfigureAppCertificateIssuer(issuer)
	csrRes := performJSON(env.router, http.MethodPost, "/v1/app/end-users/auth/login", map[string]any{
		"email":       "consumer@example.com",
		"password":    "consumer-password123",
		"app_csr_pem": generateTestCSR(t, "app-end-user:"+login.EndUser.ID),
	}, "")
	if csrRes.Code != http.StatusOK {
		t.Fatalf("expected app end-user csr login 200, got %d: %s", csrRes.Code, csrRes.Body.String())
	}
	csrLogin := decodeBody[endUserLoginBody](t, csrRes)
	if csrLogin.AppCertificate.Status != "issued" || csrLogin.AppCertificate.Subject != "app-end-user:"+login.EndUser.ID {
		t.Fatalf("unexpected app certificate response: %+v", csrLogin.AppCertificate)
	}
}

func TestIntegrationAppEndUserClaimCreatesMultiBrandBindings(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	admin := registerUser(t, env.router, "end-user-platform-admin@example.com", "End User Platform Admin")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	brandA := createBrandCloudForTest(t, env, admin.Tokens.AccessToken, "Brand A", "brand-a")
	brandB := createBrandCloudForTest(t, env, admin.Tokens.AccessToken, "Brand B", "brand-b")

	loginRes := performJSON(env.router, http.MethodPost, "/v1/app/end-users/auth/login", map[string]any{
		"email":    "multi-brand-consumer@example.com",
		"password": "consumer-password123",
	}, "")
	if loginRes.Code != http.StatusOK {
		t.Fatalf("expected app end-user login 200, got %d: %s", loginRes.Code, loginRes.Body.String())
	}
	login := decodeBody[endUserLoginBody](t, loginRes)
	claims, err := env.server.auth.ParseAccessToken(login.Tokens.AccessToken)
	if err != nil {
		t.Fatal(err)
	}

	rawA := "app-end-user-brand-a-claim"
	if _, err := store.New(env.db).CreateDeviceClaimToken(ctx, store.DeviceClaimTokenCreateInput{
		OrganizationID:      &brandA.BrandCloud.ID,
		CreatedBy:           &admin.User.ID,
		TokenHash:           auth.HashToken(rawA),
		Category:            model.DeviceCategoryGeneric,
		VideoCloudDevid:     "video-brand-a-device",
		ActivityID:          "activity-brand-a",
		ClipPublicKey:       "clip-brand-a",
		ServiceOptions:      []string{"mqtt"},
		ExpiresAt:           time.Now().UTC().Add(time.Hour),
		Metadata:            map[string]any{"brand": "a"},
		DeviceItemProfileID: nil,
	}); err != nil {
		t.Fatal(err)
	}
	claimARes := performJSON(env.router, http.MethodPost, "/v1/app/devices/claim/resolve", map[string]any{
		"claim_token": rawA,
		"device_name": "Brand A Device",
	}, login.Tokens.AccessToken)
	if claimARes.Code != http.StatusCreated {
		t.Fatalf("expected app claim A 201, got %d: %s", claimARes.Code, claimARes.Body.String())
	}
	claimA := decodeBody[endUserClaimResolveResponse](t, claimARes)
	if claimA.BrandLink.BrandCloudID != brandA.BrandCloud.ID ||
		bytes.Contains(claimARes.Body.Bytes(), []byte("multi-brand-consumer@example.com")) {
		t.Fatalf("claim A leaked or mis-scoped the Brand projection: %+v", claimA.BrandLink)
	}

	rawB := "app-end-user-brand-b-claim"
	if _, err := store.New(env.db).CreateDeviceClaimToken(ctx, store.DeviceClaimTokenCreateInput{
		OrganizationID:  &brandB.BrandCloud.ID,
		CreatedBy:       &admin.User.ID,
		TokenHash:       auth.HashToken(rawB),
		Category:        model.DeviceCategoryGeneric,
		VideoCloudDevid: "video-brand-b-device",
		ActivityID:      "activity-brand-b",
		ClipPublicKey:   "clip-brand-b",
		ServiceOptions:  []string{"mqtt"},
		ExpiresAt:       time.Now().UTC().Add(time.Hour),
		Metadata:        map[string]any{"brand": "b"},
	}); err != nil {
		t.Fatal(err)
	}
	claimBRes := performJSON(env.router, http.MethodPost, "/v1/app/devices/claim/resolve", map[string]any{
		"claim_token": rawB,
		"device_name": "Brand B Device",
	}, login.Tokens.AccessToken)
	if claimBRes.Code != http.StatusCreated {
		t.Fatalf("expected app claim B 201, got %d: %s", claimBRes.Code, claimBRes.Body.String())
	}
	claimB := decodeBody[endUserClaimResolveResponse](t, claimBRes)
	if claimB.BrandLink.BrandCloudID != brandB.BrandCloud.ID ||
		bytes.Contains(claimBRes.Body.Bytes(), []byte(brandA.BrandCloud.ID)) ||
		bytes.Contains(claimBRes.Body.Bytes(), []byte("multi-brand-consumer@example.com")) {
		t.Fatalf("claim B leaked or mis-scoped the Brand projection: %+v", claimB.BrandLink)
	}

	var brandLinks int
	if err := env.db.QueryRow(ctx, `
		SELECT count(*)::int
		FROM brand_cloud_end_users
		WHERE end_user_id = $1
	`, claims.EndUserID).Scan(&brandLinks); err != nil {
		t.Fatal(err)
	}
	if brandLinks != 2 {
		t.Fatalf("expected two brand links, got %d", brandLinks)
	}
	var bindings int
	if err := env.db.QueryRow(ctx, `
		SELECT count(*)::int
		FROM device_user_bindings
		WHERE end_user_id = $1
	`, claims.EndUserID).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if bindings != 2 {
		t.Fatalf("expected two device bindings, got %d", bindings)
	}
	env.server.ConfigureInternalAuthToken("end-user-internal-authz-token")
	endUserAllowedRes := performJSON(env.router, http.MethodPost, "/v1/internal/app-token-authorizations", map[string]any{
		"subject_type": "end_user",
		"end_user_id":  claims.EndUserID,
		"devid":        "video-brand-a-device",
	}, "end-user-internal-authz-token")
	if endUserAllowedRes.Code != http.StatusOK {
		t.Fatalf("expected end-user internal authorization 200, got %d: %s", endUserAllowedRes.Code, endUserAllowedRes.Body.String())
	}
	endUserForbiddenRes := performJSON(env.router, http.MethodPost, "/v1/internal/app-token-authorizations", map[string]any{
		"subject_type": "end_user",
		"end_user_id":  claims.EndUserID,
		"devid":        "missing-end-user-device",
	}, "end-user-internal-authz-token")
	if endUserForbiddenRes.Code != http.StatusForbidden {
		t.Fatalf("expected end-user missing device authorization 403, got %d: %s", endUserForbiddenRes.Code, endUserForbiddenRes.Body.String())
	}

	brandLoginRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/brand-a/auth/login", map[string]any{
		"email":    "brand-developer@example.com",
		"password": "brand-password123",
	}, "")
	if brandLoginRes.Code != http.StatusUnauthorized {
		t.Fatalf("brand developer should not exist yet; got %d: %s", brandLoginRes.Code, brandLoginRes.Body.String())
	}
}

func TestIntegrationACLAdminWorkflow(t *testing.T) {
	env := newIntegrationEnv(t)
	admin := registerUser(t, env.router, "acl-admin@example.com", "ACL Admin Org")
	if _, err := env.db.Exec(context.Background(), `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	tenant := registerUser(t, env.router, "acl-tenant@example.com", "ACL Tenant Org")

	nonAdminRes := performJSON(env.router, http.MethodGet, "/v1/admin/acl/permissions", nil, tenant.Tokens.AccessToken)
	if nonAdminRes.Code != http.StatusForbidden {
		t.Fatalf("expected non-platform-admin ACL permission list 403, got %d: %s", nonAdminRes.Code, nonAdminRes.Body.String())
	}

	permissionsRes := performJSON(env.router, http.MethodGet, "/v1/admin/acl/permissions", nil, admin.Tokens.AccessToken)
	if permissionsRes.Code != http.StatusOK {
		t.Fatalf("expected ACL permissions 200, got %d: %s", permissionsRes.Code, permissionsRes.Body.String())
	}
	for _, permission := range []string{store.PermissionChipsetProviderRead, store.PermissionChipsetProviderEdit, store.PermissionChipsetProviderPublish} {
		if !bytes.Contains(permissionsRes.Body.Bytes(), []byte(`"name":"`+permission+`"`)) {
			t.Fatalf("ACL permission catalog is missing %q: %s", permission, permissionsRes.Body.String())
		}
	}
	rolesRes := performJSON(env.router, http.MethodGet, "/v1/admin/acl/roles", nil, admin.Tokens.AccessToken)
	if rolesRes.Code != http.StatusOK {
		t.Fatalf("expected ACL roles 200, got %d: %s", rolesRes.Code, rolesRes.Body.String())
	}
	for path, label := range map[string]string{
		"/v1/orgs/" + tenant.Organization.ID + "/roles":       "customer ACL roles",
		"/v1/orgs/" + tenant.Organization.ID + "/permissions": "customer ACL permissions",
	} {
		res := performJSON(env.router, http.MethodGet, path, nil, tenant.Tokens.AccessToken)
		if res.Code != http.StatusOK {
			t.Fatalf("expected %s 200, got %d: %s", label, res.Code, res.Body.String())
		}
	}
	accessRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+tenant.Organization.ID+"/access/check?permission=organization.update&scope_type=organization&scope_id="+tenant.Organization.ID, nil, tenant.Tokens.AccessToken)
	if accessRes.Code != http.StatusNotFound {
		t.Fatalf("platform subject customer access check = %d: %s", accessRes.Code, accessRes.Body.String())
	}
	for name, target := range map[string]string{
		"missing scope":  "/v1/orgs/brand-cloud/access/check",
		"lookup failure": "/v1/orgs/brand-cloud/access/check?permission=organization.update&scope_type=organization&scope_id=brand-cloud",
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			requestContext, _ := gin.CreateTestContext(recorder)
			requestContext.Params = gin.Params{{Key: "orgId", Value: "brand-cloud"}}
			requestContext.Set("subjectType", auth.SubjectTypeBrandCloudUser)
			requestContext.Set("brandCloudID", "brand-cloud")
			requestContext.Set("brandCloudUserID", "brand-user")
			request := httptest.NewRequest(http.MethodGet, target, nil)
			if name == "lookup failure" {
				canceled, cancel := context.WithCancel(request.Context())
				cancel()
				request = request.WithContext(canceled)
			}
			requestContext.Request = request
			env.server.checkOrganizationAccess(requestContext)
			want := http.StatusBadRequest
			if name == "lookup failure" {
				want = http.StatusNotFound
			}
			if recorder.Code != want {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, want, recorder.Body.String())
			}
		})
	}

	roleName := fmt.Sprintf("custom_installer_%s", admin.User.ID[:8])
	roleRes := performJSON(env.router, http.MethodPost, "/v1/admin/acl/roles", map[string]any{
		"name":        roleName,
		"scope_type":  "organization",
		"description": "Custom installer",
	}, admin.Tokens.AccessToken)
	if roleRes.Code != http.StatusCreated {
		t.Fatalf("expected role create 201, got %d: %s", roleRes.Code, roleRes.Body.String())
	}
	getRoleRes := performJSON(env.router, http.MethodGet, "/v1/admin/acl/roles/"+roleName, nil, admin.Tokens.AccessToken)
	if getRoleRes.Code != http.StatusOK {
		t.Fatalf("expected role show 200, got %d: %s", getRoleRes.Code, getRoleRes.Body.String())
	}
	updateRoleRes := performJSON(env.router, http.MethodPatch, "/v1/admin/acl/roles/"+roleName, map[string]any{"description": "Updated custom installer"}, admin.Tokens.AccessToken)
	if updateRoleRes.Code != http.StatusOK {
		t.Fatalf("expected role update 200, got %d: %s", updateRoleRes.Code, updateRoleRes.Body.String())
	}
	bindRes := performJSON(env.router, http.MethodPost, "/v1/admin/acl/roles/"+roleName+"/permissions/claim.resolve", nil, admin.Tokens.AccessToken)
	if bindRes.Code != http.StatusOK {
		t.Fatalf("expected role permission bind 200, got %d: %s", bindRes.Code, bindRes.Body.String())
	}
	assignRes := performJSON(env.router, http.MethodPost, "/v1/admin/acl/role-assignments", map[string]any{
		"role_name":       "read_only_observer",
		"actor_id":        tenant.User.ID,
		"scope_type":      "organization",
		"organization_id": admin.Organization.ID,
	}, admin.Tokens.AccessToken)
	if assignRes.Code != http.StatusCreated {
		t.Fatalf("expected role assignment 201, got %d: %s", assignRes.Code, assignRes.Body.String())
	}
	assignment := decodeBody[aclRoleAssignmentBody](t, assignRes)
	assignmentsRes := performJSON(env.router, http.MethodGet, "/v1/admin/acl/role-assignments", nil, admin.Tokens.AccessToken)
	if assignmentsRes.Code != http.StatusOK {
		t.Fatalf("expected role assignment list 200, got %d: %s", assignmentsRes.Code, assignmentsRes.Body.String())
	}
	deleteAssignmentRes := performJSON(env.router, http.MethodDelete, "/v1/admin/acl/role-assignments/"+assignment.RoleAssignment.ID, nil, admin.Tokens.AccessToken)
	if deleteAssignmentRes.Code != http.StatusOK {
		t.Fatalf("expected role assignment delete 200, got %d: %s", deleteAssignmentRes.Code, deleteAssignmentRes.Body.String())
	}
	mappingRes := performJSON(env.router, http.MethodPost, "/v1/admin/acl/external-group-mappings", map[string]any{
		"provider_id":     "keycloak",
		"external_group":  "/installers",
		"role_name":       "installer",
		"scope_type":      "organization",
		"organization_id": admin.Organization.ID,
	}, admin.Tokens.AccessToken)
	if mappingRes.Code != http.StatusCreated {
		t.Fatalf("expected external group mapping 201, got %d: %s", mappingRes.Code, mappingRes.Body.String())
	}
	mapping := decodeBody[aclExternalGroupMappingBody](t, mappingRes)
	mappingsRes := performJSON(env.router, http.MethodGet, "/v1/admin/acl/external-group-mappings", nil, admin.Tokens.AccessToken)
	if mappingsRes.Code != http.StatusOK {
		t.Fatalf("expected external group mapping list 200, got %d: %s", mappingsRes.Code, mappingsRes.Body.String())
	}
	deleteMappingRes := performJSON(env.router, http.MethodDelete, "/v1/admin/acl/external-group-mappings/"+mapping.ExternalGroupMapping.ID, nil, admin.Tokens.AccessToken)
	if deleteMappingRes.Code != http.StatusOK {
		t.Fatalf("expected external group mapping delete 200, got %d: %s", deleteMappingRes.Code, deleteMappingRes.Body.String())
	}
	deleteRoleRes := performJSON(env.router, http.MethodDelete, "/v1/admin/acl/roles/"+roleName, nil, admin.Tokens.AccessToken)
	if deleteRoleRes.Code != http.StatusOK {
		t.Fatalf("expected role delete 200, got %d: %s", deleteRoleRes.Code, deleteRoleRes.Body.String())
	}
	missingRoleRes := performJSON(env.router, http.MethodGet, "/v1/admin/acl/roles/"+roleName, nil, admin.Tokens.AccessToken)
	if missingRoleRes.Code != http.StatusNotFound {
		t.Fatalf("expected deleted role show 404, got %d: %s", missingRoleRes.Code, missingRoleRes.Body.String())
	}
	missingRoleUpdateRes := performJSON(env.router, http.MethodPatch, "/v1/admin/acl/roles/"+roleName, map[string]any{"description": "again"}, admin.Tokens.AccessToken)
	if missingRoleUpdateRes.Code != http.StatusNotFound {
		t.Fatalf("expected deleted role update 404, got %d: %s", missingRoleUpdateRes.Code, missingRoleUpdateRes.Body.String())
	}
	missingRoleDeleteRes := performJSON(env.router, http.MethodDelete, "/v1/admin/acl/roles/"+roleName, nil, admin.Tokens.AccessToken)
	if missingRoleDeleteRes.Code != http.StatusNotFound {
		t.Fatalf("expected deleted role delete 404, got %d: %s", missingRoleDeleteRes.Code, missingRoleDeleteRes.Body.String())
	}
	missingAssignmentDeleteRes := performJSON(env.router, http.MethodDelete, "/v1/admin/acl/role-assignments/"+assignment.RoleAssignment.ID, nil, admin.Tokens.AccessToken)
	if missingAssignmentDeleteRes.Code != http.StatusNotFound {
		t.Fatalf("expected deleted role assignment delete 404, got %d: %s", missingAssignmentDeleteRes.Code, missingAssignmentDeleteRes.Body.String())
	}
	missingMappingDeleteRes := performJSON(env.router, http.MethodDelete, "/v1/admin/acl/external-group-mappings/"+mapping.ExternalGroupMapping.ID, nil, admin.Tokens.AccessToken)
	if missingMappingDeleteRes.Code != http.StatusNotFound {
		t.Fatalf("expected deleted external group mapping delete 404, got %d: %s", missingMappingDeleteRes.Code, missingMappingDeleteRes.Body.String())
	}
	auditRes := performJSON(env.router, http.MethodGet, "/v1/admin/acl/audit-events?event_type=external_group_mapping_created", nil, admin.Tokens.AccessToken)
	if auditRes.Code != http.StatusOK {
		t.Fatalf("expected ACL audit list 200, got %d: %s", auditRes.Code, auditRes.Body.String())
	}

	badRoleRes := performJSON(env.router, http.MethodPost, "/v1/admin/acl/roles", map[string]any{
		"name":       " ",
		"scope_type": "organization",
	}, admin.Tokens.AccessToken)
	if badRoleRes.Code != http.StatusBadRequest {
		t.Fatalf("expected blank ACL role 400, got %d: %s", badRoleRes.Code, badRoleRes.Body.String())
	}
	missingPermissionRes := performJSON(env.router, http.MethodPost, "/v1/admin/acl/roles/"+roleName+"/permissions/missing.permission", nil, admin.Tokens.AccessToken)
	if missingPermissionRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing permission 404, got %d: %s", missingPermissionRes.Code, missingPermissionRes.Body.String())
	}
	badAssignmentRes := performJSON(env.router, http.MethodPost, "/v1/admin/acl/role-assignments", map[string]any{
		"role_name":       "missing-role",
		"actor_id":        tenant.User.ID,
		"scope_type":      "organization",
		"scope_id":        " ",
		"organization_id": admin.Organization.ID,
	}, admin.Tokens.AccessToken)
	if badAssignmentRes.Code != http.StatusNotFound {
		t.Fatalf("expected missing role assignment 404, got %d: %s", badAssignmentRes.Code, badAssignmentRes.Body.String())
	}
	badMappingRes := performJSON(env.router, http.MethodPost, "/v1/admin/acl/external-group-mappings", map[string]any{
		"provider_id":    "keycloak",
		"external_group": " ",
		"role_name":      "installer",
		"scope_type":     "organization",
	}, admin.Tokens.AccessToken)
	if badMappingRes.Code != http.StatusBadRequest {
		t.Fatalf("expected blank external group mapping 400, got %d: %s", badMappingRes.Code, badMappingRes.Body.String())
	}
}

func TestIntegrationBrandCloudScopedRoleAssignmentWorkflow(t *testing.T) {
	env := newIntegrationEnv(t)
	admin := registerUser(t, env.router, "scoped-acl-platform-admin@example.com", "Scoped ACL Platform Admin")
	if _, err := env.db.Exec(context.Background(), `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	brand := createBrandCloudForTest(t, env, admin.Tokens.AccessToken, "Scoped ACL Brand", "scoped-acl-brand")
	userRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users", map[string]any{
		"email": "scoped-acl-operator@example.com", "password": "scoped-acl-password123", "role": "admin",
	}, admin.Tokens.AccessToken)
	if userRes.Code != http.StatusCreated {
		t.Fatalf("expected brand user create 201, got %d: %s", userRes.Code, userRes.Body.String())
	}
	brandUser := decodeBody[brandCloudUserBody](t, userRes)
	loginRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/scoped-acl-brand/auth/login", map[string]any{
		"email": "scoped-acl-operator@example.com", "password": "scoped-acl-password123",
	}, "")
	if loginRes.Code != http.StatusOK {
		t.Fatalf("expected brand login 200, got %d: %s", loginRes.Code, loginRes.Body.String())
	}
	login := decodeBody[brandCloudLoginBody](t, loginRes)
	if res := performJSON(env.router, http.MethodGet, "/v1/developer/chipsets", nil, login.Tokens.AccessToken); res.Code != http.StatusForbidden {
		t.Fatalf("expected brand session developer chipset list 403, got %d: %s", res.Code, res.Body.String())
	}
	if res := performJSON(env.router, http.MethodGet, "/v1/developer/chipsets/missing", nil, login.Tokens.AccessToken); res.Code != http.StatusForbidden {
		t.Fatalf("expected brand session developer chipset detail 403, got %d: %s", res.Code, res.Body.String())
	}
	rolesRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+brand.BrandCloud.ID+"/roles", nil, login.Tokens.AccessToken)
	if rolesRes.Code != http.StatusOK || !bytes.Contains(rolesRes.Body.Bytes(), []byte("firmware_operator")) {
		t.Fatalf("expected customer ACL role catalog 200, got %d: %s", rolesRes.Code, rolesRes.Body.String())
	}
	permissionsRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+brand.BrandCloud.ID+"/permissions", nil, login.Tokens.AccessToken)
	if permissionsRes.Code != http.StatusOK || !bytes.Contains(permissionsRes.Body.Bytes(), []byte("registry_device.read")) {
		t.Fatalf("expected customer ACL permission catalog 200, got %d: %s", permissionsRes.Code, permissionsRes.Body.String())
	}
	missingScopeRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+brand.BrandCloud.ID+"/access/check", nil, login.Tokens.AccessToken)
	if missingScopeRes.Code != http.StatusBadRequest {
		t.Fatalf("expected incomplete access check 400, got %d: %s", missingScopeRes.Code, missingScopeRes.Body.String())
	}
	accessRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+brand.BrandCloud.ID+"/access/check?permission=registry_device.read&scope_type=organization&scope_id="+brand.BrandCloud.ID, nil, login.Tokens.AccessToken)
	if accessRes.Code != http.StatusOK || !bytes.Contains(accessRes.Body.Bytes(), []byte(`"allowed":true`)) {
		t.Fatalf("expected organization access check 200, got %d: %s", accessRes.Code, accessRes.Body.String())
	}
	listRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+brand.BrandCloud.ID+"/role-assignments", nil, login.Tokens.AccessToken)
	if listRes.Code != http.StatusOK {
		t.Fatalf("expected scoped assignment list 200, got %d: %s", listRes.Code, listRes.Body.String())
	}
	assignmentRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+brand.BrandCloud.ID+"/role-assignments", map[string]any{
		"role_name": "firmware_operator", "actor_id": brandUser.BrandCloudUser.ID, "scope_type": "sku", "scope_id": "sku-camera-pro",
	}, login.Tokens.AccessToken)
	if assignmentRes.Code != http.StatusCreated {
		t.Fatalf("expected scoped assignment create 201, got %d: %s", assignmentRes.Code, assignmentRes.Body.String())
	}
	assignment := decodeBody[aclRoleAssignmentBody](t, assignmentRes)
	if assignment.RoleAssignment.ScopeType != "sku" || assignment.RoleAssignment.ScopeID == nil || *assignment.RoleAssignment.ScopeID != "sku-camera-pro" {
		t.Fatalf("unexpected scoped assignment: %+v", assignment.RoleAssignment)
	}
	deleteRes := performJSON(env.router, http.MethodDelete, "/v1/orgs/"+brand.BrandCloud.ID+"/role-assignments/"+assignment.RoleAssignment.ID, nil, login.Tokens.AccessToken)
	if deleteRes.Code != http.StatusNoContent {
		t.Fatalf("expected scoped assignment delete 204, got %d: %s", deleteRes.Code, deleteRes.Body.String())
	}
}

func TestIntegrationBrandCloudResourceScopeFiltersFleetQueries(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()
	admin := registerUser(t, env.router, "resource-scope-platform-admin@example.com", "Resource Scope Platform Admin")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	brand := createBrandCloudForTest(t, env, admin.Tokens.AccessToken, "Resource Scope Brand", "resource-scope-brand")
	ownerRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users", map[string]any{
		"email": "resource-scope-owner@example.com", "password": "resource-scope-owner123", "role": "admin",
	}, admin.Tokens.AccessToken)
	if ownerRes.Code != http.StatusCreated {
		t.Fatalf("expected owner create 201, got %d: %s", ownerRes.Code, ownerRes.Body.String())
	}
	ownerLoginRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/resource-scope-brand/auth/login", map[string]any{
		"email": "resource-scope-owner@example.com", "password": "resource-scope-owner123",
	}, "")
	if ownerLoginRes.Code != http.StatusOK {
		t.Fatalf("expected owner login 200, got %d: %s", ownerLoginRes.Code, ownerLoginRes.Body.String())
	}
	ownerLogin := decodeBody[brandCloudLoginBody](t, ownerLoginRes)
	createDevice := func(name string) deviceBody {
		res := performJSON(env.router, http.MethodPost, "/v1/orgs/"+brand.BrandCloud.ID+"/devices", map[string]any{"name": name, "category": "generic"}, ownerLogin.Tokens.AccessToken)
		if res.Code != http.StatusCreated {
			t.Fatalf("expected device create 201, got %d: %s", res.Code, res.Body.String())
		}
		return decodeBody[deviceBody](t, res)
	}
	scopedDevice := createDevice("Scoped Device")
	otherDevice := createDevice("Other Device")
	memberRes := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds/"+brand.BrandCloud.ID+"/users", map[string]any{
		"email": "resource-scope-member@example.com", "password": "resource-scope-member123", "role": "member",
	}, admin.Tokens.AccessToken)
	if memberRes.Code != http.StatusCreated {
		t.Fatalf("expected member create 201, got %d: %s", memberRes.Code, memberRes.Body.String())
	}
	member := decodeBody[brandCloudUserBody](t, memberRes)
	if _, err := env.db.Exec(ctx, `UPDATE role_assignments SET disabled_at = now() WHERE actor_type = 'brand_cloud_user' AND actor_id = $1 AND scope_type = 'organization'`, member.BrandCloudUser.ID); err != nil {
		t.Fatal(err)
	}
	scopeID := scopedDevice.Device.ID
	if _, err := store.New(env.db).CreateRoleAssignment(ctx, store.RoleAssignmentCreateInput{RoleName: "read_only_observer", ActorType: store.ActorTypeBrandCloudUser, ActorID: member.BrandCloudUser.ID, ScopeType: store.ScopeTypeDevice, ScopeID: &scopeID, OrganizationID: &brand.BrandCloud.ID}); err != nil {
		t.Fatal(err)
	}
	memberLoginRes := performJSON(env.router, http.MethodPost, "/v1/brand-clouds/resource-scope-brand/auth/login", map[string]any{
		"email": "resource-scope-member@example.com", "password": "resource-scope-member123",
	}, "")
	if memberLoginRes.Code != http.StatusOK {
		t.Fatalf("expected member login 200, got %d: %s", memberLoginRes.Code, memberLoginRes.Body.String())
	}
	memberLogin := decodeBody[brandCloudLoginBody](t, memberLoginRes)
	scopedDeviceRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+brand.BrandCloud.ID+"/devices/"+scopedDevice.Device.ID, nil, memberLogin.Tokens.AccessToken)
	if scopedDeviceRes.Code != http.StatusOK {
		t.Fatalf("expected scoped device read 200, got %d: %s", scopedDeviceRes.Code, scopedDeviceRes.Body.String())
	}
	otherDeviceRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+brand.BrandCloud.ID+"/devices/"+otherDevice.Device.ID, nil, memberLogin.Tokens.AccessToken)
	if otherDeviceRes.Code != http.StatusForbidden {
		t.Fatalf("expected out-of-scope device read 403, got %d: %s", otherDeviceRes.Code, otherDeviceRes.Body.String())
	}
	deviceTagsRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+brand.BrandCloud.ID+"/devices/"+scopedDevice.Device.ID+"/tags", nil, memberLogin.Tokens.AccessToken)
	if deviceTagsRes.Code != http.StatusOK {
		t.Fatalf("expected scoped device tags read 200, got %d: %s", deviceTagsRes.Code, deviceTagsRes.Body.String())
	}
	fleetRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+brand.BrandCloud.ID+"/fleet/devices?limit=100", nil, memberLogin.Tokens.AccessToken)
	if fleetRes.Code != http.StatusOK {
		t.Fatalf("expected scoped fleet query 200, got %d: %s", fleetRes.Code, fleetRes.Body.String())
	}
	fleet := decodeBody[struct {
		Devices []struct {
			ID string `json:"id"`
		} `json:"devices"`
	}](t, fleetRes)
	if len(fleet.Devices) != 1 || fleet.Devices[0].ID != scopedDevice.Device.ID || fleet.Devices[0].ID == otherDevice.Device.ID {
		t.Fatalf("resource scope leaked devices: %+v", fleet.Devices)
	}
	summaryRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+brand.BrandCloud.ID+"/fleet/summary", nil, memberLogin.Tokens.AccessToken)
	if summaryRes.Code != http.StatusOK || !bytes.Contains(summaryRes.Body.Bytes(), []byte(`"total":1`)) {
		t.Fatalf("expected scoped fleet summary 200 with one device, got %d: %s", summaryRes.Code, summaryRes.Body.String())
	}
}

func TestIntegrationAdminMetricsIncludesLifecycleVisibility(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()
	admin := registerUser(t, env.router, "lifecycle-metrics-admin@example.com", "Lifecycle Metrics Admin Org")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	nonAdmin := registerUser(t, env.router, "lifecycle-metrics-user@example.com", "Lifecycle Metrics User Org")

	deviceRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+admin.Organization.ID+"/devices", devicePayload("metrics-device", "METRICS-DEVICE-1"), admin.Tokens.AccessToken)
	if deviceRes.Code != http.StatusCreated {
		t.Fatalf("expected device create 201, got %d: %s", deviceRes.Code, deviceRes.Body.String())
	}
	device := decodeBody[deviceBody](t, deviceRes)
	now := time.Now().UTC().Truncate(time.Microsecond)
	retryable := false
	s := store.New(env.db)
	pendingOperationID := "api-metrics-op-pending-" + device.Device.ID
	pendingCorrelationID := "api-metrics-corr-pending-" + device.Device.ID
	deadOperationID := "api-metrics-op-dead-" + device.Device.ID
	deadCorrelationID := "api-metrics-corr-dead-" + device.Device.ID
	if _, _, err := s.CreateOrGetDeviceOperation(ctx, store.DeviceOperationCreateInput{
		OperationID:    pendingOperationID,
		CorrelationID:  pendingCorrelationID,
		OrganizationID: admin.Organization.ID,
		DeviceID:       device.Device.ID,
		OperationType:  model.DeviceOperationTypeProvision,
		Status:         model.DeviceOperationStatusPending,
		RequestedBy:    &admin.User.ID,
		RequestPayload: map[string]any{"video_cloud_devid": "api-video-metrics-1"},
		ResultPayload:  map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateOrGetDeviceOperation(ctx, store.DeviceOperationCreateInput{
		OperationID:    deadOperationID,
		CorrelationID:  deadCorrelationID,
		OrganizationID: admin.Organization.ID,
		DeviceID:       device.Device.ID,
		OperationType:  model.DeviceOperationTypeDeactivate,
		Status:         model.DeviceOperationStatusDeadLettered,
		RequestedBy:    &admin.User.ID,
		RequestPayload: map[string]any{"video_cloud_devid": "api-video-metrics-1"},
		ResultPayload:  map[string]any{},
		ErrorCode:      stringPtr("deactivate_dead_lettered"),
		ErrorMessage:   stringPtr("deactivate publish failed"),
		Retryable:      &retryable,
		CompletedAt:    &now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOutboxMessage(ctx, store.DeviceMessageOutboxCreateInput{
		MessageID:     "api-metrics-outbox-dead-" + device.Device.ID,
		OperationID:   deadOperationID,
		CorrelationID: deadCorrelationID,
		Stream:        "account.video.commands",
		MessageType:   "DeviceDeactivateRequested",
		SchemaVersion: "1.0",
		PartitionKey:  device.Device.ID,
		Payload:       map[string]any{"operation_id": deadOperationID},
		Status:        model.DeviceMessageOutboxStatusDeadLettered,
		AttemptCount:  3,
		LastError:     stringPtr("publish_failed"),
		AvailableAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateOrGetInboxMessage(ctx, store.DeviceMessageInboxCreateInput{
		MessageID:     "api-metrics-inbox-dead-" + device.Device.ID,
		OperationID:   deadOperationID,
		CorrelationID: deadCorrelationID,
		Stream:        "video.account.events",
		MessageType:   "DeviceDeactivateFailed",
		SchemaVersion: "1.0",
		PartitionKey:  device.Device.ID,
		Payload:       map[string]any{"operation_id": deadOperationID},
		Status:        model.DeviceMessageInboxStatusDeadLettered,
		AttemptCount:  3,
		LastError:     stringPtr("projection_failed"),
		ReceivedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}

	nonAdminMetricsRes := performJSON(env.router, http.MethodGet, "/v1/admin/metrics", nil, nonAdmin.Tokens.AccessToken)
	if nonAdminMetricsRes.Code != http.StatusForbidden {
		t.Fatalf("expected non-platform-admin metrics 403, got %d", nonAdminMetricsRes.Code)
	}
	metricsRes := performJSON(env.router, http.MethodGet, "/v1/admin/metrics", nil, admin.Tokens.AccessToken)
	if metricsRes.Code != http.StatusOK {
		t.Fatalf("expected admin metrics 200, got %d: %s", metricsRes.Code, metricsRes.Body.String())
	}
	metricsBody := decodeBody[evalTierMetricsBody](t, metricsRes)
	if metricsBody.Lifecycle.Outbox.ByStatus[string(model.DeviceMessageOutboxStatusDeadLettered)] != 1 {
		t.Fatalf("expected outbox dead-letter count, got %+v", metricsBody.Lifecycle.Outbox)
	}
	if len(metricsBody.Lifecycle.Outbox.DeadLetteredByError) != 1 ||
		metricsBody.Lifecycle.Outbox.DeadLetteredByError[0].MessageType != "DeviceDeactivateRequested" ||
		metricsBody.Lifecycle.Outbox.DeadLetteredByError[0].ErrorCode != "publish_failed" {
		t.Fatalf("unexpected outbox dead-letter breakdown: %+v", metricsBody.Lifecycle.Outbox.DeadLetteredByError)
	}
	if metricsBody.Lifecycle.Inbox.ByStatus[string(model.DeviceMessageInboxStatusDeadLettered)] != 1 {
		t.Fatalf("expected inbox dead-letter count, got %+v", metricsBody.Lifecycle.Inbox)
	}
	if metricsBody.Lifecycle.Operations.ByStatus[string(model.DeviceOperationStatusPending)] != 1 ||
		metricsBody.Lifecycle.Operations.ByStatus[string(model.DeviceOperationStatusDeadLettered)] != 1 {
		t.Fatalf("expected operation status counts, got %+v", metricsBody.Lifecycle.Operations.ByStatus)
	}
	if !hasLifecycleTypeStatus(metricsBody.Lifecycle.Operations.ByTypeAndStatus, string(model.DeviceOperationTypeDeactivate), string(model.DeviceOperationStatusDeadLettered), 1) {
		t.Fatalf("expected dead-lettered deactivate type/status count, got %+v", metricsBody.Lifecycle.Operations.ByTypeAndStatus)
	}
}

func TestIntegrationQuotaRaiseValidationAndDefaultApproval(t *testing.T) {
	env := newIntegrationEnv(t)

	registered := registerUser(t, env.router, "validate-quota@example.com", "Validate Quota Org")
	markEvaluationOrg(t, env, registered.Organization.ID, 5)
	orgID := registered.Organization.ID
	accessToken := registered.Tokens.AccessToken

	invalidRequests := []map[string]any{
		{"requested_quota": 0, "use_case": "pilot", "contact_info": map[string]any{"email": "buyer@example.com"}},
		{"requested_quota": 201, "use_case": "pilot", "contact_info": map[string]any{"email": "buyer@example.com"}},
		{"requested_quota": 8, "use_case": "   ", "contact_info": map[string]any{"email": "buyer@example.com"}},
		{"requested_quota": 8, "use_case": "pilot", "contact_info": map[string]any{}},
	}
	for i, body := range invalidRequests {
		res := performJSON(env.router, http.MethodPost, "/v1/orgs/"+orgID+"/quota-raise-requests", body, accessToken)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("expected invalid quota raise request %d to fail 400, got %d: %s", i, res.Code, res.Body.String())
		}
	}

	raiseReqRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+orgID+"/quota-raise-requests", map[string]any{
		"requested_quota": 8,
		"use_case":        "pilot expansion",
		"contact_info": map[string]any{
			"email": "buyer@example.com",
		},
	}, accessToken)
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
	declineReqRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+orgID+"/quota-raise-requests", map[string]any{
		"requested_quota": 12,
		"use_case":        "contract exit",
		"contact_info": map[string]any{
			"email": "buyer@example.com",
		},
	}, accessToken)
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
	organizationTagsRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/tags", nil, member.Tokens.AccessToken)
	if organizationTagsRes.Code != http.StatusOK || !bytes.Contains(organizationTagsRes.Body.Bytes(), []byte(`"tag":"lobby"`)) {
		t.Fatalf("expected organization tag list 200, got %d: %s", organizationTagsRes.Code, organizationTagsRes.Body.String())
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

func TestIntegrationFleetDeviceQueryAndSummaryAreServerSide(t *testing.T) {
	env := newIntegrationEnv(t)
	owner := registerUser(t, env.router, "fleet-query-owner@example.com", "Fleet Query Org")
	for _, serial := range []string{"FLEET-A", "FLEET-B"} {
		res := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload("fleet-"+serial, serial), owner.Tokens.AccessToken)
		if res.Code != http.StatusCreated {
			t.Fatalf("create device status = %d: %s", res.Code, res.Body.String())
		}
	}
	filtered := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/fleet/devices?q=FLEET-A&limit=100", nil, owner.Tokens.AccessToken)
	if filtered.Code != http.StatusOK {
		t.Fatalf("fleet query status = %d: %s", filtered.Code, filtered.Body.String())
	}
	var page struct {
		Devices []struct {
			ID string `json:"id"`
		} `json:"devices"`
		Pagination paginationBody `json:"pagination"`
		Query      map[string]any `json:"query"`
	}
	if err := json.Unmarshal(filtered.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Devices) != 1 || page.Pagination.Total != 1 || page.Query["server_side"] != true {
		t.Fatalf("unexpected fleet query response: %+v", page)
	}
	summary := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/fleet/summary", nil, owner.Tokens.AccessToken)
	if summary.Code != http.StatusOK || !strings.Contains(summary.Body.String(), `"total":2`) {
		t.Fatalf("unexpected fleet summary: %d %s", summary.Code, summary.Body.String())
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
		"service_options":   []string{"mqtt"},
		"operation_id":      "member-provision-op-1",
	}, member.Tokens.AccessToken)
	if memberProvisionRes.Code != http.StatusCreated {
		t.Fatalf("expected member provision 201, got %d: %s", memberProvisionRes.Code, memberProvisionRes.Body.String())
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
		"service_options":   []string{"video_streaming", "video_storage"},
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
		"service_options":   []string{"video_streaming", "video_storage"},
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
	if !equalStringSlices(provisionCommand.ServiceOptions, []string{"video_streaming", "video_storage"}) {
		t.Fatalf("unexpected provision command service options: %+v", provisionCommand.ServiceOptions)
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
	if memberState.Readiness.Failure != nil {
		t.Fatalf("pending provisioning must not return failure attribution, got %+v", memberState.Readiness.Failure)
	}

	if _, err := env.db.Exec(context.Background(), `
		UPDATE device_operations
		SET status = 'failed',
			error_code = 'video_activation_timeout',
			error_message = 'Video activation timed out',
			retryable = true,
			completed_at = now(),
			updated_at = now()
		WHERE operation_id = 'provision-op-1'
	`); err != nil {
		t.Fatal(err)
	}
	failedStateRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provisioning", nil, member.Tokens.AccessToken)
	if failedStateRes.Code != http.StatusOK {
		t.Fatalf("expected failed provisioning state 200, got %d: %s", failedStateRes.Code, failedStateRes.Body.String())
	}
	failedState := decodeBody[provisioningBody](t, failedStateRes)
	if failedState.Readiness.State != model.DeviceReadinessStateActivationFailed ||
		failedState.Readiness.Failure == nil ||
		failedState.Readiness.Failure.FailedLayer != "cloud_activation" ||
		failedState.Readiness.Failure.SourceState != string(model.DeviceOperationStatusFailed) ||
		!failedState.Readiness.Failure.Retryable ||
		failedState.Readiness.Failure.ErrorCode != "video_activation_timeout" ||
		failedState.Readiness.Failure.OperationID == nil ||
		*failedState.Readiness.Failure.OperationID != "provision-op-1" {
		t.Fatalf("expected cloud activation failure attribution, got %+v", failedState.Readiness)
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
		"service_options":   []string{"video_streaming", "video_storage"},
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
	fixtures := newAPIFixtureBuilder(t, env)

	owner := fixtures.register("claim-owner")
	admin := fixtures.register("claim-admin")
	member := fixtures.register("claim-member")
	otherOwner := fixtures.register("claim-other")
	fixtures.addMember(owner, admin, "admin")
	fixtures.addMember(owner, member, "member")

	seedClaimToken := func(rawToken, videoDevid string, expiresAt time.Time, orgID *string, category model.DeviceCategory) {
		t.Helper()
		if _, err := claims.CreateDeviceClaimToken(ctx, store.DeviceClaimTokenCreateInput{
			OrganizationID:  orgID,
			TokenHash:       auth.HashToken(rawToken),
			Category:        category,
			VideoCloudDevid: videoDevid,
			ActivityID:      "activity-" + videoDevid,
			ClipPublicKey:   "clip-key-" + videoDevid,
			ServiceOptions:  []string{"video_streaming", "video_storage"},
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
	if !equalStringSlices(claimBody.ProvisionInput.ServiceOptions, []string{"video_streaming", "video_storage"}) {
		t.Fatalf("unexpected provision service options: %+v", claimBody.ProvisionInput.ServiceOptions)
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
	if memberClaimRes.Code != http.StatusCreated {
		t.Fatalf("expected member claim resolve 201, got %d: %s", memberClaimRes.Code, memberClaimRes.Body.String())
	}

	invalidClaimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "missing-token",
		"device_name": "Missing Camera",
	}, owner.Tokens.AccessToken)
	assertErrorDetails(t, invalidClaimRes, http.StatusNotFound, "invalid_claim_token", false, "scan_or_enter_a_valid_claim_token")

	seedClaimToken("claim-token-expired", "claim-video-expired", time.Now().Add(-time.Hour), &ownerOrgID, model.DeviceCategoryIPCamera)
	expiredClaimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "claim-token-expired",
		"device_name": "Expired Camera",
	}, owner.Tokens.AccessToken)
	assertErrorDetails(t, expiredClaimRes, http.StatusBadRequest, "expired_claim_token", false, "request_new_claim_token")

	alreadyClaimedRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "claim-token-owner",
		"device_name": "Front Door Again",
	}, owner.Tokens.AccessToken)
	assertErrorDetails(t, alreadyClaimedRes, http.StatusConflict, "already_claimed", false, "use_existing_device_or_contact_support")

	seedClaimToken("claim-token-owner-duplicate-device", "claim-video-owner", time.Now().Add(time.Hour), &ownerOrgID, model.DeviceCategoryIPCamera)
	duplicateDeviceClaimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "claim-token-owner-duplicate-device",
		"device_name": "Front Door Duplicate Token",
	}, owner.Tokens.AccessToken)
	assertErrorDetails(t, duplicateDeviceClaimRes, http.StatusConflict, "already_claimed", false, "use_existing_device_or_contact_support")

	seedClaimToken("claim-token-cross-org", "claim-video-cross-org", time.Now().Add(time.Hour), &ownerOrgID, model.DeviceCategoryIPCamera)
	crossOrgClaimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+otherOwner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "claim-token-cross-org",
		"device_name": "Cross Org Camera",
	}, otherOwner.Tokens.AccessToken)
	assertErrorDetails(t, crossOrgClaimRes, http.StatusForbidden, "forbidden", false, "switch_organization_or_contact_support")

	seedClaimToken("claim-token-mqtt", "claim-video-mqtt", time.Now().Add(time.Hour), &ownerOrgID, model.DeviceCategoryMQTT)
	mqttClaimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "claim-token-mqtt",
		"device_name": "MQTT Device",
	}, owner.Tokens.AccessToken)
	if mqttClaimRes.Code != http.StatusCreated {
		t.Fatalf("expected mqtt claim resolve 201, got %d: %s", mqttClaimRes.Code, mqttClaimRes.Body.String())
	}
	mqttClaim := decodeBody[claimResolveBody](t, mqttClaimRes)
	if mqttClaim.Device.Category != model.DeviceCategoryMQTT {
		t.Fatalf("expected mqtt device category, got %+v", mqttClaim.Device)
	}
	if !equalStringSlices(mqttClaim.ProvisionInput.ServiceOptions, []string{"video_streaming", "video_storage"}) {
		t.Fatalf("unexpected mqtt provision service options: %+v", mqttClaim.ProvisionInput.ServiceOptions)
	}

	if _, err := env.db.Exec(ctx, `
		UPDATE organizations
		SET tier = 'evaluation', evaluation_device_quota = 1
		WHERE id = $1
	`, owner.Organization.ID); err != nil {
		t.Fatal(err)
	}
	seedClaimToken("claim-token-quota", "claim-video-quota", time.Now().Add(time.Hour), &ownerOrgID, model.DeviceCategoryIPCamera)
	quotaClaimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": "claim-token-quota",
		"device_name": "Quota Camera",
	}, owner.Tokens.AccessToken)
	assertErrorDetails(t, quotaClaimRes, http.StatusConflict, "EVALUATION_QUOTA_EXCEEDED", false, "request_quota_raise_or_contact_admin")
}

func TestIntegrationAuthorizationAndTenancyMatrix(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()
	fixtures := newAPIFixtureBuilder(t, env)

	owner := fixtures.register("matrix-owner")
	admin := fixtures.register("matrix-admin")
	member := fixtures.register("matrix-member")
	outsider := fixtures.register("matrix-outsider")
	platformAdmin := fixtures.register("matrix-platform-admin")
	disabled := fixtures.register("matrix-disabled")
	fixtures.addMember(owner, admin, "admin")
	fixtures.addMember(owner, member, "member")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, platformAdmin.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, disabled.User.ID); err != nil {
		t.Fatal(err)
	}

	quotaRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/quota-raise-requests", map[string]any{
		"requested_quota": 8,
		"use_case":        "matrix",
		"contact_info":    map[string]any{"email": "matrix@example.com"},
	}, owner.Tokens.AccessToken)
	if quotaRes.Code != http.StatusCreated {
		t.Fatalf("fixture quota raise request failed with %d: %s", quotaRes.Code, quotaRes.Body.String())
	}
	quotaBody := decodeBody[quotaRaiseRequestBody](t, quotaRes)

	projectedDevice := fixtures.createDevice(owner, "matrix-projected-device", "MATRIX-PROJECTED-001")
	if _, err := store.New(env.db).ProjectDevice(ctx, owner.Organization.ID, projectedDevice.Device.ID, store.ProvisionSucceededProjection(channel.DeviceProvisionSucceededPayload{
		OrgID:           owner.Organization.ID,
		AccountDeviceID: projectedDevice.Device.ID,
		VideoCloudDevid: "matrix-video-device",
		ActivityID:      "matrix-activity",
		ActivatedAt:     time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC),
	})); err != nil {
		t.Fatal(err)
	}

	type actor struct {
		name  string
		token string
	}
	actors := map[string]actor{
		"owner":          {name: "owner", token: owner.Tokens.AccessToken},
		"admin":          {name: "admin", token: admin.Tokens.AccessToken},
		"member":         {name: "member", token: member.Tokens.AccessToken},
		"outsider":       {name: "outsider", token: outsider.Tokens.AccessToken},
		"platform_admin": {name: "platform_admin", token: platformAdmin.Tokens.AccessToken},
		"disabled_user":  {name: "disabled_user", token: disabled.Tokens.AccessToken},
	}

	type matrixCase struct {
		name       string
		actor      string
		method     string
		path       string
		body       func(string) any
		wantStatus int
	}
	cases := []matrixCase{
		{name: "owner can list devices", actor: "owner", method: http.MethodGet, path: "/v1/orgs/" + owner.Organization.ID + "/devices", wantStatus: http.StatusOK},
		{name: "admin can list devices", actor: "admin", method: http.MethodGet, path: "/v1/orgs/" + owner.Organization.ID + "/devices", wantStatus: http.StatusOK},
		{name: "member can list devices", actor: "member", method: http.MethodGet, path: "/v1/orgs/" + owner.Organization.ID + "/devices", wantStatus: http.StatusOK},
		{name: "outsider cannot list org devices", actor: "outsider", method: http.MethodGet, path: "/v1/orgs/" + owner.Organization.ID + "/devices", wantStatus: http.StatusNotFound},
		{name: "disabled user cannot list own devices", actor: "disabled_user", method: http.MethodGet, path: "/v1/orgs/" + disabled.Organization.ID + "/devices", wantStatus: http.StatusUnauthorized},
		{name: "owner can create device", actor: "owner", method: http.MethodPost, path: "/v1/orgs/" + owner.Organization.ID + "/devices", body: func(s string) any { return devicePayload("matrix-owner-device-"+s, "MATRIX-OWNER-"+s) }, wantStatus: http.StatusCreated},
		{name: "admin can create device", actor: "admin", method: http.MethodPost, path: "/v1/orgs/" + owner.Organization.ID + "/devices", body: func(s string) any { return devicePayload("matrix-admin-device-"+s, "MATRIX-ADMIN-"+s) }, wantStatus: http.StatusCreated},
		{name: "member cannot create device", actor: "member", method: http.MethodPost, path: "/v1/orgs/" + owner.Organization.ID + "/devices", body: func(s string) any { return devicePayload("matrix-member-device-"+s, "MATRIX-MEMBER-"+s) }, wantStatus: http.StatusForbidden},
		{name: "outsider cannot create device in foreign org", actor: "outsider", method: http.MethodPost, path: "/v1/orgs/" + owner.Organization.ID + "/devices", body: func(s string) any { return devicePayload("matrix-outsider-device-"+s, "MATRIX-OUTSIDER-"+s) }, wantStatus: http.StatusNotFound},
		{name: "owner can resolve claim", actor: "owner", method: http.MethodPost, path: "/v1/orgs/" + owner.Organization.ID + "/devices/claim/resolve", body: fixtures.claimResolvePayload(owner.Organization.ID, "owner"), wantStatus: http.StatusCreated},
		{name: "admin can resolve claim", actor: "admin", method: http.MethodPost, path: "/v1/orgs/" + owner.Organization.ID + "/devices/claim/resolve", body: fixtures.claimResolvePayload(owner.Organization.ID, "admin"), wantStatus: http.StatusCreated},
		{name: "member can resolve claim", actor: "member", method: http.MethodPost, path: "/v1/orgs/" + owner.Organization.ID + "/devices/claim/resolve", body: fixtures.claimResolvePayload(owner.Organization.ID, "member"), wantStatus: http.StatusCreated},
		{name: "outsider cannot resolve claim in foreign org", actor: "outsider", method: http.MethodPost, path: "/v1/orgs/" + owner.Organization.ID + "/devices/claim/resolve", body: fixtures.claimResolvePayload(owner.Organization.ID, "outsider"), wantStatus: http.StatusNotFound},
		{name: "owner can start provision", actor: "owner", method: http.MethodPost, path: "/v1/orgs/" + owner.Organization.ID + "/devices/" + fixtures.createDevice(owner, "matrix-provision-owner", "MATRIX-PROVISION-OWNER").Device.ID + "/provision", body: lifecycleProvisionPayload("matrix-provision-owner"), wantStatus: http.StatusCreated},
		{name: "admin can start provision", actor: "admin", method: http.MethodPost, path: "/v1/orgs/" + owner.Organization.ID + "/devices/" + fixtures.createDevice(owner, "matrix-provision-admin", "MATRIX-PROVISION-ADMIN").Device.ID + "/provision", body: lifecycleProvisionPayload("matrix-provision-admin"), wantStatus: http.StatusCreated},
		{name: "member can start provision", actor: "member", method: http.MethodPost, path: "/v1/orgs/" + owner.Organization.ID + "/devices/" + fixtures.createDevice(owner, "matrix-provision-member", "MATRIX-PROVISION-MEMBER").Device.ID + "/provision", body: lifecycleProvisionPayload("matrix-provision-member"), wantStatus: http.StatusCreated},
		{name: "outsider cannot start provision in foreign org", actor: "outsider", method: http.MethodPost, path: "/v1/orgs/" + owner.Organization.ID + "/devices/" + fixtures.createDevice(owner, "matrix-provision-outsider", "MATRIX-PROVISION-OUTSIDER").Device.ID + "/provision", body: lifecycleProvisionPayload("matrix-provision-outsider"), wantStatus: http.StatusNotFound},
		{name: "owner can start deactivation", actor: "owner", method: http.MethodPost, path: "/v1/orgs/" + owner.Organization.ID + "/devices/" + projectedDevice.Device.ID + "/deactivate", body: lifecycleDeactivatePayload("matrix-deactivate-owner"), wantStatus: http.StatusCreated},
		{name: "member can start deactivation", actor: "member", method: http.MethodPost, path: "/v1/orgs/" + owner.Organization.ID + "/devices/" + projectedDevice.Device.ID + "/deactivate", body: lifecycleDeactivatePayload("matrix-deactivate-member"), wantStatus: http.StatusCreated},
		{name: "outsider cannot start deactivation in foreign org", actor: "outsider", method: http.MethodPost, path: "/v1/orgs/" + owner.Organization.ID + "/devices/" + projectedDevice.Device.ID + "/deactivate", body: lifecycleDeactivatePayload("matrix-deactivate-outsider"), wantStatus: http.StatusNotFound},
		{name: "platform admin can list claim tokens", actor: "platform_admin", method: http.MethodGet, path: "/v1/admin/device-claim-tokens", wantStatus: http.StatusOK},
		{name: "owner cannot list claim tokens as platform admin", actor: "owner", method: http.MethodGet, path: "/v1/admin/device-claim-tokens", wantStatus: http.StatusForbidden},
		{name: "platform admin can list quota requests", actor: "platform_admin", method: http.MethodGet, path: "/v1/admin/quota-raise-requests", wantStatus: http.StatusOK},
		{name: "member cannot list quota requests as platform admin", actor: "member", method: http.MethodGet, path: "/v1/admin/quota-raise-requests", wantStatus: http.StatusForbidden},
		{name: "platform admin can show quota request", actor: "platform_admin", method: http.MethodGet, path: "/v1/admin/quota-raise-requests/" + quotaBody.QuotaRaiseRequest.ID, wantStatus: http.StatusOK},
		{name: "platform admin can list audit events", actor: "platform_admin", method: http.MethodGet, path: "/v1/admin/audit-events", wantStatus: http.StatusOK},
		{name: "owner cannot list audit events", actor: "owner", method: http.MethodGet, path: "/v1/admin/audit-events", wantStatus: http.StatusForbidden},
	}

	for i, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			suffix := fmt.Sprintf("%02d", i)
			var body any
			if tc.body != nil {
				body = tc.body(suffix)
			}
			res := performJSON(env.router, tc.method, tc.path, body, actors[tc.actor].token)
			if res.Code != tc.wantStatus {
				t.Fatalf("%s as %s expected %d, got %d: %s", tc.name, actors[tc.actor].name, tc.wantStatus, res.Code, res.Body.String())
			}
			if tc.wantStatus == http.StatusNotFound && bytes.Contains(res.Body.Bytes(), []byte(projectedDevice.Device.ID)) {
				t.Fatalf("cross-tenant response leaked foreign device id: %s", res.Body.String())
			}
		})
	}
}

func TestIntegrationVideoCloudRuntimeScopeDoesNotGrantProductRole(t *testing.T) {
	env := newIntegrationEnv(t)
	runtimeToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"scope":      "admin",
		"subject_id": "device-1",
		"iat":        time.Now().Add(-time.Minute).Unix(),
		"exp":        time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte("video-cloud-runtime-secret"))
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"current product user": "/v1/me",
		"platform ACL":         "/v1/admin/acl/permissions",
	} {
		response := performJSON(env.router, http.MethodGet, path, nil, runtimeToken)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s accepted a video-cloud runtime scope as product authorization: status=%d body=%s", name, response.Code, response.Body.String())
		}
	}
}

func TestIntegrationAdminDeviceClaimTokenWorkflow(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	admin := registerUser(t, env.router, "claim-token-platform-admin@example.com", "Claim Token Admin Org")
	nonAdmin := registerUser(t, env.router, "claim-token-non-admin@example.com", "Claim Token Non Admin Org")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}

	createPayload := map[string]any{
		"organization_id":   nonAdmin.Organization.ID,
		"category":          "ip_camera",
		"video_cloud_devid": "admin-claim-video-1",
		"activity_id":       "admin-claim-activity-1",
		"clip_public_key":   "admin-claim-clip-key-1",
		"service_options":   []string{"video_streaming", "video_storage"},
		"expires_at":        time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
		"metadata":          map[string]any{"batch": "generated"},
		"notes":             "generated token",
	}
	nonAdminCreateRes := performJSON(env.router, http.MethodPost, "/v1/admin/device-claim-tokens", createPayload, nonAdmin.Tokens.AccessToken)
	if nonAdminCreateRes.Code != http.StatusForbidden {
		t.Fatalf("expected non-admin create 403, got %d: %s", nonAdminCreateRes.Code, nonAdminCreateRes.Body.String())
	}

	createRes := performJSON(env.router, http.MethodPost, "/v1/admin/device-claim-tokens", createPayload, admin.Tokens.AccessToken)
	if createRes.Code != http.StatusCreated {
		t.Fatalf("expected claim token create 201, got %d: %s", createRes.Code, createRes.Body.String())
	}
	generated := decodeBody[deviceClaimTokenAdminBody](t, createRes)
	if generated.DeviceClaimToken.ID == "" || generated.ClaimToken == nil || *generated.ClaimToken == "" {
		t.Fatalf("expected generated raw token once, got %+v", generated)
	}
	if generated.DeviceClaimToken.CreatedBy == nil || *generated.DeviceClaimToken.CreatedBy != admin.User.ID {
		t.Fatalf("expected created_by platform admin, got %+v", generated.DeviceClaimToken)
	}
	if !equalStringSlices(generated.DeviceClaimToken.ServiceOptions, []string{"video_streaming", "video_storage"}) {
		t.Fatalf("unexpected generated token service options: %+v", generated.DeviceClaimToken.ServiceOptions)
	}

	listRes := performJSON(env.router, http.MethodGet, "/v1/admin/device-claim-tokens", nil, admin.Tokens.AccessToken)
	if listRes.Code != http.StatusOK {
		t.Fatalf("expected list 200, got %d: %s", listRes.Code, listRes.Body.String())
	}
	listBody := decodeBody[deviceClaimTokensAdminBody](t, listRes)
	if listBody.Pagination.Total != 1 || len(listBody.DeviceClaimTokens) != 1 {
		t.Fatalf("expected one claim token in list, got %+v", listBody)
	}

	getRes := performJSON(env.router, http.MethodGet, "/v1/admin/device-claim-tokens/"+generated.DeviceClaimToken.ID, nil, admin.Tokens.AccessToken)
	if getRes.Code != http.StatusOK {
		t.Fatalf("expected get 200, got %d: %s", getRes.Code, getRes.Body.String())
	}
	got := decodeBody[deviceClaimTokenAdminBody](t, getRes)
	if got.ClaimToken != nil {
		t.Fatalf("raw generated token must not be returned after create, got %+v", got)
	}

	resolveGeneratedRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+nonAdmin.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": *generated.ClaimToken,
		"device_name": "Generated Camera",
	}, nonAdmin.Tokens.AccessToken)
	if resolveGeneratedRes.Code != http.StatusCreated {
		t.Fatalf("expected generated token resolve 201, got %d: %s", resolveGeneratedRes.Code, resolveGeneratedRes.Body.String())
	}

	importedRaw := "imported-claim-token-secret"
	importRes := performJSON(env.router, http.MethodPost, "/v1/admin/device-claim-tokens", map[string]any{
		"claim_token":       importedRaw,
		"category":          "ip_camera",
		"video_cloud_devid": "admin-claim-video-2",
		"activity_id":       "admin-claim-activity-2",
		"clip_public_key":   "admin-claim-clip-key-2",
		"service_options":   []string{"mqtt"},
		"expires_at":        time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
		"metadata":          map[string]any{"batch": "imported"},
	}, admin.Tokens.AccessToken)
	if importRes.Code != http.StatusCreated {
		t.Fatalf("expected imported claim token create 201, got %d: %s", importRes.Code, importRes.Body.String())
	}
	imported := decodeBody[deviceClaimTokenAdminBody](t, importRes)
	if imported.ClaimToken != nil {
		t.Fatalf("imported raw token must not be echoed, got %+v", imported)
	}
	if !equalStringSlices(imported.DeviceClaimToken.ServiceOptions, []string{"mqtt"}) {
		t.Fatalf("unexpected imported token service options: %+v", imported.DeviceClaimToken.ServiceOptions)
	}

	invalidServiceOptionsRes := performJSON(env.router, http.MethodPost, "/v1/admin/device-claim-tokens", map[string]any{
		"category":          "generic",
		"video_cloud_devid": "admin-claim-video-invalid",
		"activity_id":       "admin-claim-activity-invalid",
		"clip_public_key":   "admin-claim-clip-key-invalid",
		"service_options":   []string{"mqtt", "admin"},
		"expires_at":        time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}, admin.Tokens.AccessToken)
	if invalidServiceOptionsRes.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid service options 400, got %d: %s", invalidServiceOptionsRes.Code, invalidServiceOptionsRes.Body.String())
	}

	var rawTokenCount int
	if err := env.db.QueryRow(ctx, `
		SELECT count(*)::int
		FROM device_claim_tokens
		WHERE token_hash = $1 OR metadata::text LIKE '%' || $1 || '%' OR COALESCE(notes, '') LIKE '%' || $1 || '%'
	`, importedRaw).Scan(&rawTokenCount); err != nil {
		t.Fatal(err)
	}
	if rawTokenCount != 0 {
		t.Fatal("raw imported claim token was persisted")
	}

	revokeRes := performJSON(env.router, http.MethodPost, "/v1/admin/device-claim-tokens/"+imported.DeviceClaimToken.ID+"/revoke", nil, admin.Tokens.AccessToken)
	if revokeRes.Code != http.StatusOK {
		t.Fatalf("expected revoke 200, got %d: %s", revokeRes.Code, revokeRes.Body.String())
	}
	revoked := decodeBody[deviceClaimTokenAdminBody](t, revokeRes)
	if revoked.DeviceClaimToken.RevokedAt == nil {
		t.Fatalf("expected revoked_at, got %+v", revoked.DeviceClaimToken)
	}

	resolveRevokedRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+nonAdmin.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": importedRaw,
		"device_name": "Revoked Camera",
	}, nonAdmin.Tokens.AccessToken)
	assertErrorCode(t, resolveRevokedRes, http.StatusNotFound, "invalid_claim_token")
}

func TestIntegrationAdminIdentityProviderWorkflow(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	admin := registerUser(t, env.router, "idp-platform-admin@example.com", "IdP Admin Org")
	nonAdmin := registerUser(t, env.router, "idp-non-admin@example.com", "IdP Non Admin Org")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, admin.User.ID); err != nil {
		t.Fatal(err)
	}

	createPayload := map[string]any{
		"provider_id":       "keycloak",
		"name":              "Keycloak",
		"issuer_url":        "https://sso.example.test/realms/account",
		"client_id":         "rtk-account-manager",
		"client_secret_ref": "env:OIDC_CLIENT_SECRET",
		"scopes":            []string{"openid", "email", "profile"},
		"enabled":           true,
		"metadata":          map[string]any{"realm": "account"},
	}

	nonAdminCreateRes := performJSON(env.router, http.MethodPost, "/v1/admin/identity-providers", createPayload, nonAdmin.Tokens.AccessToken)
	if nonAdminCreateRes.Code != http.StatusForbidden {
		t.Fatalf("expected non-admin create 403, got %d: %s", nonAdminCreateRes.Code, nonAdminCreateRes.Body.String())
	}
	rawSecretPayload := map[string]any{
		"provider_id":       "raw-secret-provider",
		"name":              "Raw Secret Provider",
		"issuer_url":        "https://raw-secret.example.test/realms/account",
		"client_id":         "raw-client",
		"client_secret_ref": "super-secret-value",
	}
	rawSecretRes := performJSON(env.router, http.MethodPost, "/v1/admin/identity-providers", rawSecretPayload, admin.Tokens.AccessToken)
	assertErrorCode(t, rawSecretRes, http.StatusBadRequest, "invalid_client_secret_ref")

	createRes := performJSON(env.router, http.MethodPost, "/v1/admin/identity-providers", createPayload, admin.Tokens.AccessToken)
	if createRes.Code != http.StatusCreated {
		t.Fatalf("expected identity provider create 201, got %d: %s", createRes.Code, createRes.Body.String())
	}
	created := decodeBody[identityProviderAdminBody](t, createRes)
	if created.IdentityProvider.ProviderID != "keycloak" || created.IdentityProvider.ClientSecretRef == nil || *created.IdentityProvider.ClientSecretRef != "env:OIDC_CLIENT_SECRET" || !created.IdentityProvider.Enabled {
		t.Fatalf("unexpected created provider: %+v", created)
	}
	if bytes.Contains(createRes.Body.Bytes(), []byte("super-secret-value")) {
		t.Fatalf("raw secret leaked in create response: %s", createRes.Body.String())
	}

	createSecondEnabledRes := performJSON(env.router, http.MethodPost, "/v1/admin/identity-providers", map[string]any{
		"provider_id":       "second-enabled",
		"name":              "Second Enabled",
		"issuer_url":        "https://second.example.test/realms/account",
		"client_id":         "second-client",
		"client_secret_ref": "env:SECOND_OIDC_SECRET",
		"enabled":           true,
	}, admin.Tokens.AccessToken)
	assertErrorCode(t, createSecondEnabledRes, http.StatusConflict, "conflict")

	createSecondDisabledRes := performJSON(env.router, http.MethodPost, "/v1/admin/identity-providers", map[string]any{
		"provider_id":       "second-disabled",
		"name":              "Second Disabled",
		"issuer_url":        "https://second-disabled.example.test/realms/account",
		"client_id":         "second-disabled-client",
		"client_secret_ref": "env:SECOND_OIDC_SECRET",
		"enabled":           false,
	}, admin.Tokens.AccessToken)
	if createSecondDisabledRes.Code != http.StatusCreated {
		t.Fatalf("expected disabled second provider create 201, got %d: %s", createSecondDisabledRes.Code, createSecondDisabledRes.Body.String())
	}

	patchSecondEnabledRes := performJSON(env.router, http.MethodPatch, "/v1/admin/identity-providers/second-disabled", map[string]any{
		"enabled": true,
	}, admin.Tokens.AccessToken)
	assertErrorCode(t, patchSecondEnabledRes, http.StatusConflict, "conflict")

	listRes := performJSON(env.router, http.MethodGet, "/v1/admin/identity-providers?limit=1&offset=0", nil, admin.Tokens.AccessToken)
	if listRes.Code != http.StatusOK {
		t.Fatalf("expected identity provider list 200, got %d: %s", listRes.Code, listRes.Body.String())
	}
	listBody := decodeBody[identityProvidersAdminBody](t, listRes)
	if listBody.Pagination.Total != 2 || len(listBody.IdentityProviders) != 1 {
		t.Fatalf("unexpected provider list: %+v", listBody)
	}

	nonAdminListRes := performJSON(env.router, http.MethodGet, "/v1/admin/identity-providers", nil, nonAdmin.Tokens.AccessToken)
	if nonAdminListRes.Code != http.StatusForbidden {
		t.Fatalf("expected non-admin list 403, got %d: %s", nonAdminListRes.Code, nonAdminListRes.Body.String())
	}

	getRes := performJSON(env.router, http.MethodGet, "/v1/admin/identity-providers/keycloak", nil, admin.Tokens.AccessToken)
	if getRes.Code != http.StatusOK {
		t.Fatalf("expected identity provider get 200, got %d: %s", getRes.Code, getRes.Body.String())
	}

	patchRes := performJSON(env.router, http.MethodPatch, "/v1/admin/identity-providers/keycloak", map[string]any{
		"name":              "Keycloak Updated",
		"client_secret_ref": "env:OIDC_CLIENT_SECRET_ROTATED",
		"scopes":            []string{"openid", "email"},
		"metadata":          map[string]any{"realm": "account", "rotation": "planned"},
	}, admin.Tokens.AccessToken)
	if patchRes.Code != http.StatusOK {
		t.Fatalf("expected identity provider patch 200, got %d: %s", patchRes.Code, patchRes.Body.String())
	}
	patched := decodeBody[identityProviderAdminBody](t, patchRes)
	if patched.IdentityProvider.Name != "Keycloak Updated" || patched.IdentityProvider.ClientSecretRef == nil || *patched.IdentityProvider.ClientSecretRef != "env:OIDC_CLIENT_SECRET_ROTATED" || len(patched.IdentityProvider.Scopes) != 2 {
		t.Fatalf("unexpected patched provider: %+v", patched)
	}

	rawPatchRes := performJSON(env.router, http.MethodPatch, "/v1/admin/identity-providers/keycloak", map[string]any{
		"client_secret_ref": "raw-rotated-secret",
	}, admin.Tokens.AccessToken)
	assertErrorCode(t, rawPatchRes, http.StatusBadRequest, "invalid_client_secret_ref")

	deleteRes := performJSON(env.router, http.MethodDelete, "/v1/admin/identity-providers/keycloak", nil, admin.Tokens.AccessToken)
	if deleteRes.Code != http.StatusNoContent {
		t.Fatalf("expected identity provider delete 204, got %d: %s", deleteRes.Code, deleteRes.Body.String())
	}
	showDisabledRes := performJSON(env.router, http.MethodGet, "/v1/admin/identity-providers/keycloak", nil, admin.Tokens.AccessToken)
	if showDisabledRes.Code != http.StatusOK {
		t.Fatalf("expected disabled provider show 200, got %d: %s", showDisabledRes.Code, showDisabledRes.Body.String())
	}
	disabled := decodeBody[identityProviderAdminBody](t, showDisabledRes)
	if disabled.IdentityProvider.Enabled {
		t.Fatalf("expected delete to disable provider, got %+v", disabled)
	}

	enableSecondRes := performJSON(env.router, http.MethodPatch, "/v1/admin/identity-providers/second-disabled", map[string]any{
		"enabled": true,
	}, admin.Tokens.AccessToken)
	if enableSecondRes.Code != http.StatusOK {
		t.Fatalf("expected enabling second provider after disabling first to succeed, got %d: %s", enableSecondRes.Code, enableSecondRes.Body.String())
	}

	var rawSecretCount int
	if err := env.db.QueryRow(ctx, `
		SELECT count(*)::int
		FROM identity_providers
		WHERE client_secret_ref IN ('super-secret-value', 'raw-rotated-secret')
			OR metadata::text LIKE '%super-secret-value%'
			OR metadata::text LIKE '%raw-rotated-secret%'
	`).Scan(&rawSecretCount); err != nil {
		t.Fatal(err)
	}
	if rawSecretCount != 0 {
		t.Fatal("raw OIDC client secret was persisted")
	}

	events, err := store.New(env.db).ListAuditEvents(ctx, store.AuditEventListFilter{
		SubjectType: "identity_provider",
		Limit:       20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if events.Page.Total < 4 {
		t.Fatalf("expected identity provider audit events, got %+v", events)
	}
	if !hasAuditEvent(events.Events, "identity_provider_created") || !hasAuditEvent(events.Events, "identity_provider_updated") || !hasAuditEvent(events.Events, "identity_provider_disabled") {
		t.Fatalf("missing expected identity provider audit events: %+v", events.Events)
	}
}

func TestIntegrationAdminDeviceClaimOverrideWorkflow(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	platformAdmin := registerUser(t, env.router, "claim-override-platform-admin@example.com", "Claim Override Admin Org")
	source := registerUser(t, env.router, "claim-override-source@example.com", "Claim Override Source Org")
	target := registerUser(t, env.router, "claim-override-target@example.com", "Claim Override Target Org")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, platformAdmin.User.ID); err != nil {
		t.Fatal(err)
	}

	claims := store.New(env.db)
	transferRaw := "claim-override-transfer-raw"
	transferToken, err := claims.CreateDeviceClaimToken(ctx, store.DeviceClaimTokenCreateInput{
		OrganizationID:  &source.Organization.ID,
		TokenHash:       auth.HashToken(transferRaw),
		Category:        model.DeviceCategoryIPCamera,
		VideoCloudDevid: "claim-override-transfer-video",
		ActivityID:      "claim-override-transfer-activity",
		ClipPublicKey:   "claim-override-transfer-clip-key",
		ServiceOptions:  []string{"video_streaming", "video_storage"},
		ExpiresAt:       time.Now().UTC().Add(time.Hour),
		Now:             time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	transferResolveRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+source.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": transferRaw,
		"device_name": "Claim Transfer API Camera",
	}, source.Tokens.AccessToken)
	if transferResolveRes.Code != http.StatusCreated {
		t.Fatalf("expected transfer seed resolve 201, got %d: %s", transferResolveRes.Code, transferResolveRes.Body.String())
	}
	transferClaim := decodeBody[claimResolveBody](t, transferResolveRes)

	transferPayload := map[string]any{
		"target_organization_id": target.Organization.ID,
		"reason":                 "support verified transfer",
		"evidence":               map[string]any{"ticket": "SUP-API-131"},
	}
	nonAdminTransferRes := performJSON(env.router, http.MethodPost, "/v1/admin/device-claims/"+transferClaim.ClaimID+"/transfer", transferPayload, source.Tokens.AccessToken)
	if nonAdminTransferRes.Code != http.StatusForbidden {
		t.Fatalf("expected non-admin transfer 403, got %d: %s", nonAdminTransferRes.Code, nonAdminTransferRes.Body.String())
	}
	missingEvidenceTransferRes := performJSON(env.router, http.MethodPost, "/v1/admin/device-claims/"+transferClaim.ClaimID+"/transfer", map[string]any{
		"target_organization_id": target.Organization.ID,
		"reason":                 "missing evidence",
		"evidence":               map[string]any{},
	}, platformAdmin.Tokens.AccessToken)
	assertErrorCode(t, missingEvidenceTransferRes, http.StatusBadRequest, "operator_evidence_required")

	transferRes := performJSON(env.router, http.MethodPost, "/v1/admin/device-claims/"+transferClaim.ClaimID+"/transfer", transferPayload, platformAdmin.Tokens.AccessToken)
	if transferRes.Code != http.StatusOK {
		t.Fatalf("expected transfer 200, got %d: %s", transferRes.Code, transferRes.Body.String())
	}
	transferred := decodeBody[deviceClaimOverrideAdminBody](t, transferRes)
	if transferred.DeviceClaim.Status != "transferred" || transferred.DeviceClaim.OrganizationID != target.Organization.ID {
		t.Fatalf("expected transferred claim in target org, got %+v", transferred)
	}
	if transferred.DeviceClaimToken.ID != transferToken.ID || transferred.DeviceClaimToken.OrganizationID == nil || *transferred.DeviceClaimToken.OrganizationID != target.Organization.ID {
		t.Fatalf("expected transferred token in target org, got %+v", transferred.DeviceClaimToken)
	}

	transferResolveAgainRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+target.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": transferRaw,
		"device_name": "Claim Transfer Again",
	}, target.Tokens.AccessToken)
	assertErrorCode(t, transferResolveAgainRes, http.StatusConflict, "already_claimed")

	reclaimRaw := "claim-override-reclaim-raw"
	reclaimToken, err := claims.CreateDeviceClaimToken(ctx, store.DeviceClaimTokenCreateInput{
		OrganizationID:  &source.Organization.ID,
		TokenHash:       auth.HashToken(reclaimRaw),
		Category:        model.DeviceCategoryIPCamera,
		VideoCloudDevid: "claim-override-reclaim-video",
		ActivityID:      "claim-override-reclaim-activity",
		ClipPublicKey:   "claim-override-reclaim-clip-key",
		ServiceOptions:  []string{"video_streaming", "video_storage"},
		ExpiresAt:       time.Now().UTC().Add(time.Hour),
		Now:             time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	reclaimResolveRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+source.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": reclaimRaw,
		"device_name": "Claim Reclaim API Camera",
	}, source.Tokens.AccessToken)
	if reclaimResolveRes.Code != http.StatusCreated {
		t.Fatalf("expected reclaim seed resolve 201, got %d: %s", reclaimResolveRes.Code, reclaimResolveRes.Body.String())
	}
	reclaimRes := performJSON(env.router, http.MethodPost, "/v1/admin/device-claim-tokens/"+reclaimToken.ID+"/reclaim", map[string]any{
		"target_organization_id": target.Organization.ID,
		"reason":                 "factory reset and support verified",
		"evidence":               map[string]any{"factory_reset": true, "ticket": "SUP-API-132"},
	}, platformAdmin.Tokens.AccessToken)
	if reclaimRes.Code != http.StatusOK {
		t.Fatalf("expected reclaim 200, got %d: %s", reclaimRes.Code, reclaimRes.Body.String())
	}
	reclaimed := decodeBody[deviceClaimOverrideAdminBody](t, reclaimRes)
	if reclaimed.DeviceClaim.Status != "reclaimed" || reclaimed.DeviceClaim.OrganizationID != target.Organization.ID {
		t.Fatalf("expected reclaimed claim in target org, got %+v", reclaimed)
	}

	auditRes := performJSON(env.router, http.MethodGet, "/v1/admin/audit-events?subject_type=device_claim", nil, platformAdmin.Tokens.AccessToken)
	if auditRes.Code != http.StatusOK {
		t.Fatalf("expected audit list 200, got %d: %s", auditRes.Code, auditRes.Body.String())
	}
	audit := decodeBody[auditEventsBody](t, auditRes)
	if audit.Pagination.Total != 2 {
		t.Fatalf("expected transfer and reclaim audit events, got %+v", audit)
	}
}

func TestIntegrationDeviceUserUnprovisionWorkflow(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	owner := registerUser(t, env.router, "unprovision-owner@example.com", "Unprovision Owner Org")
	member := registerUser(t, env.router, "unprovision-member@example.com", "Unprovision Member Org")
	outsider := registerUser(t, env.router, "unprovision-outsider@example.com", "Unprovision Outsider Org")
	fixtures := newAPIFixtureBuilder(t, env)
	fixtures.addMember(owner, member, "member")

	claims := store.New(env.db)
	rawToken := "unprovision-raw-claim-token"
	token, err := claims.CreateDeviceClaimToken(ctx, store.DeviceClaimTokenCreateInput{
		OrganizationID:  &owner.Organization.ID,
		TokenHash:       auth.HashToken(rawToken),
		Category:        model.DeviceCategoryMQTT,
		VideoCloudDevid: "unprovision-video-device",
		ActivityID:      "unprovision-activity",
		ClipPublicKey:   "unprovision-clip-key",
		ServiceOptions:  []string{"mqtt"},
		ExpiresAt:       time.Now().UTC().Add(time.Hour),
		Now:             time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

	resolveRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": rawToken,
		"device_name": "Resale MQTT Device",
	}, owner.Tokens.AccessToken)
	if resolveRes.Code != http.StatusCreated {
		t.Fatalf("expected claim resolve 201, got %d: %s", resolveRes.Code, resolveRes.Body.String())
	}
	resolved := decodeBody[claimResolveBody](t, resolveRes)

	outsiderRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+resolved.Device.ID+"/unprovision", map[string]any{
		"reason": "not my device",
	}, outsider.Tokens.AccessToken)
	if outsiderRes.Code != http.StatusNotFound {
		t.Fatalf("expected outsider unprovision 404, got %d: %s", outsiderRes.Code, outsiderRes.Body.String())
	}

	unprovisionRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+resolved.Device.ID+"/unprovision", map[string]any{
		"reason": "user resale",
	}, member.Tokens.AccessToken)
	if unprovisionRes.Code != http.StatusOK {
		t.Fatalf("expected member unprovision 200, got %d: %s", unprovisionRes.Code, unprovisionRes.Body.String())
	}
	unprovisioned := decodeBody[deviceUnprovisionTestBody](t, unprovisionRes)
	if unprovisioned.Unprovision.DeviceID != resolved.Device.ID ||
		unprovisioned.Unprovision.OrganizationID != owner.Organization.ID ||
		unprovisioned.Unprovision.VideoCloudDevid != "unprovision-video-device" ||
		unprovisioned.Unprovision.Status != "unprovisioned" {
		t.Fatalf("unexpected unprovision response: %+v", unprovisioned)
	}

	var operationID string
	var messageType string
	var partitionKey string
	var payload []byte
	if err := env.db.QueryRow(ctx, `
		SELECT o.operation_id, m.message_type, m.partition_key, m.payload
		FROM device_operations o
		JOIN device_message_outbox m ON m.operation_id = o.operation_id
		WHERE o.device_id = $1 AND o.operation_type = 'unprovision'
	`, resolved.Device.ID).Scan(&operationID, &messageType, &partitionKey, &payload); err != nil {
		t.Fatal(err)
	}
	if messageType != "DeviceUnprovisionRequested" {
		t.Fatalf("expected unprovision outbox message type, got %s", messageType)
	}
	if partitionKey != resolved.Device.ID {
		t.Fatalf("expected unprovision partition key %s, got %s", resolved.Device.ID, partitionKey)
	}
	var commandPayload map[string]any
	if err := json.Unmarshal(payload, &commandPayload); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"org_id", "account_device_id", "video_cloud_devid", "requested_by", "reason", "platform_override", "unprovisioned_at"} {
		if _, ok := commandPayload[field]; !ok {
			t.Fatalf("expected unprovision command payload field %q, got %+v", field, commandPayload)
		}
	}
	if commandPayload["video_cloud_devid"] != "unprovision-video-device" || commandPayload["reason"] != "user resale" {
		t.Fatalf("unexpected unprovision command payload: %+v", commandPayload)
	}
	unprovisionMessage, err := store.New(env.db).GetLatestOutboxMessageByOperationID(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	unprovisionPayload := validateAccountCommandEnvelope(t, unprovisionMessage)
	unprovisionCommand, ok := unprovisionPayload.(*channel.DeviceUnprovisionRequestedPayload)
	if !ok {
		t.Fatalf("expected unprovision payload type, got %T", unprovisionPayload)
	}
	if unprovisionCommand.VideoCloudDevid != "unprovision-video-device" ||
		unprovisionCommand.Reason != "user resale" ||
		unprovisionCommand.RequestedBy != member.User.ID ||
		unprovisionCommand.PlatformOverride {
		t.Fatalf("unexpected unprovision command payload: %+v", unprovisionCommand)
	}

	getOldRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices/"+resolved.Device.ID, nil, owner.Tokens.AccessToken)
	if getOldRes.Code != http.StatusNotFound {
		t.Fatalf("expected old device binding to be gone, got %d: %s", getOldRes.Code, getOldRes.Body.String())
	}
	deactivateOldRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/"+resolved.Device.ID+"/deactivate", map[string]any{
		"operation_id": "unprovision-old-deactivate",
	}, member.Tokens.AccessToken)
	if deactivateOldRes.Code != http.StatusNotFound {
		t.Fatalf("expected old device deactivate 404, got %d: %s", deactivateOldRes.Code, deactivateOldRes.Body.String())
	}
	resolveOldTokenRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": rawToken,
		"device_name": "Old Token Reuse",
	}, owner.Tokens.AccessToken)
	assertErrorCode(t, resolveOldTokenRes, http.StatusConflict, "already_claimed")

	var claimedAt *time.Time
	if err := env.db.QueryRow(ctx, `SELECT claimed_at FROM device_claim_tokens WHERE id = $1`, token.ID).Scan(&claimedAt); err != nil {
		t.Fatal(err)
	}
	if claimedAt == nil {
		t.Fatal("unprovision must not make the original one-time Claim Token reusable")
	}

	rawReplacementToken := "unprovision-replacement-raw-claim-token"
	if _, err := claims.CreateDeviceClaimToken(ctx, store.DeviceClaimTokenCreateInput{
		OrganizationID:  &owner.Organization.ID,
		TokenHash:       auth.HashToken(rawReplacementToken),
		Category:        model.DeviceCategoryMQTT,
		VideoCloudDevid: "unprovision-video-device",
		ActivityID:      "unprovision-activity-2",
		ClipPublicKey:   "unprovision-clip-key-2",
		ServiceOptions:  []string{"mqtt"},
		ExpiresAt:       time.Now().UTC().Add(time.Hour),
		Now:             time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	reclaimRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": rawReplacementToken,
		"device_name": "Reclaimed Resale MQTT Device",
	}, member.Tokens.AccessToken)
	if reclaimRes.Code != http.StatusCreated {
		t.Fatalf("expected replacement claim resolve 201 after unprovision, got %d: %s", reclaimRes.Code, reclaimRes.Body.String())
	}
	reclaimed := decodeBody[claimResolveBody](t, reclaimRes)
	if reclaimed.Device.ID == resolved.Device.ID || reclaimed.ProvisionInput.VideoCloudDevid != "unprovision-video-device" {
		t.Fatalf("expected new registry device for same factory identity, got %+v", reclaimed)
	}

	auditRes := performJSON(env.router, http.MethodGet, "/v1/admin/audit-events?subject_type=device", nil, owner.Tokens.AccessToken)
	if auditRes.Code != http.StatusForbidden {
		t.Fatalf("expected non-platform audit list 403, got %d", auditRes.Code)
	}
}

func TestIntegrationAdminDeviceUnprovisionOverride(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	platformAdmin := registerUser(t, env.router, "unprovision-platform-admin@example.com", "Unprovision Platform Admin Org")
	owner := registerUser(t, env.router, "unprovision-override-owner@example.com", "Unprovision Override Owner Org")
	nonAdmin := registerUser(t, env.router, "unprovision-override-non-admin@example.com", "Unprovision Override Non Admin Org")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, platformAdmin.User.ID); err != nil {
		t.Fatal(err)
	}

	rawToken := "unprovision-override-raw-claim-token"
	_, err := store.New(env.db).CreateDeviceClaimToken(ctx, store.DeviceClaimTokenCreateInput{
		OrganizationID:  &owner.Organization.ID,
		TokenHash:       auth.HashToken(rawToken),
		Category:        model.DeviceCategoryIPCamera,
		VideoCloudDevid: "unprovision-override-video-device",
		ActivityID:      "unprovision-override-activity",
		ClipPublicKey:   "unprovision-override-clip-key",
		ServiceOptions:  []string{"video_streaming", "video_storage"},
		ExpiresAt:       time.Now().UTC().Add(time.Hour),
		Now:             time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolveRes := performJSON(env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices/claim/resolve", map[string]any{
		"claim_token": rawToken,
		"device_name": "Override Camera",
	}, owner.Tokens.AccessToken)
	if resolveRes.Code != http.StatusCreated {
		t.Fatalf("expected override seed resolve 201, got %d: %s", resolveRes.Code, resolveRes.Body.String())
	}
	resolved := decodeBody[claimResolveBody](t, resolveRes)

	nonAdminRes := performJSON(env.router, http.MethodPost, "/v1/admin/devices/"+resolved.Device.ID+"/unprovision", map[string]any{
		"reason":   "support request",
		"evidence": map[string]any{"ticket": "SUP-UNPROVISION-1"},
	}, nonAdmin.Tokens.AccessToken)
	if nonAdminRes.Code != http.StatusForbidden {
		t.Fatalf("expected non-platform admin override 403, got %d: %s", nonAdminRes.Code, nonAdminRes.Body.String())
	}
	missingEvidenceRes := performJSON(env.router, http.MethodPost, "/v1/admin/devices/"+resolved.Device.ID+"/unprovision", map[string]any{
		"reason":   "support request",
		"evidence": map[string]any{},
	}, platformAdmin.Tokens.AccessToken)
	assertErrorCode(t, missingEvidenceRes, http.StatusBadRequest, "operator_evidence_required")

	overrideRes := performJSON(env.router, http.MethodPost, "/v1/admin/devices/"+resolved.Device.ID+"/unprovision", map[string]any{
		"reason":   "support verified resale release",
		"evidence": map[string]any{"ticket": "SUP-UNPROVISION-2"},
	}, platformAdmin.Tokens.AccessToken)
	if overrideRes.Code != http.StatusOK {
		t.Fatalf("expected platform admin override 200, got %d: %s", overrideRes.Code, overrideRes.Body.String())
	}
	override := decodeBody[deviceUnprovisionTestBody](t, overrideRes)
	if override.Unprovision.DeviceID != resolved.Device.ID ||
		override.Unprovision.OrganizationID != owner.Organization.ID ||
		override.Unprovision.Status != "unprovisioned" {
		t.Fatalf("unexpected override response: %+v", override)
	}
	overrideMessage, err := store.New(env.db).GetLatestOutboxMessageByOperationID(ctx, "unprovision-"+resolved.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	overridePayload := validateAccountCommandEnvelope(t, overrideMessage)
	overrideCommand, ok := overridePayload.(*channel.DeviceUnprovisionRequestedPayload)
	if !ok {
		t.Fatalf("expected override unprovision payload type, got %T", overridePayload)
	}
	if !overrideCommand.PlatformOverride || overrideCommand.RequestedBy != platformAdmin.User.ID || overrideCommand.Reason != "support verified resale release" {
		t.Fatalf("unexpected override unprovision command payload: %+v", overrideCommand)
	}

	auditRes := performJSON(env.router, http.MethodGet, "/v1/admin/audit-events?subject_type=device&event_type=device_unprovisioned", nil, platformAdmin.Tokens.AccessToken)
	if auditRes.Code != http.StatusOK {
		t.Fatalf("expected audit list 200, got %d: %s", auditRes.Code, auditRes.Body.String())
	}
	audit := decodeBody[auditEventsBody](t, auditRes)
	if audit.Pagination.Total != 1 || !hasAuditEventBody(audit.AuditEvents, "device_unprovisioned") {
		t.Fatalf("expected device_unprovisioned audit event, got %+v", audit)
	}
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
	if memberDeactivateRes.Code != http.StatusCreated {
		t.Fatalf("expected member deactivate 201, got %d: %s", memberDeactivateRes.Code, memberDeactivateRes.Body.String())
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

	if _, err := env.db.Exec(context.Background(), `
		UPDATE device_operations
		SET status = 'dead_lettered',
			error_code = 'deactivate_dead_lettered',
			error_message = 'Deactivate command exhausted retries',
			retryable = false,
			completed_at = now(),
			updated_at = now()
		WHERE operation_id = 'deactivate-op-1'
	`); err != nil {
		t.Fatal(err)
	}
	deactivateFailedStateRes := performJSON(env.router, http.MethodGet, "/v1/orgs/"+owner.Organization.ID+"/devices/"+device.Device.ID+"/provisioning", nil, admin.Tokens.AccessToken)
	if deactivateFailedStateRes.Code != http.StatusOK {
		t.Fatalf("expected failed deactivation state 200, got %d: %s", deactivateFailedStateRes.Code, deactivateFailedStateRes.Body.String())
	}
	deactivateFailedState := decodeBody[provisioningBody](t, deactivateFailedStateRes)
	if deactivateFailedState.Readiness.State != model.DeviceReadinessStateDeactivationFailed ||
		deactivateFailedState.Readiness.Failure == nil ||
		deactivateFailedState.Readiness.Failure.FailedLayer != "deactivation" ||
		deactivateFailedState.Readiness.Failure.SourceState != string(model.DeviceOperationStatusDeadLettered) ||
		deactivateFailedState.Readiness.Failure.Retryable ||
		deactivateFailedState.Readiness.Failure.ErrorCode != "deactivate_dead_lettered" ||
		deactivateFailedState.Readiness.Failure.OperationID == nil ||
		*deactivateFailedState.Readiness.Failure.OperationID != "deactivate-op-1" {
		t.Fatalf("expected deactivation failure attribution, got %+v", deactivateFailedState.Readiness)
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
		Email                     string `json:"email"`
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

type developerSignupBody struct {
	User struct {
		ID                        string `json:"id"`
		Email                     string `json:"email"`
		DeveloperCloudLimit       int    `json:"developer_cloud_limit"`
		EmailVerified             bool   `json:"email_verified"`
		SignupPendingVerification bool   `json:"signup_pending_verification"`
	} `json:"user"`
	BrandCloud struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		Role             string `json:"role"`
		TenantSlug       string `json:"tenant_slug"`
		OrganizationKind string `json:"organization_kind"`
	} `json:"brand_cloud"`
	Organization struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		Role             string `json:"role"`
		TenantSlug       string `json:"tenant_slug"`
		OrganizationKind string `json:"organization_kind"`
	} `json:"organization"`
}

type userBody struct {
	User struct {
		ID                        string     `json:"id"`
		EmailVerified             bool       `json:"email_verified"`
		SignupPendingVerification bool       `json:"signup_pending_verification"`
		EmailVerifiedAt           *time.Time `json:"email_verified_at"`
	} `json:"user"`
	Tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	} `json:"tokens"`
}

type tokenBody struct {
	User struct {
		EmailVerified             bool `json:"email_verified"`
		SignupPendingVerification bool `json:"signup_pending_verification"`
	} `json:"user"`
	Tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	} `json:"tokens"`
	AppCertificate struct {
		Status              string `json:"status"`
		Subject             string `json:"subject"`
		CertificatePEM      string `json:"certificate_pem"`
		CertificateChainPEM string `json:"certificate_chain_pem"`
		FingerprintSHA256   string `json:"fingerprint_sha256"`
		SerialNumber        string `json:"serial_number"`
		IssuerRequestID     string `json:"issuer_request_id"`
	} `json:"app_certificate"`
}

type endUserLoginBody struct {
	EndUser struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"end_user"`
	Tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	} `json:"tokens"`
	AppCertificate struct {
		Status              string `json:"status"`
		Subject             string `json:"subject"`
		CertificatePEM      string `json:"certificate_pem"`
		CertificateChainPEM string `json:"certificate_chain_pem"`
		FingerprintSHA256   string `json:"fingerprint_sha256"`
		SerialNumber        string `json:"serial_number"`
		IssuerRequestID     string `json:"issuer_request_id"`
	} `json:"app_certificate"`
}

type endUserMeBody struct {
	EndUser struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"end_user"`
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
		ID       string               `json:"id"`
		Name     string               `json:"name"`
		Category model.DeviceCategory `json:"category"`
	} `json:"device"`
	ProvisionInput struct {
		VideoCloudDevid string   `json:"video_cloud_devid"`
		ActivityID      string   `json:"activity_id"`
		ClipPublicKey   string   `json:"clip_public_key"`
		ServiceOptions  []string `json:"service_options"`
	} `json:"provision_input"`
}

type deviceClaimTokenAdminBody struct {
	DeviceClaimToken struct {
		ID                  string         `json:"id"`
		OrganizationID      *string        `json:"organization_id"`
		CreatedBy           *string        `json:"created_by"`
		DeviceItemProfileID *string        `json:"device_item_profile_id"`
		VideoCloudDevid     string         `json:"video_cloud_devid"`
		ServiceOptions      []string       `json:"service_options"`
		Metadata            map[string]any `json:"metadata"`
		RevokedAt           *time.Time     `json:"revoked_at"`
	} `json:"device_claim_token"`
	ClaimToken *string `json:"claim_token"`
}

type deviceClaimTokensAdminBody struct {
	DeviceClaimTokens []struct {
		ID string `json:"id"`
	} `json:"device_claim_tokens"`
	Pagination paginationBody `json:"pagination"`
}

type identityProviderAdminBody struct {
	IdentityProvider model.IdentityProvider `json:"identity_provider"`
}

type identityProvidersAdminBody struct {
	IdentityProviders []model.IdentityProvider `json:"identity_providers"`
	Pagination        paginationBody           `json:"pagination"`
}

type aclRoleAssignmentBody struct {
	RoleAssignment model.RoleAssignment `json:"role_assignment"`
}

type aclExternalGroupMappingBody struct {
	ExternalGroupMapping model.ExternalGroupMapping `json:"external_group_mapping"`
}

type deviceClaimOverrideAdminBody struct {
	DeviceClaimToken struct {
		ID             string  `json:"id"`
		OrganizationID *string `json:"organization_id"`
	} `json:"device_claim_token"`
	DeviceClaim struct {
		ID             string `json:"id"`
		OrganizationID string `json:"organization_id"`
		Status         string `json:"status"`
	} `json:"device_claim"`
	Device struct {
		ID             string `json:"id"`
		OrganizationID string `json:"organization_id"`
	} `json:"device"`
}

type deviceUnprovisionTestBody struct {
	Unprovision struct {
		DeviceID        string    `json:"device_id"`
		OrganizationID  string    `json:"organization_id"`
		VideoCloudDevid string    `json:"video_cloud_devid"`
		Status          string    `json:"status"`
		UnprovisionedAt time.Time `json:"unprovisioned_at"`
	} `json:"unprovision"`
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

type brandCloudBody struct {
	BrandCloud struct {
		ID                    string         `json:"id"`
		Name                  string         `json:"name"`
		TenantSlug            string         `json:"tenant_slug"`
		OrganizationKind      string         `json:"organization_kind"`
		Status                string         `json:"status"`
		Tier                  string         `json:"tier"`
		EvaluationDeviceQuota int            `json:"evaluation_device_quota"`
		Metadata              map[string]any `json:"metadata"`
	} `json:"brand_cloud"`
}

type brandCloudsBody struct {
	BrandClouds []struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		TenantSlug       string `json:"tenant_slug"`
		OrganizationKind string `json:"organization_kind"`
		Status           string `json:"status"`
		Role             string `json:"role"`
	} `json:"brand_clouds"`
	Pagination paginationBody `json:"pagination"`
}

type deviceItemProfileBody struct {
	DeviceItemProfile model.DeviceItemProfile `json:"device_item_profile"`
}

type deviceItemProfilesBody struct {
	DeviceItemProfiles []model.DeviceItemProfile `json:"device_item_profiles"`
	Pagination         paginationBody            `json:"pagination"`
}

type productionRunBody struct {
	ProductionRun model.ProductionRun `json:"production_run"`
	FactoryJWT    string              `json:"factory_jwt"`
	TokenType     string              `json:"token_type"`
	ExpiresAt     time.Time           `json:"expires_at"`
	Audience      string              `json:"audience"`
}

type brandCloudUserBody struct {
	Action         string `json:"action"`
	BrandCloudUser struct {
		ID                        string     `json:"id"`
		BrandCloudID              string     `json:"brand_cloud_id"`
		Email                     string     `json:"email"`
		DisplayName               *string    `json:"display_name"`
		EmailVerified             bool       `json:"email_verified"`
		SignupPendingVerification bool       `json:"signup_pending_verification"`
		DisabledAt                *time.Time `json:"disabled_at"`
	} `json:"brand_cloud_user"`
	BrandCloudMember struct {
		BrandCloudUserID string `json:"brand_cloud_user_id"`
		Role             string `json:"role"`
	} `json:"brand_cloud_member"`
}

type brandCloudUsersBody struct {
	BrandCloudUsers []struct {
		ID                        string     `json:"id"`
		BrandCloudID              string     `json:"brand_cloud_id"`
		Email                     string     `json:"email"`
		DisplayName               *string    `json:"display_name"`
		EmailVerified             bool       `json:"email_verified"`
		SignupPendingVerification bool       `json:"signup_pending_verification"`
		DisabledAt                *time.Time `json:"disabled_at"`
	} `json:"brand_cloud_users"`
	Pagination paginationBody `json:"pagination"`
}

type brandCloudUserStateBody struct {
	BrandCloudUser struct {
		ID                        string     `json:"id"`
		BrandCloudID              string     `json:"brand_cloud_id"`
		Email                     string     `json:"email"`
		DisplayName               *string    `json:"display_name"`
		EmailVerified             bool       `json:"email_verified"`
		SignupPendingVerification bool       `json:"signup_pending_verification"`
		DisabledAt                *time.Time `json:"disabled_at"`
	} `json:"brand_cloud_user"`
}

type brandCloudLoginBody struct {
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	BrandCloudUser struct {
		ID                        string     `json:"id"`
		BrandCloudID              string     `json:"brand_cloud_id"`
		Email                     string     `json:"email"`
		DisplayName               *string    `json:"display_name"`
		EmailVerified             bool       `json:"email_verified"`
		SignupPendingVerification bool       `json:"signup_pending_verification"`
		DisabledAt                *time.Time `json:"disabled_at"`
	} `json:"brand_cloud_user"`
	BrandCloudMember struct {
		BrandCloudUserID string `json:"brand_cloud_user_id"`
		Role             string `json:"role"`
	} `json:"brand_cloud_member"`
	Tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	} `json:"tokens"`
}

type brandCloudOwnerTransferBody struct {
	OwnerTransfer struct {
		ID                string     `json:"id"`
		BrandCloudID      string     `json:"brand_cloud_id"`
		RequestedByUserID string     `json:"requested_by_user_id"`
		TargetUserID      string     `json:"target_user_id"`
		TargetEmail       string     `json:"target_email"`
		Status            string     `json:"status"`
		AcceptedAt        *time.Time `json:"accepted_at"`
	} `json:"owner_transfer"`
}

type verifiedDeveloperFixture struct {
	UserID       string
	BrandCloudID string
	AccessToken  string
}

type quotaRaiseRequestBody struct {
	QuotaRaiseRequest struct {
		ID             string `json:"id"`
		Status         string `json:"status"`
		RequestedQuota int    `json:"requested_quota"`
	} `json:"quota_raise_request"`
}

type quotaRaiseRequestsBody struct {
	QuotaRaiseRequests []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"quota_raise_requests"`
	Pagination paginationBody `json:"pagination"`
}

type auditEventsBody struct {
	AuditEvents []struct {
		ID             string  `json:"id"`
		EventType      string  `json:"event_type"`
		ActorUserID    *string `json:"actor_user_id"`
		OrganizationID *string `json:"organization_id"`
		SubjectType    string  `json:"subject_type"`
		SubjectID      string  `json:"subject_id"`
	} `json:"audit_events"`
	Pagination paginationBody `json:"pagination"`
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
	Lifecycle lifecycleMetricsBody `json:"lifecycle"`
}

type lifecycleMetricsBody struct {
	Outbox struct {
		ByStatus            map[string]int64 `json:"by_status"`
		DeadLetteredByError []struct {
			MessageType string `json:"message_type"`
			ErrorCode   string `json:"error_code"`
			Count       int64  `json:"count"`
		} `json:"dead_lettered_by_error"`
		LastCompletedAt *time.Time `json:"last_completed_at"`
	} `json:"outbox"`
	Inbox struct {
		ByStatus            map[string]int64 `json:"by_status"`
		DeadLetteredByError []struct {
			MessageType string `json:"message_type"`
			ErrorCode   string `json:"error_code"`
			Count       int64  `json:"count"`
		} `json:"dead_lettered_by_error"`
		LastCompletedAt *time.Time `json:"last_completed_at"`
	} `json:"inbox"`
	Operations struct {
		ByStatus                map[string]int64                    `json:"by_status"`
		ByTypeAndStatus         []lifecycleOperationStatusCountBody `json:"by_type_and_status"`
		OldestActiveAgeSeconds  int64                               `json:"oldest_active_age_seconds"`
		LastTerminalCompletedAt *time.Time                          `json:"last_terminal_completed_at"`
	} `json:"operations"`
}

type lifecycleOperationStatusCountBody struct {
	OperationType string `json:"operation_type"`
	Status        string `json:"status"`
	Count         int64  `json:"count"`
}

func hasLifecycleTypeStatus(counts []lifecycleOperationStatusCountBody, operationType, status string, count int64) bool {
	for _, got := range counts {
		if got.OperationType == operationType && got.Status == status && got.Count == count {
			return true
		}
	}
	return false
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

func markEvaluationOrg(t *testing.T, env integrationEnv, orgID string, quota int) {
	t.Helper()
	if _, err := env.db.Exec(context.Background(), `
		UPDATE organizations
		SET tier = 'evaluation', evaluation_device_quota = $2, updated_at = now()
		WHERE id = $1
	`, orgID, quota); err != nil {
		t.Fatal(err)
	}
}

func verifiedDeveloperForTest(t *testing.T, env integrationEnv, email string) verifiedDeveloperFixture {
	t.Helper()
	signupRes := performJSON(env.router, http.MethodPost, "/v1/auth/signup", map[string]any{
		"email": email,
	}, "")
	if signupRes.Code != http.StatusAccepted {
		t.Fatalf("expected developer signup 202, got %d: %s", signupRes.Code, signupRes.Body.String())
	}
	signup := decodeBody[developerSignupBody](t, signupRes)
	verifyToken := latestAuthToken(t, env.tokenSink, email, "email_verification")
	verifyRes := performJSON(env.router, http.MethodPost, "/v1/auth/verify-email", map[string]any{
		"token": verifyToken, "new_password": "password123",
	}, "")
	if verifyRes.Code != http.StatusOK {
		t.Fatalf("expected verify 200, got %d: %s", verifyRes.Code, verifyRes.Body.String())
	}
	loginRes := performJSON(env.router, http.MethodPost, "/v1/auth/login", map[string]any{
		"email":    email,
		"password": "password123",
	}, "")
	if loginRes.Code != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", loginRes.Code, loginRes.Body.String())
	}
	login := decodeBody[tokenBody](t, loginRes)
	return verifiedDeveloperFixture{
		UserID:       signup.User.ID,
		BrandCloudID: signup.BrandCloud.ID,
		AccessToken:  login.Tokens.AccessToken,
	}
}

func brandCloudListHasRole(body brandCloudsBody, brandCloudID, role string) bool {
	for _, brandCloud := range body.BrandClouds {
		if brandCloud.ID == brandCloudID && brandCloud.Role == role {
			return true
		}
	}
	return false
}

func createBrandCloudForTest(t *testing.T, env integrationEnv, accessToken, name, tenantSlug string) brandCloudBody {
	t.Helper()
	res := performJSON(env.router, http.MethodPost, "/v1/admin/brand-clouds", map[string]any{
		"name":        name,
		"tenant_slug": tenantSlug,
	}, accessToken)
	if res.Code != http.StatusCreated {
		t.Fatalf("expected brand cloud create 201, got %d: %s", res.Code, res.Body.String())
	}
	return decodeBody[brandCloudBody](t, res)
}

func equalStringSlices(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

type apiFixtureBuilder struct {
	t   *testing.T
	env integrationEnv
}

func newAPIFixtureBuilder(t *testing.T, env integrationEnv) apiFixtureBuilder {
	t.Helper()
	return apiFixtureBuilder{t: t, env: env}
}

func (b apiFixtureBuilder) register(slug string) registerBody {
	b.t.Helper()
	return registerUser(b.t, b.env.router, slug+"@example.com", slug+" Org")
}

func (b apiFixtureBuilder) addMember(owner registerBody, member registerBody, role string) {
	b.t.Helper()
	res := performJSON(b.env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/members", map[string]any{
		"email": member.User.Email,
		"role":  role,
	}, owner.Tokens.AccessToken)
	if res.Code != http.StatusCreated {
		b.t.Fatalf("fixture add member %s as %s failed with %d: %s", member.User.Email, role, res.Code, res.Body.String())
	}
}

func (b apiFixtureBuilder) createDevice(owner registerBody, name, serial string) deviceBody {
	b.t.Helper()
	res := performJSON(b.env.router, http.MethodPost, "/v1/orgs/"+owner.Organization.ID+"/devices", devicePayload(name, serial), owner.Tokens.AccessToken)
	if res.Code != http.StatusCreated {
		b.t.Fatalf("fixture create device %s/%s failed with %d: %s", name, serial, res.Code, res.Body.String())
	}
	return decodeBody[deviceBody](b.t, res)
}

func (b apiFixtureBuilder) claimResolvePayload(orgID, slug string) func(string) any {
	b.t.Helper()
	return func(suffix string) any {
		rawToken := "claim-token-" + slug + "-" + suffix
		videoDevid := "claim-video-" + slug + "-" + suffix
		if _, err := store.New(b.env.db).CreateDeviceClaimToken(context.Background(), store.DeviceClaimTokenCreateInput{
			OrganizationID:  &orgID,
			TokenHash:       auth.HashToken(rawToken),
			Category:        model.DeviceCategoryIPCamera,
			VideoCloudDevid: videoDevid,
			ActivityID:      "activity-" + videoDevid,
			ClipPublicKey:   "clip-key-" + videoDevid,
			ServiceOptions:  []string{"video_streaming", "video_storage"},
			ExpiresAt:       time.Now().UTC().Add(time.Hour),
			Now:             time.Now().UTC(),
		}); err != nil {
			b.t.Fatalf("fixture seed claim token %s failed: %v", rawToken, err)
		}
		return map[string]any{
			"claim_token": rawToken,
			"device_name": "Claim Camera " + slug + " " + suffix,
		}
	}
}

func lifecycleProvisionPayload(slug string) func(string) any {
	return func(suffix string) any {
		return map[string]any{
			"operation_id":      slug + "-op-" + suffix,
			"video_cloud_devid": slug + "-video-" + suffix,
			"activity_id":       slug + "-activity-" + suffix,
			"clip_public_key":   slug + "-clip-key-" + suffix,
		}
	}
}

func lifecycleDeactivatePayload(slug string) func(string) any {
	return func(suffix string) any {
		return map[string]any{
			"operation_id": slug + "-op-" + suffix,
		}
	}
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

func configureOIDCTestServer(t *testing.T, server *Server, fake *apiOIDCTestServer, autoLink bool) {
	t.Helper()
	t.Setenv("OIDC_CLIENT_SECRET", "client-secret")
	server.ConfigureOIDC(OIDCOptions{
		Env: auth.OIDCEnvConfig{
			Enabled:       true,
			ProviderID:    "keycloak",
			ProviderName:  "Keycloak",
			IssuerURL:     fake.server.URL,
			ClientID:      "rtk-account-manager",
			ClientSecret:  "client-secret",
			RedirectURL:   "https://api.example.test/v1/auth/oidc/keycloak/callback",
			Scopes:        []string{"openid", "email", "profile"},
			AutoLinkEmail: autoLink,
		},
		HTTPClient: fake.server.Client(),
		Now:        fake.nowFn,
	})
}

func startOIDCTestLogin(t *testing.T, router *gin.Engine) (string, string) {
	t.Helper()
	loginRes := performJSON(router, http.MethodGet, "/v1/auth/oidc/keycloak/login", nil, "")
	if loginRes.Code != http.StatusFound {
		t.Fatalf("expected OIDC login redirect, got %d: %s", loginRes.Code, loginRes.Body.String())
	}
	authURL, err := url.Parse(loginRes.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := authURL.Query().Get("state")
	nonce := authURL.Query().Get("nonce")
	if state == "" || nonce == "" {
		t.Fatalf("missing state/nonce in redirect: %s", loginRes.Header().Get("Location"))
	}
	return state, nonce
}

func seedOIDCTestProvider(t *testing.T, db *pgxpool.Pool, fake *apiOIDCTestServer) model.IdentityProvider {
	t.Helper()
	secretRef := "env:OIDC_CLIENT_SECRET"
	provider, err := store.New(db).CreateIdentityProvider(context.Background(), store.IdentityProviderCreateInput{
		ProviderID:      "keycloak",
		Name:            "Keycloak",
		Type:            model.IdentityProviderTypeOIDC,
		IssuerURL:       fake.server.URL,
		ClientID:        "rtk-account-manager",
		ClientSecretRef: &secretRef,
		Scopes:          []string{"openid", "email", "profile"},
		Enabled:         true,
		Metadata:        map[string]any{"source": "test"},
		Now:             time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func seedOIDCTestState(t *testing.T, db *pgxpool.Pool, providerID string) (string, string) {
	t.Helper()
	state := "state-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	nonce := "nonce-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := store.New(db).CreateOIDCLoginState(context.Background(), store.OIDCLoginStateCreateInput{
		ProviderID:  providerID,
		StateHash:   auth.HashToken(state),
		NonceHash:   auth.HashToken(nonce),
		RedirectURL: "https://api.example.test/v1/auth/oidc/keycloak/callback",
		ExpiresAt:   time.Now().UTC().Add(24 * time.Hour),
		Now:         time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return state, nonce
}

func assertOIDCStateStoredAsHash(t *testing.T, db *pgxpool.Pool, state, nonce string) {
	t.Helper()
	var rawCount int
	if err := db.QueryRow(context.Background(), `
		SELECT count(*) FROM oidc_login_states WHERE state_hash = $1 OR nonce_hash = $2
	`, state, nonce).Scan(&rawCount); err != nil {
		t.Fatal(err)
	}
	if rawCount != 0 {
		t.Fatal("raw OIDC state or nonce was persisted")
	}
	var hashedCount int
	if err := db.QueryRow(context.Background(), `
		SELECT count(*) FROM oidc_login_states WHERE state_hash = $1 AND nonce_hash = $2
	`, auth.HashToken(state), auth.HashToken(nonce)).Scan(&hashedCount); err != nil {
		t.Fatal(err)
	}
	if hashedCount != 1 {
		t.Fatalf("expected hashed OIDC state row, got %d", hashedCount)
	}
}

func hasAuditEvent(events []model.AuditEvent, eventType string) bool {
	for _, event := range events {
		if event.EventType == eventType {
			return true
		}
	}
	return false
}

func hasAuditEventBody(events []struct {
	ID             string  `json:"id"`
	EventType      string  `json:"event_type"`
	ActorUserID    *string `json:"actor_user_id"`
	OrganizationID *string `json:"organization_id"`
	SubjectType    string  `json:"subject_type"`
	SubjectID      string  `json:"subject_id"`
}, eventType string) bool {
	for _, event := range events {
		if event.EventType == eventType {
			return true
		}
	}
	return false
}

type apiOIDCTestServer struct {
	server  *httptest.Server
	key     *rsa.PrivateKey
	keyID   string
	now     time.Time
	idToken string
}

func newAPIOIDCTestServer(t *testing.T) *apiOIDCTestServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fake := &apiOIDCTestServer{
		key:   key,
		keyID: "api-oidc-test-key",
		now:   time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(t, w, map[string]any{
			"issuer":                 fake.server.URL,
			"authorization_endpoint": fake.server.URL + "/authorize",
			"token_endpoint":         fake.server.URL + "/token",
			"jwks_uri":               fake.server.URL + "/jwks",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "auth-code" || r.Form.Get("client_id") != "rtk-account-manager" || r.Form.Get("client_secret") != "client-secret" {
			http.Error(w, "invalid token request", http.StatusBadRequest)
			return
		}
		writeJSONResponse(t, w, map[string]any{
			"token_type":    "Bearer",
			"expires_in":    3600,
			"access_token":  "provider-access-token",
			"refresh_token": "provider-refresh-token",
			"id_token":      fake.idToken,
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(t, w, map[string]any{"keys": []map[string]string{{
			"kid": fake.keyID,
			"kty": "RSA",
			"alg": "RS256",
			"use": "sig",
			"n":   base64.RawURLEncoding.EncodeToString(fake.key.PublicKey.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(fake.key.PublicKey.E)).Bytes()),
		}}})
	})
	fake.server = httptest.NewServer(mux)
	return fake
}

func (s *apiOIDCTestServer) close() {
	s.server.Close()
}

func (s *apiOIDCTestServer) nowFn() time.Time {
	return s.now
}

type apiOIDCTokenFixture struct {
	Subject       string
	Email         string
	EmailVerified bool
	Nonce         string
	Groups        []string
}

func (s *apiOIDCTestServer) signToken(t *testing.T, fixture apiOIDCTokenFixture) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":            s.server.URL,
		"sub":            fixture.Subject,
		"aud":            "rtk-account-manager",
		"exp":            s.now.Add(time.Hour).Unix(),
		"iat":            s.now.Unix(),
		"nonce":          fixture.Nonce,
		"email":          fixture.Email,
		"email_verified": fixture.EmailVerified,
	}
	if len(fixture.Groups) > 0 {
		claims["groups"] = fixture.Groups
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = s.keyID
	signed, err := token.SignedString(s.key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func writeJSONResponse(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
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

type fakeAppCertificateIssuer struct {
	calls []AppCertificateIssueRequest
}

func (f *fakeAppCertificateIssuer) IssueAppCertificate(_ context.Context, req AppCertificateIssueRequest) (AppCertificateIssueResponse, error) {
	f.calls = append(f.calls, req)
	subject := csrSubjectForTest(req.CSRPem)
	if subject == "" {
		subject = "app-user:" + req.UserID
	}
	now := time.Now().UTC().Truncate(time.Second)
	certPEM := generateTestCertificate(subject, now, now.Add(365*24*time.Hour))
	return AppCertificateIssueResponse{
		RequestID:           req.RequestID,
		UserID:              req.UserID,
		Subject:             subject,
		SerialNumber:        "1",
		NotBefore:           now,
		NotAfter:            now.Add(365 * 24 * time.Hour),
		CertificatePEM:      certPEM,
		CertificateChainPEM: certPEM,
		IssuedAt:            now,
	}, nil
}

func csrSubjectForTest(csrPEM string) string {
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil {
		return ""
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return ""
	}
	return csr.Subject.CommonName
}

func generateTestCSR(t *testing.T, subject string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: subject},
	}, key)
	if err != nil {
		t.Fatalf("CreateCertificateRequest() error = %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func generateTestCertificate(subject string, notBefore, notAfter time.Time) string {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: subject},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
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

func assertErrorDetails(t *testing.T, res *httptest.ResponseRecorder, status int, code string, retryable bool, resolutionAction string) {
	t.Helper()
	if res.Code != status {
		t.Fatalf("expected status %d, got %d: %s", status, res.Code, res.Body.String())
	}
	var body struct {
		Error struct {
			Code             string `json:"code"`
			Retryable        bool   `json:"retryable"`
			ResolutionAction string `json:"resolution_action"`
		} `json:"error"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v: %s", err, res.Body.String())
	}
	if body.Error.Code != code {
		t.Fatalf("expected error code %q, got %q: %s", code, body.Error.Code, res.Body.String())
	}
	if body.Error.Retryable != retryable {
		t.Fatalf("expected retryable %v, got %v: %s", retryable, body.Error.Retryable, res.Body.String())
	}
	if body.Error.ResolutionAction != resolutionAction {
		t.Fatalf("expected resolution_action %q, got %q: %s", resolutionAction, body.Error.ResolutionAction, res.Body.String())
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
