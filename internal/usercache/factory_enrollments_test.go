package usercache

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"rtk_account_manager/internal/api"
	"rtk_account_manager/internal/store"
)

type factoryBacking struct {
	api.Store
	admissions  []store.FactoryEnrollmentAdmission
	results     []store.FactoryEnrollmentResult
	reservation store.FactoryEnrollmentReservation
}

func (s *factoryBacking) ReserveFactoryEnrollment(_ context.Context, in store.FactoryEnrollmentAdmission) (store.FactoryEnrollmentReservation, error) {
	s.admissions = append(s.admissions, in)
	return s.reservation, nil
}

func (s *factoryBacking) LookupFactoryEnrollment(_ context.Context, in store.FactoryEnrollmentAdmission) (store.FactoryEnrollmentReservation, error) {
	s.admissions = append(s.admissions, in)
	return s.reservation, nil
}

func (s *factoryBacking) CompleteFactoryEnrollment(_ context.Context, in store.FactoryEnrollmentResult) (store.FactoryEnrollmentReservation, error) {
	s.results = append(s.results, in)
	s.reservation.Status, s.reservation.EvidenceSHA256 = in.Status, &in.EvidenceSHA256
	return s.reservation, nil
}

// Match cmd/server's api.Store -> cache wrapper -> api.New composition. Factory
// coordination must always reach durable persistence, never the user cache.
func TestStoreFactoryCoordinationRoutesBypassUserCache(t *testing.T) {
	scope := store.FactoryEnrollmentAdmission{RunID: "run", CloudID: "cloud", ProductID: "product", RequestID: "request", DeviceID: "device", RequestSHA256: strings.Repeat("a", 64)}
	backing := &factoryBacking{reservation: store.FactoryEnrollmentReservation{ID: "reservation", RunID: scope.RunID, RequestID: scope.RequestID, DeviceID: scope.DeviceID, RequestSHA256: scope.RequestSHA256, Status: "reserved"}}
	var apiStore api.Store = backing
	// A nil cache makes any accidental use of user caching fail this test.
	apiStore = NewStore(apiStore, nil, nil)
	server := api.New(apiStore, nil)
	const credential = "isolated-factory-coordination-service-key"
	const signingKey = "isolated-production-jwt-signing-key"
	server.ConfigureFactoryEnrollmentToken(credential)
	server.ConfigureProductionJWT(signingKey, "factory-enroll")
	router := server.Router()
	now := time.Now()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "factory_production_run:run", "aud": "factory-enroll", "jti": "run-token",
		"nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
		"production_run_id": scope.RunID, "brand_cloud_id": scope.CloudID,
		"device_item_profile_id": scope.ProductID, "allowed_quantity": 1,
	}).SignedString([]byte(signingKey))
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"production_run_id": scope.RunID, "brand_cloud_id": scope.CloudID, "device_item_profile_id": scope.ProductID, "request_id": scope.RequestID, "devid": scope.DeviceID, "request_sha256": scope.RequestSHA256, "production_jwt": token}
	call := func(path string) {
		t.Helper()
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/v1/internal/factory-enrollments/"+path, strings.NewReader(string(payload)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+credential)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	call("reserve")
	delete(body, "production_jwt")
	call("lookup")
	body["status"], body["evidence_sha256"] = "issued", strings.Repeat("b", 64)
	call("reservation/result")
	call("reservation/result")
	if len(backing.admissions) != 4 || len(backing.results) != 2 {
		t.Fatal("factory operations were cached or not forwarded")
	}
	for _, got := range backing.admissions {
		if !reflect.DeepEqual(got, scope) {
			t.Fatal("factory admission scope changed through cache wrapper")
		}
	}
	for _, got := range backing.results {
		if got.ReservationID != "reservation" || got.RunID != scope.RunID || got.CloudID != scope.CloudID || got.RequestSHA256 != scope.RequestSHA256 || got.Status != "issued" || got.EvidenceSHA256 != strings.Repeat("b", 64) {
			t.Fatal("factory result binding changed through cache wrapper")
		}
	}
}
