package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type managedListStore struct {
	Store
	page                                  store.ManagedBrandCloudPage
	err                                   error
	permissionErr                         error
	actor, view                           string
	limit, offset, calls, permissionCalls int
}

func (s *managedListStore) ListManagedBrandClouds(_ context.Context, actor, view string, limit, offset int) (store.ManagedBrandCloudPage, error) {
	s.calls++
	s.actor = actor
	s.view = view
	s.limit = limit
	s.offset = offset
	return s.page, s.err
}
func (s *managedListStore) ListUserOrganizationPermissions(context.Context, string, string) ([]string, error) {
	s.permissionCalls++
	return []string{}, s.permissionErr
}

func runManagedList(s *managedListStore, query string) *httptest.ResponseRecorder {
	r := gin.New()
	server := &Server{store: s}
	r.GET("/clouds", func(c *gin.Context) { c.Set("userID", "global-user"); server.listDeveloperBrandClouds(c) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/clouds"+query, nil))
	return w
}

func TestManagedCloudListHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, query := range []string{"?view=wrong", "?limit=0", "?limit=101", "?limit=x", "?offset=-1", "?offset=x"} {
		s := &managedListStore{}
		w := runManagedList(s, query)
		if w.Code != 400 || s.calls != 0 {
			t.Fatalf("query=%s status=%d calls=%d", query, w.Code, s.calls)
		}
	}
	s := &managedListStore{page: store.ManagedBrandCloudPage{BrandClouds: []store.ManagedBrandCloud{
		{Organization: model.Organization{ID: "pending", Role: model.RoleOwner}, Capabilities: []string{}, Operational: false},
		{Organization: model.Organization{ID: "active", Role: model.RoleOwner}, OwnerUserID: "global-user", MyRole: model.RoleOwner, OwnershipVersion: 1, Capabilities: []string{}, Operational: true},
	}, Page: store.Page{Total: 3, Limit: 25}, OwnedCount: 2, OwnedLimit: 8}}
	w := runManagedList(s, "")
	if w.Code != 200 || s.actor != "global-user" || s.view != "all" || s.limit != 25 || s.offset != 0 || s.permissionCalls != 1 {
		t.Fatalf("status=%d store=%+v body=%s", w.Code, s, w.Body.String())
	}
	var body struct {
		Total, Limit, Offset int
		OwnedCount           int `json:"owned_count"`
		OwnedLimit           int `json:"owned_limit"`
		Clouds               []struct {
			Capabilities []string `json:"capabilities"`
		} `json:"brand_clouds"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 3 || body.OwnedCount != 2 || body.OwnedLimit != 8 || len(body.Clouds) != 2 || len(body.Clouds[0].Capabilities) != 0 || len(body.Clouds[1].Capabilities) == 0 {
		t.Fatalf("body=%s", w.Body.String())
	}
	w = runManagedList(s, "?view=shared&limit=1&offset=2")
	if w.Code != 200 || s.view != "shared" || s.limit != 1 || s.offset != 2 {
		t.Fatalf("scope/offset lost: %+v", s)
	}
	for _, err := range []error{store.ErrNotFound, errors.New("database unavailable")} {
		s.err = err
		w = runManagedList(s, "")
		if w.Code == 200 {
			t.Fatalf("store failure swallowed: %v", err)
		}
	}
	s.err = nil
	s.permissionErr = errors.New("authorization unavailable")
	if w := runManagedList(s, ""); w.Code == 200 {
		t.Fatal("authorization failure swallowed")
	}
}
