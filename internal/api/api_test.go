package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"rtk_account_manager/internal/auth"
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
