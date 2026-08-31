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

type platformCloudCreationStore struct {
	Store
	actor string
	input store.BrandCloudInput
	calls int
	err   error
}

func (s *platformCloudCreationStore) CreateBrandCloud(_ context.Context, actor string, in store.BrandCloudInput) (model.Organization, error) {
	s.actor, s.input = actor, in
	s.calls++
	return model.Organization{ID: "cloud"}, s.err
}

func TestPlatformCloudCreationRequiresDesignatedOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const owner = "550e8400-e29b-41d4-a716-446655440000"
	s := &platformCloudCreationStore{}
	server := &Server{store: s}
	r := gin.New()
	r.POST("/clouds", func(c *gin.Context) { c.Set("userID", "operator"); server.createBrandCloud(c) })
	r.PATCH("/clouds", server.updateBrandCloud)
	run := func(method, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/clouds", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}
	for _, body := range []string{`{}`, `{"name":"cloud"}`, `{"name":"cloud","owner_user_id":"not-a-uuid"}`} {
		w := run(http.MethodPost, body)
		if w.Code != 400 || s.calls != 0 {
			t.Fatalf("invalid owner accepted: status=%d calls=%d", w.Code, s.calls)
		}
	}
	body := `{"name":"cloud","owner_user_id":"` + owner + `"}`
	if w := run(http.MethodPost, body); w.Code != 201 || s.actor != "operator" || s.input.OwnerUserID != owner {
		t.Fatalf("designated owner lost: status=%d store=%+v", w.Code, s)
	}
	if w := run(http.MethodPatch, body); w.Code != 400 {
		t.Fatalf("owner PATCH accepted: %d", w.Code)
	}
	s.err = store.ErrAccountNotActivated
	if w := run(http.MethodPost, body); w.Code != 403 {
		t.Fatalf("activation rejection: %d %s", w.Code, w.Body.String())
	}
}
