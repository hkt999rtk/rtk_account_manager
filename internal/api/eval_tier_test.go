package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestLoadSignupPolicyHonorsEnvironmentOverrides(t *testing.T) {
	t.Setenv("SIGNUP_DISPOSABLE_DOMAINS", "example.com, test.invalid ")

	policy := loadSignupPolicy()
	if _, ok := policy.disposableDomains["example.com"]; !ok {
		t.Fatalf("expected custom disposable domain to be loaded, got %+v", policy.disposableDomains)
	}
	if _, ok := policy.disposableDomains["test.invalid"]; !ok {
		t.Fatalf("expected trimmed disposable domain to be loaded, got %+v", policy.disposableDomains)
	}
	if _, ok := policy.disposableDomains["mailinator.com"]; ok {
		t.Fatalf("expected default disposable domain set to be replaced, got %+v", policy.disposableDomains)
	}
}

func TestIsDisposableSignupEmail(t *testing.T) {
	domains := map[string]struct{}{
		"mailinator.com": {},
	}

	if !isDisposableSignupEmail("Display Name <user@mailinator.com>", domains) {
		t.Fatal("expected parsed disposable email to be rejected")
	}
	if isDisposableSignupEmail("user@example.com", domains) {
		t.Fatal("expected non-disposable email to be accepted")
	}
	if isDisposableSignupEmail("not-an-email", domains) {
		t.Fatal("expected invalid email syntax to be treated as non-disposable")
	}
}

func TestAllowSignupEnforcesDisposableAndRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	disposableRecorder := httptest.NewRecorder()
	disposableContext, _ := gin.CreateTestContext(disposableRecorder)
	disposableContext.Request = httptest.NewRequest(http.MethodPost, "/v1/auth/signup", nil)
	disposableContext.Request.RemoteAddr = "203.0.113.11:1234"
	disposableServer := &Server{
		signupLimiter: newSignupLimiter(5, time.Hour),
		signupPolicy: signupPolicy{
			disposableDomains: map[string]struct{}{"mailinator.com": {}},
		},
	}
	if disposableServer.allowSignup(disposableContext, "Display Name <user@mailinator.com>") {
		t.Fatal("expected disposable email to block signup")
	}
	if disposableRecorder.Code != http.StatusBadRequest {
		t.Fatalf("expected disposable email failure 400, got %d", disposableRecorder.Code)
	}

	limitedRecorder := httptest.NewRecorder()
	limitedContext, _ := gin.CreateTestContext(limitedRecorder)
	limitedContext.Request = httptest.NewRequest(http.MethodPost, "/v1/auth/signup", nil)
	limitedContext.Request.RemoteAddr = "203.0.113.12:1234"
	limitedServer := &Server{
		signupLimiter: newSignupLimiter(1, time.Hour),
		signupPolicy:  signupPolicy{},
	}
	if !limitedServer.allowSignup(limitedContext, "user@example.com") {
		t.Fatal("expected first signup attempt to pass")
	}
	if limitedServer.allowSignup(limitedContext, "user@example.com") {
		t.Fatal("expected second signup attempt in the same window to be rate limited")
	}
	if limitedRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("expected rate limit failure 429, got %d", limitedRecorder.Code)
	}
}

func TestSignupLimiterEvictsStaleEntries(t *testing.T) {
	limiter := newSignupLimiter(1, time.Millisecond)
	now := time.Now().UTC()
	for i := 0; i < 300; i++ {
		limiter.allow(fmt.Sprintf("10.0.0.%d", i), now)
	}
	later := now.Add(time.Second)
	if !limiter.allow("fresh-ip", later) {
		t.Fatal("expected fresh ip to be allowed after eviction")
	}
	if _, ok := limiter.counters["10.0.0.0"]; ok {
		t.Fatalf("expected stale entries to be evicted, still present")
	}
}

func TestFailureFromMetadataUsesProjectedErrorFacts(t *testing.T) {
	updatedAt := time.Date(2026, 5, 18, 8, 30, 0, 0, time.UTC)

	failure := failureFromMetadata(map[string]any{
		"code":    "video_timeout",
		"message": "Video cloud did not finish activation",
	}, updatedAt)
	if failure == nil {
		t.Fatal("expected failure response")
	}
	if failure.FailedLayer != "cloud_activation" || failure.SourceState != "video_cloud_last_error" {
		t.Fatalf("unexpected failure source: %+v", failure)
	}
	if failure.Retryable {
		t.Fatal("projected metadata failures should not be retryable by default")
	}
	if failure.ErrorCode != "video_timeout" || failure.ErrorMessage != "Video cloud did not finish activation" {
		t.Fatalf("expected projected error facts, got %+v", failure)
	}
	if failure.OccurredAt == nil || !failure.OccurredAt.Equal(updatedAt) {
		t.Fatalf("expected metadata update time, got %+v", failure.OccurredAt)
	}

	fallback := failureFromMetadata("legacy-error", updatedAt)
	if fallback.ErrorCode != "video_cloud_last_error" || fallback.ErrorMessage != "Projected video cloud error" {
		t.Fatalf("expected fallback metadata error facts, got %+v", fallback)
	}
}
