package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

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

func TestPrometheusMetricsRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := New(nil, nil).Router()

	req := httptest.NewRequest(http.MethodGet, "/metrics/prometheus", nil)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected metrics 200, got %d: %s", res.Code, res.Body.String())
	}
	if got := res.Header().Get("Content-Type"); !strings.Contains(got, "text/plain") {
		t.Fatalf("expected text/plain content type, got %q", got)
	}
	if !strings.Contains(res.Body.String(), "rtk_account_manager_up 1") {
		t.Fatalf("expected up metric, got:\n%s", res.Body.String())
	}
}

func TestPrometheusMetricHelpersFormatLabelsDeterministically(t *testing.T) {
	var b strings.Builder

	writeMetricHelp(&b, "rtk_account_manager_test_total", "Test metric.")
	writeMetricType(&b, "rtk_account_manager_test_total", "counter")
	writeMetric(&b, "rtk_account_manager_test_total", map[string]string{
		"status": "quoted \"value\"",
		"queue":  "line\\one\nline two",
	}, 42)

	want := strings.Join([]string{
		"# HELP rtk_account_manager_test_total Test metric.",
		"# TYPE rtk_account_manager_test_total counter",
		`rtk_account_manager_test_total{queue="line\\one\nline two",status="quoted \"value\""} 42`,
		"",
	}, "\n")
	if got := b.String(); got != want {
		t.Fatalf("unexpected metric output:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteStatusMetricsSortsStatuses(t *testing.T) {
	var b strings.Builder

	writeStatusMetrics(&b, "rtk_account_manager_lifecycle_messages", "outbox", map[string]int64{
		"published": 2,
		"pending":   1,
	})

	want := strings.Join([]string{
		`rtk_account_manager_lifecycle_messages{queue="outbox",status="pending"} 1`,
		`rtk_account_manager_lifecycle_messages{queue="outbox",status="published"} 2`,
		"",
	}, "\n")
	if got := b.String(); got != want {
		t.Fatalf("unexpected status metric output:\n%s\nwant:\n%s", got, want)
	}
}

func TestHealthRequestEmitsStructuredLog(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core).With(
		zap.String("service", "rtk_account_manager_api"),
		zap.String("env", "staging"),
		zap.String("version", "build-1"),
	)
	server := New(nil, nil)
	server.SetLogger(logger)
	router := server.Router()

	req := httptest.NewRequest(http.MethodGet, "/v1/health?token=secret-token", nil)
	req.Header.Set("X-Request-Id", "req-123")
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected health 200, got %d", res.Code)
	}
	entries := logs.FilterMessage("http request").All()
	if len(entries) != 1 {
		t.Fatalf("expected one request log, got %d", len(entries))
	}
	fields := entries[0].ContextMap()
	for key, want := range map[string]any{
		"service":    "rtk_account_manager_api",
		"env":        "staging",
		"version":    "build-1",
		"request_id": "req-123",
		"status":     int64(http.StatusOK),
	} {
		if got := fields[key]; got != want {
			t.Fatalf("expected %s=%v, got %v in %+v", key, want, got, fields)
		}
	}
	if _, ok := fields["duration_ms"]; !ok {
		t.Fatalf("expected duration_ms field in %+v", fields)
	}
	if got := fields["path"]; got != "/v1/health?token=[REDACTED]" {
		t.Fatalf("expected redacted path, got %v", got)
	}
}

func TestRecoveryLogDoesNotIncludeSensitiveHeaders(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	server := New(nil, nil)
	server.SetLogger(zap.New(core))
	router := server.Router()
	router.GET("/panic", func(*gin.Context) {
		panic("boom")
	})

	req := httptest.NewRequest(http.MethodGet, "/panic?client_secret=top-secret", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Cookie", "refresh_token=secret-refresh")
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusInternalServerError {
		t.Fatalf("expected panic recovery 500, got %d", res.Code)
	}
	entries := logs.FilterMessage("panic recovered").All()
	if len(entries) != 1 {
		t.Fatalf("expected one panic recovery log, got %d", len(entries))
	}
	got := entries[0].ContextMap()
	serialized := entries[0].Message + strings.Join([]string{
		fmt.Sprint(got["path"]),
		fmt.Sprint(got["authorization"]),
		fmt.Sprint(got["cookie"]),
	}, " ")
	for _, sensitive := range []string{"top-secret", "secret-token", "secret-refresh"} {
		if strings.Contains(serialized, sensitive) {
			t.Fatalf("recovery log exposed sensitive value %q in %+v", sensitive, got)
		}
	}
	if got["path"] != "/panic?client_secret=[REDACTED]" {
		t.Fatalf("expected redacted panic path, got %+v", got)
	}
}

func TestWriteClaimResolveErrorIncludesRetryability(t *testing.T) {
	tests := []struct {
		name             string
		err              error
		status           int
		code             string
		retryable        bool
		resolutionAction string
	}{
		{
			name:             "invalid token",
			err:              store.ErrNotFound,
			status:           http.StatusNotFound,
			code:             "invalid_claim_token",
			retryable:        false,
			resolutionAction: "scan_or_enter_a_valid_claim_token",
		},
		{
			name:             "quota exceeded",
			err:              store.ErrEvaluationQuotaExceeded,
			status:           http.StatusConflict,
			code:             "EVALUATION_QUOTA_EXCEEDED",
			retryable:        false,
			resolutionAction: "request_quota_raise_or_contact_admin",
		},
		{
			name:             "service unavailable",
			err:              errors.New("temporary backend failure"),
			status:           http.StatusServiceUnavailable,
			code:             "service_unavailable",
			retryable:        true,
			resolutionAction: "retry_later",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			res := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(res)
			writeClaimResolveError(c, tt.err)
			if res.Code != tt.status {
				t.Fatalf("expected status %d, got %d: %s", tt.status, res.Code, res.Body.String())
			}
			var body struct {
				Error struct {
					Code             string `json:"code"`
					Retryable        bool   `json:"retryable"`
					ResolutionAction string `json:"resolution_action"`
				} `json:"error"`
			}
			if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v: %s", err, res.Body.String())
			}
			if body.Error.Code != tt.code || body.Error.Retryable != tt.retryable || body.Error.ResolutionAction != tt.resolutionAction {
				t.Fatalf("unexpected error body: %+v", body.Error)
			}
		})
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
	core, logs := observer.New(zapcore.InfoLevel)
	sink := NewLogAuthTokenSink(zap.New(core))
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

	entries := logs.FilterMessage("auth token delivery").All()
	if len(entries) != 1 {
		t.Fatalf("expected one auth token log, got %d", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["purpose"] != "password_reset" || fields["email"] != "user@example.com" {
		t.Fatalf("unexpected auth token log fields: %+v", fields)
	}
	if fields["token"] != "reset-token" {
		t.Fatalf("expected auth token log delivery token, got %+v", fields)
	}
}

func TestSMTPAuthTokenSinkWritesDelivery(t *testing.T) {
	sink := NewSMTPAuthTokenSink("smtp.example:587", "no-reply@example.com", "https://admin.example.test/", nil)
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
	expiresAt := time.Date(2026, 6, 12, 8, 30, 0, 0, time.UTC)

	err := sink.DeliverAuthToken(context.Background(), AuthTokenDelivery{
		Purpose:   "login_activation",
		Email:     "user@example.com",
		Token:     "login-token",
		ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	if gotAddr != "smtp.example:587" || gotFrom != "no-reply@example.com" {
		t.Fatalf("unexpected SMTP envelope: addr=%q from=%q", gotAddr, gotFrom)
	}
	if len(gotTo) != 1 || gotTo[0] != "user@example.com" {
		t.Fatalf("unexpected recipients: %+v", gotTo)
	}
	message := string(gotMsg)
	for _, want := range []string{
		"To: user@example.com",
		"From: no-reply@example.com",
		"Subject: Sign in to Realtek Connect",
		"https://admin.example.test/login/activate?token=login-token",
		"Token: login-token",
		"Expires: 2026-06-12T08:30:00Z",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("expected SMTP auth message to contain %q, got %q", want, message)
		}
	}
}

func TestAuthTokenSinksHandleFallbackAndUnavailablePaths(t *testing.T) {
	if err := (LogAuthTokenSink{}).DeliverAuthToken(context.Background(), AuthTokenDelivery{
		Purpose: "login_activation",
		Email:   "user@example.com",
		Token:   "token",
	}); err != nil {
		t.Fatal(err)
	}
	smtpSink := SMTPAuthTokenSink{}
	if err := smtpSink.DeliverAuthToken(context.Background(), AuthTokenDelivery{
		Purpose: "login_activation",
		Email:   "user@example.com",
		Token:   "token",
	}); err == nil {
		t.Fatal("expected unavailable SMTP auth token sink error")
	}
}

func TestSendSMTPMailTimesOut(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		time.Sleep(500 * time.Millisecond)
	}()

	start := time.Now()
	err = smtpSendMailWithTimeout(50*time.Millisecond)(listener.Addr().String(), nil, "from@example.com", []string{"to@example.com"}, []byte("message"))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("SMTP timeout took too long: %s", elapsed)
	}
	<-done
}

func TestSendSMTPMailDeliversMessage(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	messageCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)
		writeLine := func(line string) error {
			if _, err := writer.WriteString(line + "\r\n"); err != nil {
				return err
			}
			return writer.Flush()
		}
		if err := writeLine("220 smtp.test ESMTP"); err != nil {
			errCh <- err
			return
		}
		var data strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				errCh <- err
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "EHLO "):
				if _, err := writer.WriteString("250-smtp.test\r\n250 SIZE\r\n"); err != nil {
					errCh <- err
					return
				}
				if err := writer.Flush(); err != nil {
					errCh <- err
					return
				}
			case strings.HasPrefix(line, "MAIL FROM:"):
				if err := writeLine("250 ok"); err != nil {
					errCh <- err
					return
				}
			case strings.HasPrefix(line, "RCPT TO:"):
				if err := writeLine("250 ok"); err != nil {
					errCh <- err
					return
				}
			case line == "DATA":
				if err := writeLine("354 end with dot"); err != nil {
					errCh <- err
					return
				}
				for {
					msgLine, err := reader.ReadString('\n')
					if err != nil {
						errCh <- err
						return
					}
					if strings.TrimRight(msgLine, "\r\n") == "." {
						break
					}
					data.WriteString(msgLine)
				}
				if err := writeLine("250 queued"); err != nil {
					errCh <- err
					return
				}
			case line == "QUIT":
				if err := writeLine("221 bye"); err != nil {
					errCh <- err
					return
				}
				messageCh <- data.String()
				errCh <- nil
				return
			default:
				errCh <- fmt.Errorf("unexpected SMTP command %q", line)
				return
			}
		}
	}()

	err = smtpSendMailWithTimeout(time.Second)(
		listener.Addr().String(),
		nil,
		"from@example.com",
		[]string{"to@example.com"},
		[]byte("Subject: Test\r\n\r\nhello\r\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	message := <-messageCh
	for _, want := range []string{"Subject: Test", "hello"} {
		if !strings.Contains(message, want) {
			t.Fatalf("expected SMTP message to contain %q, got %q", want, message)
		}
	}
}

func TestAuthTokenLinkRoutesByPurpose(t *testing.T) {
	for _, tt := range []struct {
		purpose string
		want    string
	}{
		{purpose: "email_verification", want: "https://admin.example.test/signup/verify?token=token+with+space"},
		{purpose: "login_activation", want: "https://admin.example.test/login/activate?token=token+with+space"},
		{purpose: "password_reset", want: "https://admin.example.test/reset-password?token=token+with+space"},
	} {
		t.Run(tt.purpose, func(t *testing.T) {
			got := authTokenLink(tt.purpose, "token with space", "https://admin.example.test/")
			if got != tt.want {
				t.Fatalf("authTokenLink() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAuthTokenSubjectAndBodyByPurpose(t *testing.T) {
	expiresAt := time.Date(2026, 6, 12, 8, 30, 0, 0, time.UTC)
	for _, tt := range []struct {
		purpose     string
		wantSubject string
		wantBody    string
	}{
		{
			purpose:     "email_verification",
			wantSubject: "Verify your Realtek Connect account",
			wantBody:    "Verify your Realtek Connect account with this link:",
		},
		{
			purpose:     "login_activation",
			wantSubject: "Sign in to Realtek Connect",
			wantBody:    "Sign in to Realtek Connect with this link:",
		},
		{
			purpose:     "password_reset",
			wantSubject: "Reset your Realtek Connect password",
			wantBody:    "Reset your Realtek Connect password with this link:",
		},
		{
			purpose:     "unknown",
			wantSubject: "Realtek Connect account token",
			wantBody:    "Use this Realtek Connect account token:",
		},
	} {
		t.Run(tt.purpose, func(t *testing.T) {
			if got := authTokenSubject(tt.purpose); got != tt.wantSubject {
				t.Fatalf("authTokenSubject() = %q, want %q", got, tt.wantSubject)
			}
			body := buildAuthTokenBody(AuthTokenDelivery{
				Purpose:   tt.purpose,
				Email:     "user@example.com",
				Token:     "token-1",
				ExpiresAt: expiresAt,
			}, "https://admin.example.test")
			for _, want := range []string{tt.wantBody, "Token: token-1", "Expires: 2026-06-12T08:30:00Z"} {
				if !strings.Contains(body, want) {
					t.Fatalf("expected body to contain %q, got %q", want, body)
				}
			}
		})
	}
	body := buildAuthTokenBody(AuthTokenDelivery{Purpose: "login_activation", Token: "token-2"}, "")
	if strings.Contains(body, "https://") || !strings.Contains(body, "Token: token-2") {
		t.Fatalf("expected token-only body without base URL, got %q", body)
	}
}

func TestLogQuotaRaiseNotificationSinkWritesDelivery(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	sink := NewLogQuotaRaiseNotificationSink(zap.New(core))

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

	entries := logs.FilterMessage("quota raise notification").All()
	if len(entries) != 1 {
		t.Fatalf("expected one quota notification log, got %d", len(entries))
	}
	fields := entries[0].ContextMap()
	for key, want := range map[string]any{
		"decision":        "approved",
		"email":           "owner@example.com",
		"org_id":          "org-1",
		"org_name":        "Owner Org",
		"requested_quota": int64(8),
		"approved_quota":  int64(12),
	} {
		if got := fields[key]; got != want {
			t.Fatalf("expected %s=%v, got %v in %+v", key, want, got, fields)
		}
	}
}

func TestQuotaNotificationSinksHandleFallbackAndUnavailablePaths(t *testing.T) {
	delivery := QuotaRaiseNotificationDelivery{
		RecipientEmail:   "owner@example.com",
		OrganizationID:   "org-1",
		OrganizationName: "Owner Org",
		RequestedQuota:   8,
		Decision:         "declined",
	}
	if err := (LogQuotaRaiseNotificationSink{}).DeliverQuotaRaiseNotification(context.Background(), delivery); err != nil {
		t.Fatal(err)
	}
	smtpSink := SMTPQuotaRaiseNotificationSink{}
	if err := smtpSink.DeliverQuotaRaiseNotification(context.Background(), delivery); err == nil {
		t.Fatal("expected unavailable SMTP quota notification sink error")
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

func TestHTTPAppCertificateIssuerIssuesAndReportsErrors(t *testing.T) {
	now := time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/issuer/v1/certificates/app/issue" {
			t.Fatalf("unexpected issuer path %s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected content type %q", r.Header.Get("Content-Type"))
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req["request_id"] == "fail" {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "issuer_down"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"request_id":            req["request_id"].(string),
			"user_id":               req["user_id"].(string),
			"subject":               "app-user:" + req["user_id"].(string),
			"serial_number":         "42",
			"not_before":            now,
			"not_after":             now.Add(time.Hour),
			"certificate_pem":       generateTestCertificate("app-user:"+req["user_id"].(string), now, now.Add(time.Hour)),
			"certificate_chain_pem": "chain",
			"issued_at":             now,
		})
	}))
	defer server.Close()

	baseURL, err := url.Parse(server.URL + "/issuer")
	if err != nil {
		t.Fatal(err)
	}
	issuer := &HTTPAppCertificateIssuer{baseURL: baseURL, client: server.Client()}
	out, err := issuer.IssueAppCertificate(context.Background(), AppCertificateIssueRequest{
		RequestID: "req-1",
		UserID:    "user-1",
		CSRPem:    "csr",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.RequestID != "req-1" || out.UserID != "user-1" || out.SerialNumber != "42" {
		t.Fatalf("unexpected issuer response: %+v", out)
	}

	if _, err := issuer.IssueAppCertificate(context.Background(), AppCertificateIssueRequest{RequestID: "fail"}); err == nil {
		t.Fatal("expected issuer status error")
	}
	badJSONServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{"))
	}))
	defer badJSONServer.Close()
	badJSONURL, err := url.Parse(badJSONServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	badJSONIssuer := &HTTPAppCertificateIssuer{baseURL: badJSONURL, client: badJSONServer.Client()}
	if _, err := badJSONIssuer.IssueAppCertificate(context.Background(), AppCertificateIssueRequest{RequestID: "bad-json"}); err == nil {
		t.Fatal("expected issuer decode error")
	}
	if _, err := (*HTTPAppCertificateIssuer)(nil).IssueAppCertificate(context.Background(), AppCertificateIssueRequest{}); err == nil {
		t.Fatal("expected unconfigured issuer error")
	}
}

func TestNewHTTPAppCertificateIssuerValidatesConfig(t *testing.T) {
	if _, err := NewHTTPAppCertificateIssuer(HTTPAppCertificateIssuerConfig{BaseURL: "://bad"}); err == nil {
		t.Fatal("expected invalid base URL error")
	}

	certFile, keyFile, caFile := writeAppIssuerTLSFiles(t)
	issuer, err := NewHTTPAppCertificateIssuer(HTTPAppCertificateIssuerConfig{
		BaseURL:    "https://issuer.example",
		ClientCert: certFile,
		ClientKey:  keyFile,
		CAFile:     caFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if issuer.client == nil || issuer.baseURL.Host != "issuer.example" {
		t.Fatalf("unexpected issuer: %+v", issuer)
	}
}

func TestAppCertificateValidationAndErrors(t *testing.T) {
	subject := "app-user:user-1"
	csrPEM := generateTestCSR(t, subject)
	if der, err := validateAppCSRSubject(csrPEM, subject); err != nil || len(der) == 0 {
		t.Fatalf("expected valid CSR, der=%d err=%v", len(der), err)
	}
	if _, err := validateAppCSRSubject(csrPEM, "app-user:other"); !errors.Is(err, errAppCertificateCSRInvalid) {
		t.Fatalf("expected CSR subject error, got %v", err)
	}
	if _, err := validateAppCSRSubject("not pem", subject); !errors.Is(err, errAppCertificateCSRInvalid) {
		t.Fatalf("expected CSR PEM error, got %v", err)
	}

	certPEM := generateTestCertificate(subject, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
	cert, fingerprint, err := certificateFingerprint(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != subject || fingerprint == "" {
		t.Fatalf("unexpected fingerprint result: subject=%s fingerprint=%q", cert.Subject.CommonName, fingerprint)
	}
	if _, _, err := certificateFingerprint("not a cert"); !errors.Is(err, errAppCertificateCSRInvalid) {
		t.Fatalf("expected certificate parse error, got %v", err)
	}

	tests := []struct {
		err    error
		status int
		code   string
	}{
		{err: errAppCertificateIssuerUnavailable, status: http.StatusServiceUnavailable, code: "app_certificate_issuer_unavailable"},
		{err: errAppCertificateCSRInvalid, status: http.StatusBadRequest, code: "app_certificate_csr_invalid"},
	}
	for _, tt := range tests {
		res := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(res)
		writeAppCertificateError(c, tt.err)
		if res.Code != tt.status {
			t.Fatalf("expected status %d, got %d: %s", tt.status, res.Code, res.Body.String())
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Error.Code != tt.code {
			t.Fatalf("expected code %q, got %+v", tt.code, body.Error)
		}
	}
}

func writeAppIssuerTLSFiles(t *testing.T) (string, string, string) {
	t.Helper()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	clientTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "client"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	certFile := dir + "/client.crt"
	keyFile := dir + "/client.key"
	caFile := dir + "/ca.crt"
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(clientKey)}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, caFile
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

func TestOIDCGroupsFromClaimsShapes(t *testing.T) {
	tests := []struct {
		name   string
		claims map[string]any
		want   []string
	}{
		{name: "missing", claims: map[string]any{}, want: nil},
		{name: "array", claims: map[string]any{"groups": []any{"/installers", "", 42, " /support "}}, want: []string{"/installers", "/support"}},
		{name: "string slice", claims: map[string]any{"groups": []string{"/fleet", " "}}, want: []string{"/fleet"}},
		{name: "single group", claims: map[string]any{"group": " /ops "}, want: []string{"/ops"}},
		{name: "unsupported", claims: map[string]any{"groups": 12}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := oidcGroupsFromClaims(tt.claims)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v want %v", got, tt.want)
				}
			}
		})
	}
}

func FuzzBindStrictRequestShape(f *testing.F) {
	gin.SetMode(gin.TestMode)

	type strictRequest struct {
		Name string `json:"name"`
	}

	f.Add(`{"name":"camera"}`)
	f.Add(`{"name":"camera","claim_material":{"serial_number":"CAM-001"}}`)
	f.Add(`{"name":"camera"} {}`)
	f.Add(`{"name":"   "}`)
	f.Add(`not json`)
	f.Add(`{"name":1}`)

	f.Fuzz(func(t *testing.T, body string) {
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))

		var req strictRequest
		ok := bindStrict(context, &req)
		if ok && recorder.Code >= http.StatusBadRequest {
			t.Fatalf("bindStrict accepted request but wrote error status %d: %s", recorder.Code, recorder.Body.String())
		}
		if !ok && recorder.Code < http.StatusBadRequest {
			t.Fatalf("bindStrict rejected request without error status, code=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})
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
		wantFailureLayer   string
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
			wantFailureLayer:   "cloud_activation",
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
			name: "deactivation failure is attributed",
			device: readinessDevice(model.DeviceStatusOffline, map[string]any{
				model.DeviceMetadataVideoCloudActivationStatus: string(model.VideoCloudActivationStatusActivated),
			}),
			provisioningStatus: model.DeviceOperationStatusSucceeded,
			deactivationStatus: operationStatusPtr(model.DeviceOperationStatusDeadLettered),
			want:               model.DeviceReadinessStateDeactivationFailed,
			wantProduct:        model.ProductReadinessStateFailed,
			wantFailureLayer:   "deactivation",
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
					OperationID:   "deactivate-op",
					OperationType: model.DeviceOperationTypeDeactivate,
					Status:        *tt.deactivationStatus,
					ErrorCode:     stringPtr("deactivate_failed"),
					ErrorMessage:  stringPtr("Deactivate failed"),
					UpdatedAt:     time.Now().UTC(),
				}
			}

			provisioningOperation := model.DeviceOperation{
				OperationID:   "provision-op",
				OperationType: model.DeviceOperationTypeProvision,
				Status:        tt.provisioningStatus,
				ErrorCode:     stringPtr("activation_failed"),
				ErrorMessage:  stringPtr("Activation failed"),
				UpdatedAt:     time.Now().UTC(),
			}
			got := readinessFromProjection(tt.device, &provisioningOperation, latestDeactivation)

			if got.State != tt.want {
				t.Fatalf("expected readiness %s, got %+v", tt.want, got)
			}
			if got.ProductState != tt.wantProduct {
				t.Fatalf("expected product readiness %s, got %+v", tt.wantProduct, got)
			}
			if got.Sources.DeviceStatus != tt.device.Status ||
				got.Sources.ProvisioningOperationStatus == nil ||
				*got.Sources.ProvisioningOperationStatus != tt.provisioningStatus {
				t.Fatalf("expected source facts to reflect device and operation, got %+v", got.Sources)
			}
			if tt.deactivationStatus != nil && (got.Sources.DeactivationOperationStatus == nil || *got.Sources.DeactivationOperationStatus != *tt.deactivationStatus) {
				t.Fatalf("expected deactivation source status %s, got %+v", *tt.deactivationStatus, got.Sources.DeactivationOperationStatus)
			}
			if tt.wantFailureLayer == "" {
				if got.Failure != nil {
					t.Fatalf("did not expect failure attribution, got %+v", got.Failure)
				}
			} else if got.Failure == nil || got.Failure.FailedLayer != tt.wantFailureLayer || got.Failure.OperationID == nil {
				t.Fatalf("expected %s failure attribution with operation id, got %+v", tt.wantFailureLayer, got.Failure)
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
