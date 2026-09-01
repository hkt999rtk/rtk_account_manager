package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The executable is built from Video Cloud's testdata/admission-client, not an
// AM-local model of that client. Stdin carries fixture secrets; stdout is only
// the scope-bound ledger response or a stable error code.
func TestIntegrationFactoryCoordinationWithVideoCloudClient(t *testing.T) {
	binary := os.Getenv("TEST_FACTORY_ADMISSION_CLIENT")
	if binary == "" {
		t.Skip("requires independently built Video Cloud admission client fixture")
	}
	if !strings.HasPrefix(binary, "/") {
		t.Fatal("client fixture must use an absolute local executable path")
	}
	env := newIntegrationEnv(t)
	body, owner, _ := factoryHTTPFixture(t, env)
	env.server.ConfigureFactoryEnrollmentToken(factoryCoordinationTestToken)
	server := httptest.NewServer(env.router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { input.Close(); cancel(); _ = cmd.Wait() })
	encode, decode := json.NewEncoder(input), json.NewDecoder(output)
	scope := factoryEnrollmentScope{RunID: body["production_run_id"].(string), CloudID: body["brand_cloud_id"].(string), ProductID: body["device_item_profile_id"].(string), RequestID: body["request_id"].(string), DeviceID: body["devid"].(string), RequestSHA256: body["request_sha256"].(string)}
	reservation := factoryEnrollmentResponse{factoryEnrollmentScope: scope}
	call := func(action string) (factoryEnrollmentResponse, string) {
		t.Helper()
		if err := encode.Encode(map[string]any{"BaseURL": server.URL, "Token": factoryCoordinationTestToken, "Action": action, "ProductionJWT": body["production_jwt"], "Reservation": reservation, "Status": "issued", "Evidence": strings.Repeat("b", 64)}); err != nil {
			t.Fatal(err)
		}
		var response struct {
			Reservation factoryEnrollmentResponse
			Error       string
		}
		if err := decode.Decode(&response); err != nil {
			t.Fatal("client fixture did not return valid output", err)
		}
		return response.Reservation, response.Error
	}
	reservation, code := call("reserve")
	if code != "" || reservation.ReservationID == "" || reservation.Status != "reserved" || reservation.factoryEnrollmentScope != scope {
		t.Fatalf("cross-service reserve failed: %s", code)
	}
	if out, code := call("reserve"); code != "" || out.ReservationID != reservation.ReservationID {
		t.Fatalf("cross-service replay failed: %s", code)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, owner); err != nil {
		t.Fatal(err)
	}
	if _, code := call("reserve"); code != "not_found" {
		t.Fatalf("disabled source admitted: %s", code)
	}
	if out, code := call("lookup"); code != "" || out.ReservationID != reservation.ReservationID {
		t.Fatalf("reconciliation lookup failed: %s", code)
	}
	for range 2 {
		out, code := call("complete")
		if code != "" || out.Status != "issued" || out.EvidenceSHA256 == nil || *out.EvidenceSHA256 != strings.Repeat("b", 64) {
			t.Fatalf("cross-service result failed: %s", code)
		}
	}
	var issued int
	if err := env.db.QueryRow(ctx, `SELECT issued_quantity FROM factory_production_runs WHERE id=$1`, scope.RunID).Scan(&issued); err != nil || issued != 1 {
		t.Fatalf("duplicate completion: %d %v", issued, err)
	}
}
