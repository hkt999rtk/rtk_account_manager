package billinghandoff

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCloudClosureTransportScopeAndEvidence(t *testing.T) {
	b := fixtureBinding()
	in := ClosureBinding{CloudDeletionScope: CloudDeletionScope{CloudID: b.CloudID, OwnerUserID: b.SourceUserID, OwnershipVersion: 1}, OperationID: b.OperationID, Cutoff: b.Cutoff}
	for _, mutation := range []string{"valid", "owner", "version", "null_ready", "missing_ready", "missing_receipt", "contradictory", "nested_scope", "cached", "oversize"} {
		t.Run(mutation, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/internal/billing/clouds/"+in.CloudID+"/closures/"+in.OperationID+"/status" || r.Header.Get("X-Billing-Owner-User-ID") != in.OwnerUserID || r.Header.Get("X-Billing-Ownership-Version") != "1" || r.Header.Get("Authorization") != "Bearer "+fixtureToken {
					t.Error("lost scope or credential")
				}
				op := map[string]any{"id": in.OperationID, "account_id": b.CloudID, "owner_user_id": in.OwnerUserID, "ownership_version": 1, "cutoff": in.Cutoff, "phase": "preparing", "version": 1, "created_at": time.Now()}
				status := map[string]any{"operation": op, "ready": true, "receipt_id": b.OperationID, "blockers": []string{}}
				body := map[string]any{"cloud_id": in.CloudID, "owner_user_id": in.OwnerUserID, "ownership_version": 1, "operation_id": in.OperationID, "status": status}
				w.Header().Set("Cache-Control", "no-store")
				switch mutation {
				case "owner":
					body["owner_user_id"] = b.TargetUserID
				case "version":
					body["ownership_version"] = 2
				case "null_ready":
					status["ready"] = nil
				case "missing_ready":
					delete(status, "ready")
				case "missing_receipt":
					delete(status, "receipt_id")
				case "contradictory":
					status["blockers"] = []string{"balance_positive"}
				case "nested_scope":
					op["owner_user_id"] = b.TargetUserID
				case "cached":
					w.Header().Del("Cache-Control")
				case "oversize":
					body["padding"] = strings.Repeat("x", 65536)
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, Token: fixtureToken})
			if err != nil {
				t.Fatal(err)
			}
			out, err := client.CloudClosureStatus(context.Background(), in)
			if mutation == "valid" {
				if err != nil || !out.Ready {
					t.Fatalf("valid: %+v %v", out, err)
				}
			} else if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("invalid accepted: %+v %v", out, err)
			}
		})
	}
}
