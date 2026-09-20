package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"rtk_account_manager/internal/store"
)

type stubPlatformServices struct {
	registered bool
}

func (*stubPlatformServices) ApprovePlatformServiceWorkload(context.Context, store.PlatformServiceWorkloadApproval) error {
	return nil
}
func (*stubPlatformServices) RevokePlatformServiceWorkload(context.Context, store.PlatformServiceWorkloadRevocation, time.Time) error {
	return nil
}
func (*stubPlatformServices) SetPlatformServiceStatus(context.Context, string, string, string, string) (int64, error) {
	return 1, nil
}
func (s *stubPlatformServices) RegisterPlatformServiceInstance(_ context.Context, _ store.PlatformServiceRegistration, _ store.PlatformServicePrincipal, _ time.Time) (store.PlatformServiceLease, error) {
	s.registered = true
	return store.PlatformServiceLease{ServiceID: "mqtt"}, nil
}
func (*stubPlatformServices) HeartbeatPlatformServiceInstance(context.Context, string, string, string, bool, store.PlatformServicePrincipal, time.Time) (store.PlatformServiceLease, error) {
	return store.PlatformServiceLease{}, nil
}
func (*stubPlatformServices) DeregisterPlatformServiceInstance(context.Context, string, string, store.PlatformServicePrincipal, time.Time) error {
	return nil
}
func (*stubPlatformServices) PublishPlatformServiceManifest(context.Context, string, string, string, store.PlatformServicePrincipal, time.Time) (int64, error) {
	return 1, nil
}
func (*stubPlatformServices) ListPlatformServiceOptions(context.Context, string, time.Time) (store.PlatformServiceCatalog, error) {
	return store.PlatformServiceCatalog{}, nil
}

func TestServiceRegistrationRequiresVerifiedClientChain(t *testing.T) {
	backend := &stubPlatformServices{}
	server := New(nil, nil)
	server.ConfigurePlatformServices(backend, "staging")
	router := server.ServiceRouter()
	url := "/v1/platform/services/mqtt/instances/mqtt-1"
	for _, tlsState := range []*tls.ConnectionState{nil, {}, {PeerCertificates: []*x509.Certificate{{}}}} {
		req := httptest.NewRequest(http.MethodPut, url, strings.NewReader(`{"request_id":"r"}`))
		req.TLS = tlsState
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		if res.Code != 401 || backend.registered {
			t.Fatalf("unverified client reached registry: %d", res.Code)
		}
	}
}

type recordingPlatformServices struct {
	stubPlatformServices
	err          error
	approved     store.PlatformServiceWorkloadApproval
	revoked      store.PlatformServiceWorkloadRevocation
	status       string
	deregistered string
	published    string
	registered   store.PlatformServiceRegistration
}

func (s *recordingPlatformServices) ApprovePlatformServiceWorkload(_ context.Context, approval store.PlatformServiceWorkloadApproval) error {
	s.approved = approval
	return s.err
}

func (s *recordingPlatformServices) RevokePlatformServiceWorkload(_ context.Context, revocation store.PlatformServiceWorkloadRevocation, _ time.Time) error {
	s.revoked = revocation
	return s.err
}

func (s *recordingPlatformServices) SetPlatformServiceStatus(_ context.Context, _, _, status, _ string) (int64, error) {
	s.status = status
	return 17, s.err
}

func (s *recordingPlatformServices) DeregisterPlatformServiceInstance(_ context.Context, _, instanceID string, _ store.PlatformServicePrincipal, _ time.Time) error {
	s.deregistered = instanceID
	return s.err
}

func (s *recordingPlatformServices) PublishPlatformServiceManifest(_ context.Context, _, _, manifestVersion string, _ store.PlatformServicePrincipal, _ time.Time) (int64, error) {
	s.published = manifestVersion
	return 17, s.err
}

func (s *recordingPlatformServices) RegisterPlatformServiceInstance(_ context.Context, registration store.PlatformServiceRegistration, _ store.PlatformServicePrincipal, _ time.Time) (store.PlatformServiceLease, error) {
	s.registered = registration
	return store.PlatformServiceLease{ServiceID: registration.ServiceID, InstanceID: registration.InstanceID}, s.err
}

func (s *recordingPlatformServices) HeartbeatPlatformServiceInstance(_ context.Context, serviceID, instanceID, _ string, _ bool, _ store.PlatformServicePrincipal, _ time.Time) (store.PlatformServiceLease, error) {
	return store.PlatformServiceLease{ServiceID: serviceID, InstanceID: instanceID}, s.err
}

func platformServiceRequest(router http.Handler, method, path, body string, verified bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if verified {
		request.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{
			{Subject: pkix.Name{CommonName: "service-mqtt"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)},
			{Raw: []byte("issuer")},
		}}}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestPlatformServiceAdministrativeRoutes(t *testing.T) {
	backend := &recordingPlatformServices{}
	server := New(nil, nil)
	server.ConfigurePlatformServices(backend, "staging")
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "operator"); c.Next() })
	router.POST("/approve", server.approvePlatformServiceWorkload)
	router.POST("/revoke", server.revokePlatformServiceWorkload)
	router.PATCH("/status/:serviceId", server.setPlatformServiceStatus)

	approval := `{"service_id":"mqtt","instance_id":"mqtt-1","certificate_subject":"service-mqtt","issuer_fingerprint_sha256":"fingerprint","allowed_option_codes":["mqtt"]}`
	revocation := `{"service_id":"mqtt","instance_id":"mqtt-1","certificate_subject":"service-mqtt","issuer_fingerprint_sha256":"fingerprint"}`
	if response := platformServiceRequest(router, http.MethodPost, "/approve", approval, false); response.Code != http.StatusNoContent {
		t.Fatalf("approval: %d %s", response.Code, response.Body.String())
	}
	if backend.approved.Environment != "staging" || backend.approved.ApprovedBy != "operator" || backend.approved.ServiceID != "mqtt" {
		t.Fatalf("approval lost authenticated scope: %+v", backend.approved)
	}
	if response := platformServiceRequest(router, http.MethodPost, "/revoke", revocation, false); response.Code != http.StatusNoContent {
		t.Fatalf("revocation: %d %s", response.Code, response.Body.String())
	}
	if backend.revoked.Environment != "staging" || backend.revoked.RevokedBy != "operator" || backend.revoked.InstanceID != "mqtt-1" {
		t.Fatalf("revocation lost authenticated scope: %+v", backend.revoked)
	}
	if response := platformServiceRequest(router, http.MethodPatch, "/status/mqtt", `{"status":"active"}`, false); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"catalog_revision":17`) {
		t.Fatalf("status publication: %d %s", response.Code, response.Body.String())
	}
	if backend.status != "active" {
		t.Fatalf("status not forwarded: %q", backend.status)
	}

	backend.err = store.ErrServiceRegistrationDenied
	for _, request := range []struct{ method, path, body string }{
		{http.MethodPost, "/approve", approval},
		{http.MethodPost, "/revoke", revocation},
		{http.MethodPatch, "/status/mqtt", `{"status":"active"}`},
	} {
		if response := platformServiceRequest(router, request.method, request.path, request.body, false); response.Code != http.StatusForbidden {
			t.Fatalf("%s %s denied: %d %s", request.method, request.path, response.Code, response.Body.String())
		}
	}
	backend.err = nil
	for _, request := range []struct{ method, path string }{
		{http.MethodPost, "/approve"},
		{http.MethodPost, "/revoke"},
		{http.MethodPatch, "/status/mqtt"},
	} {
		if response := platformServiceRequest(router, request.method, request.path, `{"unexpected":true}`, false); response.Code != http.StatusBadRequest {
			t.Fatalf("%s %s invalid body: %d", request.method, request.path, response.Code)
		}
	}
	server.ConfigurePlatformServices(nil, "staging")
	for _, request := range []struct{ method, path string }{
		{http.MethodPost, "/approve"},
		{http.MethodPost, "/revoke"},
		{http.MethodPatch, "/status/mqtt"},
	} {
		if response := platformServiceRequest(router, request.method, request.path, `{}`, false); response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s unavailable: %d", request.method, request.path, response.Code)
		}
	}
}

func TestPlatformServiceMTLSRouteLifecycle(t *testing.T) {
	backend := &recordingPlatformServices{}
	server := New(nil, nil)
	server.ConfigurePlatformServices(backend, "staging")
	router := server.ServiceRouter()
	base := "/v1/platform/services/mqtt"
	registration := `{"request_id":"request-1","service_id":"mqtt","instance_id":"mqtt-1","manifest_version":"v1","protocol_version":"1","options":[{"code":"mqtt","display_name":"MQTT"}],"endpoint_ref":"mqtt-1","ready":true}`
	if response := platformServiceRequest(router, http.MethodPut, base+"/instances/mqtt-1", registration, true); response.Code != http.StatusOK || backend.registered.ServiceID != "mqtt" || !backend.registered.Ready {
		t.Fatalf("registration: %d %s, recorded %+v", response.Code, response.Body.String(), backend.registered)
	}
	if response := platformServiceRequest(router, http.MethodPut, base+"/instances/mqtt-2", registration, true); response.Code != http.StatusBadRequest {
		t.Fatalf("route/manifest identity mismatch: %d", response.Code)
	}
	if response := platformServiceRequest(router, http.MethodPost, base+"/instances/mqtt-1/heartbeat", `{"ready":true}`, true); response.Code != http.StatusBadRequest {
		t.Fatalf("missing manifest version: %d", response.Code)
	}
	if response := platformServiceRequest(router, http.MethodPost, base+"/instances/mqtt-1/heartbeat", `{"manifest_version":"v1","ready":true}`, true); response.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", response.Code, response.Body.String())
	}
	if response := platformServiceRequest(router, http.MethodPatch, base+"/publication", `{"expected_version":"v0","manifest_version":"v1"}`, true); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"catalog_revision":17`) {
		t.Fatalf("publication: %d %s", response.Code, response.Body.String())
	}
	if backend.published != "v1" {
		t.Fatalf("manifest version not forwarded: %q", backend.published)
	}
	if response := platformServiceRequest(router, http.MethodDelete, base+"/instances/mqtt-1", "", true); response.Code != http.StatusNoContent || backend.deregistered != "mqtt-1" {
		t.Fatalf("deregister: %d %q", response.Code, backend.deregistered)
	}

	backend.err = store.ErrConflict
	for _, request := range []struct{ method, path, body string }{
		{http.MethodPut, base + "/instances/mqtt-1", registration},
		{http.MethodPost, base + "/instances/mqtt-1/heartbeat", `{"manifest_version":"v1","ready":true}`},
		{http.MethodPatch, base + "/publication", `{"manifest_version":"v2"}`},
		{http.MethodDelete, base + "/instances/mqtt-1", ""},
	} {
		if response := platformServiceRequest(router, request.method, request.path, request.body, true); response.Code != http.StatusConflict {
			t.Fatalf("%s %s conflict: %d", request.method, request.path, response.Code)
		}
	}
	if response := platformServiceRequest(router, http.MethodPatch, base+"/publication", `{"manifest_version":"v1","unexpected":true}`, true); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown publication field: %d", response.Code)
	}
	expired := httptest.NewRequest(http.MethodPut, base+"/instances/mqtt-1", strings.NewReader(registration))
	expired.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{
		{Subject: pkix.Name{CommonName: "service-mqtt"}, NotBefore: time.Now().Add(-2 * time.Hour), NotAfter: time.Now().Add(-time.Hour)},
		{Raw: []byte("issuer")},
	}}}
	expiredResponse := httptest.NewRecorder()
	router.ServeHTTP(expiredResponse, expired)
	if expiredResponse.Code != http.StatusUnauthorized {
		t.Fatalf("expired service certificate: %d", expiredResponse.Code)
	}
	server.ConfigurePlatformServices(nil, "staging")
	if response := platformServiceRequest(router, http.MethodPut, base+"/instances/mqtt-1", registration, true); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable registry: %d", response.Code)
	}
}

func TestPlatformServiceProductRejectsEmptyGrant(t *testing.T) {
	server := New(nil, nil)
	server.ConfigurePlatformServiceProductWrites(true)
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	if options, ok := server.productServiceOptions(context, nil); ok || options != nil || response.Code != http.StatusBadRequest {
		t.Fatalf("empty registered-service grant accepted: %d %+v", response.Code, options)
	}
}

func TestPlatformServiceErrorMappings(t *testing.T) {
	for _, test := range []struct {
		err  error
		code int
	}{
		{store.ErrServiceRegistrationInvalid, http.StatusBadRequest},
		{store.ErrServiceRegistrationDenied, http.StatusForbidden},
		{store.ErrNotFound, http.StatusNotFound},
		{store.ErrConflict, http.StatusConflict},
		{errors.New("database offline"), http.StatusServiceUnavailable},
	} {
		router := gin.New()
		router.GET("/", func(c *gin.Context) { platformServiceError(c, test.err) })
		if response := platformServiceRequest(router, http.MethodGet, "/", "", false); response.Code != test.code {
			t.Errorf("%v: got %d, want %d", test.err, response.Code, test.code)
		}
	}
}
