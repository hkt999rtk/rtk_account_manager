package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type cloudMemberVisibilityStore struct {
	Store
	role      model.Role
	err       error
	listCalls int
}

func (s *cloudMemberVisibilityStore) GetDeveloperBrandCloudMember(context.Context, string, string) (model.Member, error) {
	return model.Member{Role: s.role}, s.err
}
func (s *cloudMemberVisibilityStore) ListDeveloperBrandCloudMembers(context.Context, string, int, int) (store.MemberPage, error) {
	s.listCalls++
	return store.MemberPage{Members: []model.Member{}}, nil
}
func TestCloudMemberCollectionIsOwnerOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, role := range []model.Role{model.RoleOwner, model.RoleAdmin, model.RoleMember, "viewer", ""} {
		s := &cloudMemberVisibilityStore{role: role}
		server := &Server{store: s}
		r := gin.New()
		r.GET("/clouds/:brandCloudId/members", func(c *gin.Context) { c.Set("userID", "user"); server.listDeveloperBrandCloudMembers(c) })
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/clouds/cloud/members", nil))
		want, calls := http.StatusForbidden, 0
		if role == model.RoleOwner {
			want, calls = http.StatusOK, 1
		}
		if w.Code != want || s.listCalls != calls {
			t.Fatalf("role=%s status=%d listCalls=%d", role, w.Code, s.listCalls)
		}
	}
}
