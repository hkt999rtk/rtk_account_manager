package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

const factoryCoordinationTestToken = "isolated-factory-service-credential-123456"

func factoryHTTPFixture(t *testing.T, env integrationEnv) (map[string]any, string, string) {
	t.Helper()
	ctx := context.Background()
	owner := verifiedDeveloperForTest(t, env, "factory-http-coordination@example.test")
	p, err := env.store.CreateDeviceItemProfileAsUser(ctx, store.DeviceItemProfileCreateInput{ActorUserID: &owner.UserID, BrandCloudID: owner.BrandCloudID, ProfileKey: "factory-http", DisplayName: "Factory", Category: model.DeviceCategoryIPCamera, CAProfile: "ca", IssuerProfile: "issuer", ServiceOptions: []string{"mqtt"}})
	if err != nil {
		t.Fatal(err)
	}
	env.server.ConfigureProductionJWT("isolated-production-signing-secret", "factory-enroll")
	now := time.Now().UTC()
	run, token, err := env.store.IssueProductionRunAsUser(ctx, store.ProductionRunCreateInput{ActorUserID: &owner.UserID, BrandCloudID: owner.BrandCloudID, DeviceItemProfileID: p.ID, AllowedQuantity: 1, ValidFrom: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour)}, env.server.signProductionJWT)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"production_run_id": run.ID, "brand_cloud_id": run.BrandCloudID, "device_item_profile_id": p.ID, "request_id": "factory-request-1", "devid": "isolated-device", "request_sha256": strings.Repeat("a", 64), "production_jwt": token}, owner.UserID, owner.AccessToken
}

func TestIntegrationFactoryCoordinationBindsCredentialsScopeAndReconciliation(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()
	body, owner, access := factoryHTTPFixture(t, env)
	reserve := "/v1/internal/factory-enrollments/reserve"
	if r := performJSON(env.router, http.MethodPost, reserve, body, factoryCoordinationTestToken); r.Code != 503 {
		t.Fatalf("unconfigured: %d", r.Code)
	}
	env.server.ConfigureFactoryEnrollmentToken(factoryCoordinationTestToken)
	env.server.ConfigureInternalAuthToken("different-internal-service")
	for _, token := range []string{"", access, "different-internal-service"} {
		if r := performJSON(env.router, http.MethodPost, reserve, body, token); r.Code != 401 {
			t.Fatalf("wrong credential: %d", r.Code)
		}
	}
	contract := newResponseContract(t)
	r := performJSON(env.router, http.MethodPost, reserve, body, factoryCoordinationTestToken)
	if r.Code != 200 {
		t.Fatalf("reserve: %d %s", r.Code, r.Body.String())
	}
	contract.validate(t, http.MethodPost, reserve, r)
	var first factoryEnrollmentResponse
	if err := json.Unmarshal(r.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Status != "reserved" || first.ReservationID == "" || first.RequestID != "factory-request-1" || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("invalid reservation response")
	}
	r = performJSON(env.router, http.MethodPost, reserve, body, factoryCoordinationTestToken)
	var replay factoryEnrollmentResponse
	json.Unmarshal(r.Body.Bytes(), &replay)
	if r.Code != 200 || replay.ReservationID != first.ReservationID {
		t.Fatal("reservation replay allocated again")
	}
	body["request_id"] = "other-request"
	if r := performJSON(env.router, http.MethodPost, reserve, body, factoryCoordinationTestToken); r.Code != 409 {
		t.Fatalf("quota bypass: %d", r.Code)
	}
	body["request_id"] = "factory-request-1"
	body["request_sha256"] = strings.Repeat("b", 64)
	if r := performJSON(env.router, http.MethodPost, reserve, body, factoryCoordinationTestToken); r.Code != 409 {
		t.Fatalf("changed request accepted: %d", r.Code)
	}
	body["request_sha256"] = strings.Repeat("a", 64)
	// Revoked actors cannot create/replay new admission, but authenticated service
	// reconciliation must still close the already admitted work.
	if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, owner); err != nil {
		t.Fatal(err)
	}
	if r := performJSON(env.router, http.MethodPost, reserve, body, factoryCoordinationTestToken); r.Code != 404 {
		t.Fatalf("revoked actor admitted: %d", r.Code)
	}
	delete(body, "production_jwt")
	lookup := "/v1/internal/factory-enrollments/lookup"
	r = performJSON(env.router, http.MethodPost, lookup, body, factoryCoordinationTestToken)
	if r.Code != 200 {
		t.Fatalf("reconciliation read: %d", r.Code)
	}
	contract.validate(t, http.MethodPost, lookup, r)
	path := "/v1/internal/factory-enrollments/" + first.ReservationID + "/result"
	body["status"] = "issued"
	body["evidence_sha256"] = strings.Repeat("c", 64)
	for range 2 {
		r = performJSON(env.router, http.MethodPost, path, body, factoryCoordinationTestToken)
		if r.Code != 200 {
			t.Fatalf("complete/replay: %d %s", r.Code, r.Body.String())
		}
		contract.validate(t, http.MethodPost, path, r)
	}
	body["status"] = "not_issued"
	if r := performJSON(env.router, http.MethodPost, path, body, factoryCoordinationTestToken); r.Code != 409 {
		t.Fatalf("terminal result changed: %d", r.Code)
	}
	body["status"] = "issued"
	body["brand_cloud_id"] = owner
	if r := performJSON(env.router, http.MethodPost, path, body, factoryCoordinationTestToken); r.Code != 404 {
		t.Fatalf("wrong cloud reconciled: %d", r.Code)
	}
	var issued int
	if err := env.db.QueryRow(ctx, `SELECT issued_quantity FROM factory_production_runs WHERE id=$1`, first.RunID).Scan(&issued); err != nil || issued != 1 {
		t.Fatalf("issued=%d %v", issued, err)
	}
}

func TestIntegrationFactoryCoordinationRejectsInvalidProductionJWTs(t *testing.T) {
	env := newIntegrationEnv(t)
	body, _, _ := factoryHTTPFixture(t, env)
	env.server.ConfigureFactoryEnrollmentToken(factoryCoordinationTestToken)
	var original productionJWTClaims
	if _, _, err := jwt.NewParser().ParseUnverified(body["production_jwt"].(string), &original); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"expired", "future", "audience", "subject", "missing_jti", "missing_nbf", "missing_exp", "run", "cloud", "product", "quantity", "algorithm", "signature"} {
		t.Run(stage, func(t *testing.T) {
			claims := original
			method := jwt.SigningMethodHS256
			secret := "isolated-production-signing-secret"
			switch stage {
			case "expired":
				claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))
			case "future":
				claims.NotBefore = jwt.NewNumericDate(time.Now().Add(time.Hour))
			case "audience":
				claims.Audience = jwt.ClaimStrings{"other"}
			case "subject":
				claims.Subject = "user:other"
			case "missing_jti":
				claims.ID = ""
			case "missing_nbf":
				claims.NotBefore = nil
			case "missing_exp":
				claims.ExpiresAt = nil
			case "run":
				claims.ProductionRunID = claims.BrandCloudID
			case "cloud":
				claims.BrandCloudID = claims.ProductionRunID
			case "product":
				claims.DeviceItemProfileID = claims.ProductionRunID
			case "quantity":
				claims.AllowedQuantity = 0
			case "algorithm":
				method = jwt.SigningMethodHS384
			case "signature":
				secret = "incorrect-secret"
			}
			token, err := jwt.NewWithClaims(method, claims).SignedString([]byte(secret))
			if err != nil {
				t.Fatal(err)
			}
			body["production_jwt"] = token
			r := performJSON(env.router, http.MethodPost, "/v1/internal/factory-enrollments/reserve", body, factoryCoordinationTestToken)
			if r.Code != 401 || strings.Contains(r.Body.String(), token) {
				t.Fatalf("invalid JWT accepted/leaked: %d", r.Code)
			}
		})
	}
	var n int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM factory_enrollment_reservations`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("invalid JWT persisted reservation: %d %v", n, err)
	}
}

type failingFactoryCoordinationStore struct{ Store }

func (s failingFactoryCoordinationStore) ReserveFactoryEnrollment(context.Context, store.FactoryEnrollmentAdmission) (store.FactoryEnrollmentReservation, error) {
	return store.FactoryEnrollmentReservation{}, errors.New("private upstream details")
}
func (s failingFactoryCoordinationStore) LookupFactoryEnrollment(context.Context, store.FactoryEnrollmentAdmission) (store.FactoryEnrollmentReservation, error) {
	return store.FactoryEnrollmentReservation{}, errors.New("private upstream details")
}
func (s failingFactoryCoordinationStore) CompleteFactoryEnrollment(context.Context, store.FactoryEnrollmentResult) (store.FactoryEnrollmentReservation, error) {
	return store.FactoryEnrollmentReservation{}, errors.New("private upstream details")
}

func TestIntegrationFactoryCoordinationFailsClosedOnMalformedOrUnavailableRequests(t *testing.T) {
	env := newIntegrationEnv(t)
	body, _, _ := factoryHTTPFixture(t, env)
	env.server.ConfigureFactoryEnrollmentToken(factoryCoordinationTestToken)
	reserve := "/v1/internal/factory-enrollments/reserve"
	body["owner_user_id"] = "untrusted-override"
	if r := performJSON(env.router, http.MethodPost, reserve, body, factoryCoordinationTestToken); r.Code != 400 {
		t.Fatalf("extra field: %d", r.Code)
	}
	delete(body, "owner_user_id")
	env.server.ConfigureProductionJWT("", "")
	if r := performJSON(env.router, http.MethodPost, reserve, body, factoryCoordinationTestToken); r.Code != 503 {
		t.Fatalf("missing JWT verifier: %d", r.Code)
	}
	env.server.ConfigureProductionJWT("isolated-production-signing-secret", "factory-enroll")
	env.server.store = failingFactoryCoordinationStore{Store: env.store}
	if r := performJSON(env.router, http.MethodPost, reserve, body, factoryCoordinationTestToken); r.Code != 503 || strings.Contains(r.Body.String(), "private") {
		t.Fatalf("dependency failure: %d", r.Code)
	}
	delete(body, "production_jwt")
	for _, path := range []string{"/v1/internal/factory-enrollments/lookup", "/v1/internal/factory-enrollments/missing/result"} {
		if strings.HasSuffix(path, "/result") {
			body["status"] = "issued"
			body["evidence_sha256"] = strings.Repeat("c", 64)
		}
		if r := performJSON(env.router, http.MethodPost, path, body, factoryCoordinationTestToken); r.Code != 503 || strings.Contains(r.Body.String(), "private") {
			t.Fatalf("dependency failure: %d", r.Code)
		}
	}
}
