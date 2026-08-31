package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestIntegrationFactoryCancellationRequiresTrustedCredentialAndFullBinding(t *testing.T) {
	env := newIntegrationEnv(t)
	body, _, access := factoryHTTPFixture(t, env)
	production := body["production_jwt"]
	delete(body, "production_jwt")
	path := "/v1/internal/factory-enrollments/cancel"
	if r := performJSON(env.router, "POST", path, body, factoryCoordinationTestToken); r.Code != 503 {
		t.Fatal("unconfigured cancellation exposed")
	}
	env.server.ConfigureFactoryEnrollmentToken(factoryCoordinationTestToken)
	for _, token := range []string{"", access, production.(string), "other-service"} {
		if r := performJSON(env.router, "POST", path, body, token); r.Code != 401 {
			t.Fatal("untrusted cancellation accepted")
		}
	}
	r := performJSON(env.router, "POST", path, body, factoryCoordinationTestToken)
	if r.Code != 200 || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cancel: %d %s", r.Code, r.Body.String())
	}
	newResponseContract(t).validate(t, http.MethodPost, path, r)
	var out factoryEnrollmentResponse
	json.Unmarshal(r.Body.Bytes(), &out)
	if out.Status != "cancel_requested" || out.EvidenceSHA256 != nil {
		t.Fatal("intent became non-issuance proof")
	}
	body["production_jwt"] = production
	if r := performJSON(env.router, "POST", "/v1/internal/factory-enrollments/reserve", body, factoryCoordinationTestToken); r.Code != 409 {
		t.Fatal("late admission accepted")
	}
	delete(body, "production_jwt")
	body["unknown"] = true
	if r := performJSON(env.router, "POST", path, body, factoryCoordinationTestToken); r.Code != 400 {
		t.Fatal("unknown JSON accepted")
	}
	delete(body, "unknown")
	body["devid"] = "different"
	if r := performJSON(env.router, "POST", path, body, factoryCoordinationTestToken); r.Code != 409 {
		t.Fatal("changed binding accepted")
	}
	body["devid"] = out.DeviceID
	body["status"] = "not_issued"
	body["evidence_sha256"] = strings.Repeat("b", 64)
	if r := performJSON(env.router, "POST", "/v1/internal/factory-enrollments/"+out.ReservationID+"/result", body, factoryCoordinationTestToken); r.Code != 200 {
		t.Fatalf("trusted result: %d", r.Code)
	}
}
