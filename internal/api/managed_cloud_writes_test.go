package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/store"
)

func managedCloudHTTP(t *testing.T, router *gin.Engine, method, path, token, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, ok := body.(string)
	if !ok {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(b)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	return res
}

func TestIntegrationManagedCloudWrites(t *testing.T) {
	env := newIntegrationEnv(t)
	contract := newResponseContract(t)
	owner := verifiedDeveloperForTest(t, env, "crud-api-owner@example.com")
	other := verifiedDeveloperForTest(t, env, "crud-api-other@example.com")
	base := "/v1/developer/brand-clouds"
	for _, tc := range []struct{ body, key string }{
		{`{"name":"Valid"}`, ""}, {`{"name":"Valid"}`, strings.Repeat("k", 201)},
		{`{}`, "invalid"}, {`null`, "invalid"}, {`[]`, "invalid"}, {`{"name":null}`, "invalid"},
		{`{"name":"   "}`, "invalid"}, {`{"name":42}`, "invalid"}, {`{"name":"ok","description":null}`, "invalid"},
		{`{"name":"ok","tenant_slug":"custom"}`, "invalid"}, {`{"name":"ok","owner_user_id":"other"}`, "invalid"},
		{`{"name":"ok","metadata":{}}`, "invalid"}, {`{"name":"first","name":"second"}`, "invalid"},
		{`{"name":"ok"} {}`, "invalid"}, {`{"name":"` + strings.Repeat("a", 17000) + `"}`, "invalid"},
		{`{"name":"` + strings.Repeat("雲", 256) + `"}`, "invalid"},
	} {
		res := managedCloudHTTP(t, env.router, http.MethodPost, base, owner.AccessToken, tc.key, tc.body)
		if res.Code != 400 {
			t.Fatalf("invalid create: %d %s", res.Code, res.Body.String())
		}
		contract.validate(t, http.MethodPost, base, res)
	}
	body := map[string]any{"name": "API Cloud", "description": "Independent cloud"}
	res := managedCloudHTTP(t, env.router, http.MethodPost, base, owner.AccessToken, strings.Repeat("k", 200), body)
	if res.Code != 201 || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create: %d %s", res.Code, res.Body.String())
	}
	contract.validate(t, http.MethodPost, base, res)
	type response struct {
		Cloud store.ManagedBrandCloud `json:"brand_cloud"`
	}
	cloud := decodeBody[response](t, res).Cloud
	if cloud.OwnerUserID != owner.UserID || cloud.MyRole != "owner" || cloud.Description != "Independent cloud" || !hasCapability(cloud.Capabilities, "cloud.update") {
		t.Fatalf("create projection: %+v", cloud)
	}
	replay := managedCloudHTTP(t, env.router, http.MethodPost, base, owner.AccessToken, strings.Repeat("k", 200), body)
	if replay.Code != 201 || replay.Body.String() != res.Body.String() {
		t.Fatalf("replay: %d %s", replay.Code, replay.Body.String())
	}
	detail := base + "/" + cloud.ID
	res = managedCloudHTTP(t, env.router, http.MethodPatch, detail, owner.AccessToken, "patch", map[string]any{"name": "New name", "description": ""})
	if res.Code != 200 {
		t.Fatalf("patch: %d %s", res.Code, res.Body.String())
	}
	contract.validate(t, http.MethodPatch, detail, res)
	changed := decodeBody[response](t, res).Cloud
	if changed.Name != "New name" || !reflect.DeepEqual(changed.TenantSlug, cloud.TenantSlug) || changed.Description != "" || changed.OwnerUserID != cloud.OwnerUserID {
		t.Fatalf("changed immutable fields: %+v", changed)
	}
	for _, bad := range []string{`{}`, `{"description":null}`, `{"status":"disabled"}`, `{"tenant_slug":"renamed"}`, `{"name":""}`} {
		res = managedCloudHTTP(t, env.router, http.MethodPatch, detail, owner.AccessToken, "bad", bad)
		if res.Code != 400 {
			t.Fatalf("invalid patch: %d %s", res.Code, res.Body.String())
		}
	}
	res = managedCloudHTTP(t, env.router, http.MethodPatch, detail, other.AccessToken, "patch", map[string]any{"name": "stolen"})
	if res.Code != 404 {
		t.Fatalf("other owner mutated cloud: %d %s", res.Code, res.Body.String())
	}
	res = performJSON(env.router, http.MethodGet, detail, nil, other.AccessToken)
	if res.Code != 404 {
		t.Fatalf("other owner read cloud: %d %s", res.Code, res.Body.String())
	}
	res = performJSON(env.router, http.MethodGet, detail, nil, owner.AccessToken)
	if res.Code != 200 {
		t.Fatalf("detail: %d %s", res.Code, res.Body.String())
	}
	contract.validate(t, http.MethodGet, detail, res)
	if decodeBody[response](t, res).Cloud.Name != "New name" {
		t.Fatal("detail stale")
	}
	res = managedCloudHTTP(t, env.router, http.MethodPost, base, "", "anon", body)
	if res.Code != 401 {
		t.Fatalf("anonymous create: %d", res.Code)
	}
}

func TestManagedCloudNonHumanSessionRejected(t *testing.T) {
	// A service identity carrying a userID must not become a human owner.
	s := &Server{}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", "global-id")
		c.Set("subjectType", auth.SubjectType("service_account"))
	})
	r.POST("/clouds", s.createDeveloperBrandCloud)
	r.GET("/clouds", s.listDeveloperBrandClouds)
	r.GET("/clouds/:brandCloudId", s.getDeveloperBrandCloud)
	r.PATCH("/clouds/:brandCloudId", s.updateDeveloperBrandCloud)
	for _, tc := range [][2]string{{"POST", "/clouds"}, {"GET", "/clouds"}, {"GET", "/clouds/id"}, {"PATCH", "/clouds/id"}} {
		res := managedCloudHTTP(t, r, tc[0], tc[1], "", "key", `{"name":"not-human"}`)
		if res.Code != 403 {
			t.Fatalf("nonhuman %v: %d", tc, res.Code)
		}
	}
}
