package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type developerPKIHandlerStore struct {
	Store
	member    model.Member
	memberErr error
	user      model.BrandCloudUser
	userErr   error
}

func (s *developerPKIHandlerStore) GetDeveloperBrandCloudMember(context.Context, string, string) (model.Member, error) {
	return s.member, s.memberErr
}

func (s *developerPKIHandlerStore) GetBrandCloudUser(context.Context, string) (model.BrandCloudUser, error) {
	return s.user, s.userErr
}

func TestIssueDeveloperPKITestAppCertificateRejectsInvalidRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("ACCOUNT_MANAGER_ENV", "staging")

	t.Run("disabled", func(t *testing.T) {
		t.Setenv("DEVELOPER_PKI_TEST_TOOLS_ENABLED", "false")
		rec := runDeveloperPKIHandler(t, &Server{}, "idem", `{}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
	})

	t.Setenv("DEVELOPER_PKI_TEST_TOOLS_ENABLED", "true")
	t.Run("idempotency required", func(t *testing.T) {
		rec := runDeveloperPKIHandler(t, &Server{}, "", `{}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	for _, tc := range []struct {
		name  string
		store *developerPKIHandlerStore
		body  string
		want  int
	}{
		{"member lookup", &developerPKIHandlerStore{memberErr: store.ErrNotFound}, `{}`, http.StatusNotFound},
		{"member role", &developerPKIHandlerStore{member: model.Member{Role: model.RoleMember}}, `{}`, http.StatusForbidden},
		{"invalid JSON", &developerPKIHandlerStore{member: model.Member{Role: model.RoleOwner}}, `{`, http.StatusBadRequest},
		{"invalid target", &developerPKIHandlerStore{member: model.Member{Role: model.RoleOwner}}, `{"target_type":"other","target_id":"id","csr_pem":"csr"}`, http.StatusBadRequest},
		{"brand user not found", &developerPKIHandlerStore{member: model.Member{Role: model.RoleAdmin}, userErr: store.ErrNotFound}, `{"target_type":"brand_cloud_user","target_id":"id","csr_pem":"csr"}`, http.StatusNotFound},
		{"end user lookup unavailable", &developerPKIHandlerStore{member: model.Member{Role: model.RoleOwner}}, `{"target_type":"end_user","target_id":"id","csr_pem":"csr"}`, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := runDeveloperPKIHandler(t, &Server{store: tc.store}, "idem", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func runDeveloperPKIHandler(t *testing.T, server *Server, idempotencyKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	router := gin.New()
	router.POST("/v1/developer/brand-clouds/:brandCloudId/pki/test-app-certificates", func(c *gin.Context) {
		c.Set("userID", "user-1")
		server.issueDeveloperPKITestAppCertificate(c)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/developer/brand-clouds/brand-1/pki/test-app-certificates", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}
