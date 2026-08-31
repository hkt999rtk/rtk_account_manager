package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"rtk_account_manager/internal/auth"
)

func TestRetiredTenantTokenRejectedWithoutPersistence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{auth: auth.NewService("test-key", "refresh-key", time.Minute, time.Hour)}
	router := gin.New()
	entered := false
	router.GET("/protected", server.requireAuth(), func(c *gin.Context) { entered = true; c.Status(http.StatusNoContent) })
	for _, kind := range []auth.SubjectType{auth.SubjectTypeBrandCloudUser, auth.SubjectTypeUser, ""} {
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, auth.Claims{
			SubjectType: kind, UserID: "global-user", BrandCloudUserID: "retired-user", Kind: auth.TokenKindAccess,
			RegisteredClaims: jwt.RegisteredClaims{Subject: "global-user", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
		}).SignedString([]byte("test-key"))
		if err != nil {
			t.Fatal(err)
		}
		response := performJSON(router, http.MethodGet, "/protected", nil, token)
		if response.Code != http.StatusUnauthorized || entered {
			t.Fatalf("tenant credential reached handler: %d", response.Code)
		}
	}
}

func TestProductScopeNeverFallsBackToTenantClaims(t *testing.T) {
	for _, params := range []gin.Params{
		{{Key: "orgId", Value: "requested-cloud"}},
		{{Key: "brandCloudId", Value: "requested-cloud"}},
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Params = params
		c.Set("brandCloudID", "unrelated-cloud")
		c.Set("brandCloudUserID", "retired-user")
		if got := profileBrandCloudID(c); got != "requested-cloud" {
			t.Fatalf("scope came from token context: %s", got)
		}
	}
}
