package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
