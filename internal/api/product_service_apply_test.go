package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/store"
)

type productApplyHandlerCall struct {
	operation, actor, cloud, product, jobID, deviceID, previewToken, messageID, action string
	limit, offset                                                                      int
}

type productApplyHandlerStore struct {
	Store
	calls      []productApplyHandlerCall
	itemStatus string
	failOn     string
}

func (s *productApplyHandlerStore) record(call productApplyHandlerCall) error {
	s.calls = append(s.calls, call)
	if s.failOn == call.operation {
		return store.ErrConflict
	}
	return nil
}

func (s *productApplyHandlerStore) PreviewProductServiceApply(_ context.Context, actor, cloud, product string) (store.ProductServiceApplyPreview, error) {
	err := s.record(productApplyHandlerCall{operation: "preview", actor: actor, cloud: cloud, product: product})
	return store.ProductServiceApplyPreview{PreviewToken: "preview-token", TargetRevision: 7, TotalDevices: 301}, err
}

func (s *productApplyHandlerStore) AdmitProductServiceApply(_ context.Context, actor, cloud, product, jobID, previewToken string) (store.ProductServiceApplyJob, error) {
	err := s.record(productApplyHandlerCall{operation: "admit", actor: actor, cloud: cloud, product: product, jobID: jobID, previewToken: previewToken})
	return store.ProductServiceApplyJob{ID: jobID, OrganizationID: cloud, ProductID: product, TargetRevision: 7, TotalDevices: 301, Status: "active"}, err
}

func (s *productApplyHandlerStore) GetProductServiceApplyJob(_ context.Context, actor, cloud, product, jobID string) (store.ProductServiceApplyJob, error) {
	err := s.record(productApplyHandlerCall{operation: "get_job", actor: actor, cloud: cloud, product: product, jobID: jobID})
	return store.ProductServiceApplyJob{ID: jobID, TargetRevision: 7, Status: "active"}, err
}

func (s *productApplyHandlerStore) ListProductServiceApplyItems(_ context.Context, actor, cloud, product, jobID string, limit, offset int) (store.ProductServiceApplyItemPage, error) {
	err := s.record(productApplyHandlerCall{operation: "list_items", actor: actor, cloud: cloud, product: product, jobID: jobID, limit: limit, offset: offset})
	return store.ProductServiceApplyItemPage{Items: []store.ProductServiceApplyItem{{DeviceID: "device-301", Status: "pending"}}, Total: 301}, err
}

func (s *productApplyHandlerStore) GetProductServiceApplyItem(_ context.Context, actor, cloud, product, jobID, deviceID string) (store.ProductServiceApplyItem, error) {
	err := s.record(productApplyHandlerCall{operation: "get_item", actor: actor, cloud: cloud, product: product, jobID: jobID, deviceID: deviceID})
	return store.ProductServiceApplyItem{DeviceID: deviceID, Status: "pending"}, err
}

func (s *productApplyHandlerStore) DispatchProductServiceApplyItem(_ context.Context, actor, cloud, product, jobID, deviceID, messageID string) (store.ProductServiceApplyItem, error) {
	err := s.record(productApplyHandlerCall{operation: "dispatch", actor: actor, cloud: cloud, product: product, jobID: jobID, deviceID: deviceID, messageID: messageID})
	return store.ProductServiceApplyItem{DeviceID: deviceID, OperationID: "same-operation", Status: s.itemStatus}, err
}

func (s *productApplyHandlerStore) FinishProductServiceApply(_ context.Context, actor, cloud, product, jobID, action string) (store.ProductServiceApplyJob, error) {
	err := s.record(productApplyHandlerCall{operation: "finish", actor: actor, cloud: cloud, product: product, jobID: jobID, action: action})
	status := "canceled"
	if action == "complete" {
		status = "completed"
	}
	return store.ProductServiceApplyJob{ID: jobID, Status: status}, err
}

func productApplyHandlerRouter(s *Server) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "authenticated-actor"); c.Next() })
	base := "/v1/orgs/:orgId/device-item-profiles/:profileId"
	r.GET(base+"/service-apply-preview", s.previewProductServiceApply)
	r.POST(base+"/service-apply-jobs", s.admitProductServiceApply)
	r.GET(base+"/service-apply-jobs/:jobId", s.getProductServiceApplyJob)
	r.GET(base+"/service-apply-jobs/:jobId/items", s.listProductServiceApplyItems)
	r.GET(base+"/service-apply-jobs/:jobId/items/:deviceId", s.getProductServiceApplyItem)
	r.POST(base+"/service-apply-jobs/:jobId/items/:deviceId/dispatch", s.dispatchProductServiceApplyItem)
	r.POST(base+"/service-apply-jobs/:jobId/cancel", s.cancelProductServiceApply)
	r.POST(base+"/service-apply-jobs/:jobId/complete", s.completeProductServiceApply)
	return r
}

func productApplyHandlerRequest(router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestProductServiceApplyHTTPPreservesAuthenticatedScopeAndPagesPast250(t *testing.T) {
	gin.SetMode(gin.TestMode)
	backend := &productApplyHandlerStore{itemStatus: "accepted"}
	router := productApplyHandlerRouter(New(backend, nil))
	base := "/v1/orgs/cloud-1/device-item-profiles/product-1"
	job := base + "/service-apply-jobs/job-1"
	requests := []struct {
		method, path, body string
		status             int
		contains           string
	}{
		{http.MethodGet, base + "/service-apply-preview", "", http.StatusOK, `"total_devices":301`},
		{http.MethodPost, base + "/service-apply-jobs", `{"job_id":" job-1 ","preview_token":" preview-token "}`, http.StatusAccepted, `"target_revision":7`},
		{http.MethodGet, job, "", http.StatusOK, `"id":"job-1"`},
		{http.MethodGet, job + "/items?limit=500&offset=250", "", http.StatusOK, `"total":301`},
		{http.MethodGet, job + "/items/device-301", "", http.StatusOK, `"device_id":"device-301"`},
		{http.MethodPost, job + "/items/device-301/dispatch", `{}`, http.StatusAccepted, `"operation_id":"same-operation"`},
		{http.MethodPost, job + "/cancel", `{}`, http.StatusOK, `"status":"canceled"`},
		{http.MethodPost, job + "/complete", `{}`, http.StatusOK, `"status":"completed"`},
	}
	for _, request := range requests {
		response := productApplyHandlerRequest(router, request.method, request.path, request.body)
		if response.Code != request.status || !strings.Contains(response.Body.String(), request.contains) {
			t.Fatalf("%s %s = %d %s, want status %d and %s", request.method, request.path, response.Code, response.Body.String(), request.status, request.contains)
		}
	}
	if len(backend.calls) != len(requests) {
		t.Fatalf("store calls = %d, want %d", len(backend.calls), len(requests))
	}
	for _, call := range backend.calls {
		if call.actor != "authenticated-actor" || call.cloud != "cloud-1" || call.product != "product-1" {
			t.Fatalf("request lost authenticated scope: %+v", call)
		}
	}
	if call := backend.calls[1]; call.jobID != "job-1" || call.previewToken != "preview-token" {
		t.Fatalf("admission did not trim stable job/token identifiers: %+v", call)
	}
	if call := backend.calls[3]; call.limit != 200 || call.offset != 250 {
		t.Fatalf("pagination cannot reach devices past 250: %+v", call)
	}
	if call := backend.calls[5]; call.deviceID != "device-301" || call.messageID == "" {
		t.Fatalf("dispatch omitted target or message identifier: %+v", call)
	}
	if backend.calls[6].action != "cancel" || backend.calls[7].action != "complete" {
		t.Fatalf("finish actions = %+v, %+v", backend.calls[6], backend.calls[7])
	}

	backend.itemStatus = "applied"
	response := productApplyHandlerRequest(router, http.MethodPost, job+"/items/device-301/dispatch", `{}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"applied"`) {
		t.Fatalf("applied receipt = %d %s", response.Code, response.Body.String())
	}
}

func TestProductServiceApplyHTTPRejectsInvalidAndConflictingRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	backend := &productApplyHandlerStore{}
	router := productApplyHandlerRouter(New(backend, nil))
	base := "/v1/orgs/cloud-1/device-item-profiles/product-1"
	job := base + "/service-apply-jobs/job-1"
	for _, request := range []struct{ method, path, body string }{
		{http.MethodPost, base + "/service-apply-jobs", `{"job_id":"job-1"}`},
		{http.MethodPost, base + "/service-apply-jobs", `{"job_id":"  ","preview_token":"preview-token"}`},
		{http.MethodPost, job + "/items/device-301/dispatch", `{"unexpected":true}`},
		{http.MethodPost, job + "/cancel", `{"unexpected":true}`},
	} {
		response := productApplyHandlerRequest(router, request.method, request.path, request.body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid %s %s = %d %s", request.method, request.path, response.Code, response.Body.String())
		}
	}
	if len(backend.calls) != 0 {
		t.Fatalf("invalid requests reached store: %+v", backend.calls)
	}
	backend.failOn = "admit"
	response := productApplyHandlerRequest(router, http.MethodPost, base+"/service-apply-jobs", `{"job_id":"job-1","preview_token":"stale"}`)
	if response.Code != http.StatusConflict || len(backend.calls) != 1 {
		t.Fatalf("stale preview admission = %d %s, calls=%+v", response.Code, response.Body.String(), backend.calls)
	}
	backend.failOn = "dispatch"
	response = productApplyHandlerRequest(router, http.MethodPost, job+"/items/device-301/dispatch", `{}`)
	if response.Code != http.StatusConflict || len(backend.calls) != 2 {
		t.Fatalf("concurrent device grant dispatch = %d %s, calls=%+v", response.Code, response.Body.String(), backend.calls)
	}
	for _, request := range []struct{ operation, method, path, body string }{
		{"preview", http.MethodGet, base + "/service-apply-preview", ""},
		{"get_job", http.MethodGet, job, ""},
		{"list_items", http.MethodGet, job + "/items?offset=250", ""},
		{"get_item", http.MethodGet, job + "/items/device-301", ""},
		{"finish", http.MethodPost, job + "/complete", `{}`},
	} {
		backend.failOn = request.operation
		before := len(backend.calls)
		response := productApplyHandlerRequest(router, request.method, request.path, request.body)
		if response.Code != http.StatusConflict || len(backend.calls) != before+1 {
			t.Fatalf("%s conflict = %d %s, calls=%+v", request.operation, response.Code, response.Body.String(), backend.calls)
		}
	}
}

func TestProductServiceApplyRoutesRequireFeatureGateAndAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := "/v1/orgs/cloud-1/device-item-profiles/product-1/service-apply-preview"
	for _, test := range []struct {
		name, path string
		enabled    bool
		want       int
	}{
		{name: "disabled", path: path, want: http.StatusNotFound},
		{name: "enabled_requires_auth", path: path, enabled: true, want: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := New(&productApplyHandlerStore{}, auth.NewService("access-secret", "refresh-secret", time.Minute, time.Hour))
			server.ConfigurePlatformServiceProductWrites(test.enabled)
			response := productApplyHandlerRequest(server.Router(), http.MethodGet, test.path, "")
			if response.Code != test.want {
				t.Fatalf("Product apply route = %d %s, want %d", response.Code, response.Body.String(), test.want)
			}
		})
	}
}
