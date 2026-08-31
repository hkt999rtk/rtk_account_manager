package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The child is go test -c from Video Cloud's factoryenrollapp package. It runs
// actual factory composition, journal, projections and mTLS issuer, while this
// process serves actual AM APIs/store/JWTs. Secrets are stdin-only fixture data.
func TestIntegrationFactoryApplicationAcrossRealServices(t *testing.T) {
	binary, videoDSN := os.Getenv("TEST_FACTORY_APPLICATION_BINARY"), os.Getenv("TEST_FACTORY_APPLICATION_DSN")
	if binary == "" || videoDSN == "" {
		t.Skip("requires independently compiled Video Cloud factory application and isolated PostgreSQL DSN")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("factory application binary must be an absolute path")
	}
	env := newIntegrationEnv(t)
	body, owner, _ := factoryHTTPFixture(t, env)
	env.server.ConfigureFactoryEnrollmentToken(factoryCoordinationTestToken)
	var loseCompletionReply atomic.Bool
	loseCompletionReply.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/result") && loseCompletionReply.Load() {
			recorder := httptest.NewRecorder()
			env.router.ServeHTTP(recorder, r)
			if recorder.Code == http.StatusOK && loseCompletionReply.CompareAndSwap(true, false) {
				// The real AM transaction has committed; only its response is lost.
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			for key, values := range recorder.Header() {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		env.router.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestFactoryApplicationWithExternalAccountManager$", "-test.timeout=45s")
	cmd.Env = append(os.Environ(), "TEST_FACTORY_EXTERNAL_AM=1")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		input.Close()
		output.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		input.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error("factory application fixture failed", err)
			}
		case <-time.After(5 * time.Second):
			cancel()
			<-done
			t.Error("factory application fixture did not cleanly stop")
		}
	})
	encoder, decoder := json.NewEncoder(input), json.NewDecoder(output)
	if err := encoder.Encode(map[string]any{"DSN": videoDSN, "AMURL": server.URL, "AMToken": factoryCoordinationTestToken, "ProductionJWTSecret": env.server.productionJWTSecret, "ProductionJWT": body["production_jwt"], "RunID": body["production_run_id"], "CloudID": body["brand_cloud_id"], "ProductID": body["device_item_profile_id"]}); err != nil {
		t.Fatal("send factory configuration")
	}
	var ready struct {
		Ready               bool
		DeviceID, RequestID string
	}
	if err := decoder.Decode(&ready); err != nil || !ready.Ready || ready.DeviceID == "" {
		t.Fatal("factory application failed to start")
	}
	type witness struct {
		Code                                                                                int
		ErrorCode, CertificateSHA256, JournalStatus, ReservationID, StoredCertificateSHA256 string
		SignatureCount                                                                      int
		DeviceUpdatedAt                                                                     time.Time
		EvidenceSHA256                                                                      string
	}
	call := func(extra map[string]any) witness {
		t.Helper()
		if extra == nil {
			extra = map[string]any{}
		}
		if extra["Action"] == nil {
			extra["Action"] = "enroll"
		}
		if err := encoder.Encode(extra); err != nil {
			t.Fatal("send factory request")
		}
		var out witness
		if err := decoder.Decode(&out); err != nil {
			t.Fatal("read factory witness")
		}
		return out
	}
	first := call(nil)
	if first.Code != 503 || first.CertificateSHA256 != "" || first.JournalStatus != "projected" || first.SignatureCount != 1 || first.StoredCertificateSHA256 == "" {
		t.Fatalf("lost commit reply was not held: code=%d state=%s signatures=%d", first.Code, first.JournalStatus, first.SignatureCount)
	}
	var issued, reservations int
	var status, evidence string
	if err := env.db.QueryRow(ctx, `SELECT issued_quantity FROM factory_production_runs WHERE id=$1`, body["production_run_id"]).Scan(&issued); err != nil || issued != 1 {
		t.Fatalf("AM did not commit before lost reply: count=%d err=%v", issued, err)
	}
	if err := env.db.QueryRow(ctx, `SELECT status,evidence_sha256 FROM factory_enrollment_reservations WHERE id=$1 AND production_run_id=$2 AND request_id=$3 AND device_id=$4`, first.ReservationID, body["production_run_id"], ready.RequestID, ready.DeviceID).Scan(&status, &evidence); err != nil || status != "issued" || evidence != first.StoredCertificateSHA256 {
		t.Fatal("AM result does not match actual issuer digest", err)
	}
	if err := encoder.Encode(map[string]string{"Action": "restart"}); err != nil {
		t.Fatal(err)
	}
	var restarted struct{ Ready bool }
	if err := decoder.Decode(&restarted); err != nil || !restarted.Ready {
		t.Fatal("factory restart failed")
	}
	for range 3 {
		out := call(nil)
		if out.Code != 200 || out.CertificateSHA256 != evidence || out.JournalStatus != "completed" || out.ReservationID != first.ReservationID || out.SignatureCount != 1 || !out.DeviceUpdatedAt.Equal(first.DeviceUpdatedAt) {
			t.Fatalf("cross-service replay changed issuance/projections: code=%d state=%s signatures=%d", out.Code, out.JournalStatus, out.SignatureCount)
		}
	}
	if out := call(map[string]any{"RequestID": "extra-factory-request"}); out.Code != 409 || out.CertificateSHA256 != "" || out.SignatureCount != 1 {
		t.Fatal("real AM quota allowed extra signing")
	}
	if out := call(map[string]any{"CloudID": "11111111-1111-1111-1111-111111111111"}); out.Code != 403 || out.CertificateSHA256 != "" {
		t.Fatal("cross-cloud factory override accepted")
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, owner); err != nil {
		t.Fatal(err)
	}
	if out := call(nil); out.Code != 403 || out.CertificateSHA256 != "" || out.SignatureCount != 1 {
		t.Fatal("revoked real AM owner retrieved certificate")
	}
	if err := env.db.QueryRow(ctx, `SELECT issued_quantity,(SELECT count(*) FROM factory_enrollment_reservations WHERE production_run_id=r.id) FROM factory_production_runs r WHERE id=$1`, body["production_run_id"]).Scan(&issued, &reservations); err != nil || issued != 1 || reservations != 1 {
		t.Fatalf("cross-service retries changed quota: issued=%d reservations=%d err=%v", issued, reservations, err)
	}
	// A rejected admission still left a prepared consumer intent. With the owner
	// revoked, queue trusted cancellation; AM must persist an admission fence
	// rather than treating its absent reservation as proof of non-issuance.
	const cancellationRequest = "revoked-factory-cancellation"
	if out := call(map[string]any{"RequestID": cancellationRequest}); out.Code != 403 {
		t.Fatal("revoked admission fixture failed")
	}
	if out := call(map[string]any{"Action": "cancel", "RequestID": cancellationRequest}); out.Code != 202 || out.JournalStatus != "prepared" {
		t.Fatal("durable cancellation not queued")
	}
	loseCompletionReply.Store(true)
	intent := call(map[string]any{"Action": "recover", "RequestID": cancellationRequest})
	if intent.Code != 503 || intent.JournalStatus != "canceling" || intent.SignatureCount != 0 || intent.EvidenceSHA256 == "" {
		t.Fatalf("lost non-issuance reply not retained: %+v", intent)
	}
	if err := env.db.QueryRow(ctx, `SELECT status,evidence_sha256 FROM factory_enrollment_reservations WHERE id=$1`, intent.ReservationID).Scan(&status, &evidence); err != nil || status != "not_issued" || evidence != intent.EvidenceSHA256 {
		t.Fatal("AM did not commit non-issuance before dropped response", err)
	}
	if err := encoder.Encode(map[string]string{"Action": "restart"}); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&restarted); err != nil || !restarted.Ready {
		t.Fatal("recovery restart failed")
	}
	for range 3 {
		out := call(map[string]any{"Action": "recover", "RequestID": cancellationRequest})
		if out.Code != 200 || out.JournalStatus != "canceled" || out.ReservationID != intent.ReservationID || out.EvidenceSHA256 != evidence || out.SignatureCount != 0 || out.StoredCertificateSHA256 != "" {
			t.Fatalf("cancellation replay lost evidence: %+v", out)
		}
	}
	if out := call(map[string]any{"RequestID": cancellationRequest}); out.Code != 409 {
		t.Fatal("canceled journal accepted public retry")
	}
	var cancellationAudits int
	if err := env.db.QueryRow(ctx, `SELECT issued_quantity,(SELECT count(*) FROM factory_enrollment_reservations WHERE production_run_id=r.id),(SELECT count(*) FROM audit_events WHERE subject_id=$2) FROM factory_production_runs r WHERE id=$1`, body["production_run_id"], intent.ReservationID).Scan(&issued, &reservations, &cancellationAudits); err != nil || issued != 1 || reservations != 2 || cancellationAudits != 2 {
		t.Fatalf("cancellation changed accounting: %d %d %d %v", issued, reservations, cancellationAudits, err)
	}
}
