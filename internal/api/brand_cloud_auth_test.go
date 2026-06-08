package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
)

func TestBrandCloudContextHelpers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := &gin.Context{}

	if got := currentSubjectType(c); got != auth.SubjectTypePlatformUser {
		t.Fatalf("expected default platform subject, got %q", got)
	}
	if got := currentBrandCloudUserID(c); got != "" {
		t.Fatalf("expected empty brand cloud user id, got %q", got)
	}
	if got := currentBrandCloudID(c); got != "" {
		t.Fatalf("expected empty brand cloud id, got %q", got)
	}
	if got := currentTenantSlug(c); got != "" {
		t.Fatalf("expected empty tenant slug, got %q", got)
	}

	c.Set("subjectType", auth.SubjectTypeBrandCloudUser)
	c.Set("brandCloudUserID", "brand-user-1")
	c.Set("brandCloudID", "brand-cloud-1")
	c.Set("tenantSlug", "acme")
	if got := currentSubjectType(c); got != auth.SubjectTypeBrandCloudUser {
		t.Fatalf("expected brand subject, got %q", got)
	}
	if got := currentBrandCloudUserID(c); got != "brand-user-1" {
		t.Fatalf("expected brand cloud user id, got %q", got)
	}
	if got := currentBrandCloudID(c); got != "brand-cloud-1" {
		t.Fatalf("expected brand cloud id, got %q", got)
	}
	if got := currentTenantSlug(c); got != "acme" {
		t.Fatalf("expected tenant slug, got %q", got)
	}
}

func TestValueOrEmpty(t *testing.T) {
	if got := valueOrEmpty(nil); got != "" {
		t.Fatalf("expected empty nil value, got %q", got)
	}
	value := "acme"
	if got := valueOrEmpty(&value); got != "acme" {
		t.Fatalf("expected value, got %q", got)
	}
}

func TestBrandCloudRefreshRejectsPlatformAndWrongTenantTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	authService := auth.NewService("access-secret", "refresh-secret", time.Minute, time.Hour)
	server := New(nil, authService)

	platformRefresh, _, err := authService.IssueRefreshToken("platform-user-1")
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "tenantSlug", Value: "acme"}}
	context.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"refresh_token":"`+platformRefresh+`"}`))

	server.brandCloudRefresh(context)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected platform refresh token to be rejected with 401, got %d", recorder.Code)
	}

	brandRefresh, _, err := authService.IssueBrandCloudRefreshToken("user-1", "brand-user-1", "brand-cloud-1", "other")
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	context, _ = gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "tenantSlug", Value: "acme"}}
	context.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"refresh_token":"`+brandRefresh+`"}`))

	server.brandCloudRefresh(context)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected wrong-tenant refresh token to be rejected with 401, got %d", recorder.Code)
	}
}

func TestBrandCloudLogoutAndMeRejectMismatchedSubject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := New(nil, auth.NewService("access-secret", "refresh-secret", time.Minute, time.Hour))

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "tenantSlug", Value: "acme"}}
	context.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"refresh_token":"token"}`))

	server.brandCloudLogout(context)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected platform/default subject logout to be hidden as 404, got %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	context, _ = gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "tenantSlug", Value: "acme"}}
	context.Set("subjectType", auth.SubjectTypeBrandCloudUser)
	context.Set("tenantSlug", "other")
	context.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	server.brandCloudMe(context)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected mismatched tenant /me to be hidden as 404, got %d", recorder.Code)
	}
}
