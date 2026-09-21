package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type stubVideoPresenceReader struct {
	snapshot        videoPresenceSnapshot
	err             error
	deviceID        string
	reservationID   string
	targetOrgID     string
	accountDeviceID string
}

func (s *stubVideoPresenceReader) ReserveTransferFence(_ context.Context, deviceID, reservationID, targetOrgID, accountDeviceID string) error {
	s.deviceID, s.reservationID, s.targetOrgID, s.accountDeviceID = deviceID, reservationID, targetOrgID, accountDeviceID
	if errors.Is(s.err, errVideoPresenceNotFound) {
		return nil
	}
	if s.err != nil {
		return s.err
	}
	return store.ErrClaimLifecycleBound
}

func (s *stubVideoPresenceReader) ReleaseTransferFence(_ context.Context, deviceID, reservationID string) error {
	if s.err != nil {
		return s.err
	}
	if s.deviceID != deviceID || s.reservationID != reservationID {
		return store.ErrClaimLifecycleBound
	}
	s.reservationID = ""
	return nil
}

func (s *stubVideoPresenceReader) ReadTransferFence(_ context.Context, deviceID string) (store.DeviceTransferFence, error) {
	s.deviceID = deviceID
	if s.err != nil {
		return store.DeviceTransferFence{}, s.err
	}
	if s.reservationID == "" {
		return store.DeviceTransferFence{}, store.ErrNotFound
	}
	return store.DeviceTransferFence{
		DeviceID: deviceID, ReservationID: s.reservationID,
		OrganizationID: s.targetOrgID, AccountDeviceID: s.accountDeviceID,
	}, nil
}

func (s *stubVideoPresenceReader) ReadPresence(_ context.Context, deviceID string) (videoPresenceSnapshot, error) {
	s.deviceID = deviceID
	return s.snapshot, s.err
}

func TestConfigureVideoPresenceValidatesInternalOrigin(t *testing.T) {
	for _, tt := range []struct {
		name, origin, token string
		wantError           bool
	}{
		{"disabled", "", "", false},
		{"valid", "https://video.example.test", "secret", false},
		{"missing token", "https://video.example.test", "", true},
		{"token without origin", "", "secret", true},
		{"URL credentials", "https://user:pass@video.example.test", "secret", true},
		{"URL query", "https://video.example.test?token=x", "secret", true},
		{"URL path", "https://video.example.test/api", "secret", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := New(nil, nil)
			err := server.ConfigureVideoPresence(tt.origin, tt.token, time.Second)
			if (err != nil) != tt.wantError {
				t.Fatalf("ConfigureVideoPresence error = %v, wantError=%t", err, tt.wantError)
			}
		})
	}
}

func TestTransferFenceRequiresAuthenticatedDurableReceipt(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer secret" ||
			r.Header.Get("Content-Type") != "application/json" ||
			!strings.HasSuffix(r.URL.Path, "/transfer-fence") {
			t.Errorf("unexpected transfer fence request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body struct {
			ReservationID   string `json:"reservation_id"`
			OrgID           string `json:"org_id"`
			AccountDeviceID string `json:"account_device_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ReservationID != "claim-1" ||
			body.OrgID != "new-org" || body.AccountDeviceID != "account-1" {
			t.Errorf("invalid reservation body: %+v err=%v", body, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "/absent/"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":"ok","reservation_id":"claim-1"}`))
		case strings.Contains(r.URL.Path, "/bound/"):
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"status":"fail","reason":"cloud_lifecycle_bound"}`))
		case strings.Contains(r.URL.Path, "/bad-receipt/"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":"ok","reservation_id":"other"}`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer upstream.Close()
	server := New(nil, nil)
	if err := server.ConfigureVideoPresence(upstream.URL, "secret", time.Second); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		deviceID string
		want     error
	}{
		{"absent", nil},
		{"bound", store.ErrClaimLifecycleBound},
		{"bad-receipt", store.ErrClaimLifecycleCheckUnavailable},
		{"unavailable", store.ErrClaimLifecycleCheckUnavailable},
	} {
		err := server.reserveNoVideoCloudLifecycle(context.Background(), tt.deviceID, "new-org", "account-1", "claim-1")
		if tt.want == nil && err != nil || tt.want != nil && !errors.Is(err, tt.want) {
			t.Errorf("reserve %s error = %v, want %v", tt.deviceID, err, tt.want)
		}
	}
}

func TestTransferFenceCancellationRequiresAuthenticatedAcknowledgement(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.Header.Get("Authorization") != "Bearer secret" ||
			r.Header.Get("Content-Type") != "application/json" ||
			r.URL.Path != "/v1/internal/account-manager/devices/video-1/transfer-fence" {
			t.Errorf("unexpected cancellation request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body struct {
			ReservationID string `json:"reservation_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ReservationID != "claim-1" {
			t.Errorf("invalid cancellation body: %+v err=%v", body, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	server := New(nil, nil)
	if err := server.ConfigureVideoPresence(upstream.URL, "secret", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := server.releaseNoVideoCloudLifecycle(context.Background(), "video-1", "claim-1"); err != nil {
		t.Fatal(err)
	}
}

func TestTransferFenceLookupRequiresAuthenticatedExactBinding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("unexpected lookup request: %s auth=%q", r.Method, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "/present/"):
			_, _ = w.Write([]byte(`{"device_id":"present","reservation_id":"claim-1","org_id":"new-org","account_device_id":"account-1"}`))
		case strings.Contains(r.URL.Path, "/absent/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":"fail","reason":"transfer_fence_not_found"}`))
		case strings.Contains(r.URL.Path, "/wrong-binding/"):
			_, _ = w.Write([]byte(`{"device_id":"other","reservation_id":"claim-1","org_id":"new-org","account_device_id":"account-1"}`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer upstream.Close()
	server := New(nil, nil)
	if err := server.ConfigureVideoPresence(upstream.URL, "secret", time.Second); err != nil {
		t.Fatal(err)
	}
	client := server.videoPresence.(videoTransferFencer)
	if fence, err := client.ReadTransferFence(context.Background(), "present"); err != nil || fence.ReservationID != "claim-1" {
		t.Fatalf("read fence = %+v, %v", fence, err)
	}
	if _, err := client.ReadTransferFence(context.Background(), "absent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("absent fence error = %v", err)
	}
	if _, err := client.ReadTransferFence(context.Background(), "wrong-binding"); err == nil {
		t.Fatal("mismatched device binding accepted")
	}
}

func TestHTTPVideoPresenceReaderUsesInternalBearerAndDecodesSnapshot(t *testing.T) {
	observedAt := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/internal/account-manager/devices/video-1/presence" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("unexpected Video Cloud request: %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(videoPresenceSnapshot{
			DeviceID: "video-1", OrgID: "org-1", AccountDeviceID: "device-1",
			Activated: true, Online: true, OwnerTransport: "mqtt", ObservedAt: observedAt,
		})
	}))
	defer upstream.Close()
	server := New(nil, nil)
	if err := server.ConfigureVideoPresence(upstream.URL, "secret", time.Second); err != nil {
		t.Fatal(err)
	}
	snapshot, err := server.videoPresence.ReadPresence(context.Background(), "video-1")
	if err != nil || snapshot.DeviceID != "video-1" || snapshot.OwnerTransport != "mqtt" || !snapshot.ObservedAt.Equal(observedAt) {
		t.Fatalf("presence snapshot = %+v, %v", snapshot, err)
	}
}

func TestHTTPVideoPresenceReaderRejectsRedirectAndOversizedSnapshot(t *testing.T) {
	redirectTargetCalled := false
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectTargetCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oversize/") {
			_, _ = w.Write([]byte(`{"devid":"` + strings.Repeat("x", 8200) + `"}`))
			return
		}
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	defer upstream.Close()
	server := New(nil, nil)
	if err := server.ConfigureVideoPresence(upstream.URL, "secret", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := server.videoPresence.ReadPresence(context.Background(), "video-1"); err == nil || redirectTargetCalled {
		t.Fatalf("redirect was followed or accepted: err=%v targetCalled=%t", err, redirectTargetCalled)
	}
	if _, err := server.videoPresence.ReadPresence(context.Background(), "oversize"); err == nil {
		t.Fatal("oversized presence response was accepted")
	}
}

func TestHTTPVideoPresenceReaderDistinguishesAbsentDeviceFromMissingRoute(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		if strings.Contains(r.URL.Path, "malformed-device") {
			_, _ = w.Write([]byte(`{"status":"fail","reason":"device not found"} true`))
			return
		}
		if strings.Contains(r.URL.Path, "absent-device") {
			_, _ = w.Write([]byte(`{"status":"fail","reason":"device not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"fail","reason":"not found"}`))
	}))
	defer upstream.Close()
	server := New(nil, nil)
	if err := server.ConfigureVideoPresence(upstream.URL, "secret", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := server.videoPresence.ReadPresence(context.Background(), "absent-device"); !errors.Is(err, errVideoPresenceNotFound) {
		t.Fatalf("absent device error = %v", err)
	}
	if _, err := server.videoPresence.ReadPresence(context.Background(), "missing-route"); err == nil || errors.Is(err, errVideoPresenceNotFound) {
		t.Fatalf("missing route must not prove device absence: %v", err)
	}
	if _, err := server.videoPresence.ReadPresence(context.Background(), "malformed-device"); err == nil || errors.Is(err, errVideoPresenceNotFound) {
		t.Fatalf("malformed 404 must not prove device absence: %v", err)
	}
}

func TestLivePresenceOverridesStaleRegistryProjection(t *testing.T) {
	observedAt := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	device := model.Device{
		ID: "device-1", OrganizationID: "org-1", Status: model.DeviceStatusOnline,
		Metadata: map[string]any{
			model.DeviceMetadataVideoCloudDevid:            "video-1",
			model.DeviceMetadataVideoCloudActivationStatus: string(model.VideoCloudActivationStatusActivated),
		},
	}
	operation := model.DeviceOperation{Status: model.DeviceOperationStatusSucceeded}
	base := readinessFromProjection(device, &operation, nil)
	if base.State != model.DeviceReadinessStateTransportPending {
		t.Fatalf("missing projected observation must be pending before live read: %+v", base)
	}
	for _, tt := range []struct {
		name       string
		snapshot   videoPresenceSnapshot
		err        error
		wantStatus string
		wantReady  bool
	}{
		{"current owner", videoPresenceSnapshot{DeviceID: "video-1", OrgID: "org-1", AccountDeviceID: "device-1", Activated: true, Online: true, OwnerTransport: "websocket", ObservedAt: observedAt}, nil, "online", true},
		{"owner gone", videoPresenceSnapshot{DeviceID: "video-1", OrgID: "org-1", AccountDeviceID: "device-1", Activated: true, Online: false, ObservedAt: observedAt}, nil, "offline", false},
		{"missing owner despite online bit", videoPresenceSnapshot{DeviceID: "video-1", OrgID: "org-1", AccountDeviceID: "device-1", Activated: true, Online: true, ObservedAt: observedAt}, nil, "offline", false},
		{"source unavailable", videoPresenceSnapshot{}, errors.New("timeout"), "unavailable", false},
		{"identity mismatch", videoPresenceSnapshot{DeviceID: "video-1", OrgID: "other-org", AccountDeviceID: "device-1", Activated: true, Online: true, OwnerTransport: "mqtt", ObservedAt: observedAt}, nil, "identity_mismatch", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader := &stubVideoPresenceReader{snapshot: tt.snapshot, err: tt.err}
			server := New(nil, nil)
			server.videoPresence = reader
			got := server.readinessWithLivePresence(context.Background(), device, base)
			if reader.deviceID != "video-1" || got.Sources.OwnerTransportStatus != tt.wantStatus ||
				(got.State == model.DeviceReadinessStateReady) != tt.wantReady ||
				(got.ProductState == model.ProductReadinessStateOnline) != tt.wantReady {
				t.Fatalf("readiness = %+v, requested device = %q", got, reader.deviceID)
			}
		})
	}
}
