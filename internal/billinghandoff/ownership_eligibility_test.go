package billinghandoff

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func eligibilityFixture() (OwnershipEligibilityRequest, OwnershipEligibility) {
	b := fixtureBinding()
	in := OwnershipEligibilityRequest{CloudID: b.CloudID, SourceUserID: b.SourceUserID, TargetUserID: b.TargetUserID, OwnershipVersion: 1, Action: "request"}
	return in, OwnershipEligibility{Request: in, ReceiptID: b.OperationID, EvidenceSHA256: strings.Repeat("a", 64), Currency: "TWD", BalanceMinor: 1, Complete: true, Blockers: []string{}, ObservedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}
}

func TestOwnershipEligibilityTransportValidatesBindingAndEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		valid  bool
	}{
		{"positive", func(map[string]any) {}, true},
		{"zero", func(m map[string]any) { m["balance_minor"] = 0 }, true},
		{"negative", func(m map[string]any) { m["balance_minor"] = -1; m["blockers"] = []string{"balance_negative"} }, true},
		{"missing evidence", func(m map[string]any) {
			m["complete"] = false
			m["receipt_id"] = ""
			m["evidence_sha256"] = ""
			m["blockers"] = []string{"evidence_unavailable"}
		}, true},
		{"missing amount", func(m map[string]any) { delete(m, "balance_minor") }, false},
		{"null amount", func(m map[string]any) { m["balance_minor"] = nil }, false},
		{"wrong target", func(m map[string]any) { r := m["request"].(map[string]any); r["target_user_id"] = r["source_user_id"] }, false},
		{"wrong action", func(m map[string]any) { m["request"].(map[string]any)["action"] = "accept" }, false},
		{"missing action", func(m map[string]any) { delete(m["request"].(map[string]any), "action") }, false},
		{"missing empty transfer", func(m map[string]any) { delete(m["request"].(map[string]any), "transfer_id") }, false},
		{"unknown blocker", func(m map[string]any) { m["blockers"] = []string{"invented"} }, false},
		{"duplicate blocker", func(m map[string]any) { m["blockers"] = []string{"payment_pending", "payment_pending"} }, false},
		{"nil blockers", func(m map[string]any) { m["blockers"] = nil }, false},
		{"negative without blocker", func(m map[string]any) { m["balance_minor"] = -1 }, false},
		{"false negative blocker", func(m map[string]any) { m["blockers"] = []string{"balance_negative"} }, false},
		{"missing proof", func(m map[string]any) { m["receipt_id"] = "" }, false},
		{"incomplete without blocker", func(m map[string]any) { m["complete"] = false }, false},
		{"expired", func(m map[string]any) { m["expires_at"] = time.Now().Add(-time.Second) }, false},
		{"future observed", func(m map[string]any) { m["observed_at"] = time.Now().Add(time.Hour) }, false},
		{"excess lifetime", func(m map[string]any) { m["expires_at"] = time.Now().Add(time.Hour) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, out := eligibilityFixture()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/internal/billing/clouds/"+in.CloudID+"/ownership-eligibility" || r.Header.Get("Authorization") != "Bearer "+fixtureToken || r.Header.Get("X-Billing-Owner-User-ID") != in.SourceUserID || r.Header.Get("X-Billing-Ownership-Version") != "1" {
					t.Error("lost request authority")
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 3 || body["action"] != "request" || body["target_user_id"] != in.TargetUserID {
					t.Errorf("bad body: %+v %v", body, err)
				}
				raw, _ := json.Marshal(out)
				var fields map[string]any
				_ = json.Unmarshal(raw, &fields)
				tc.mutate(fields)
				w.Header().Set("Cache-Control", "no-store")
				_ = json.NewEncoder(w).Encode(fields)
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, Token: fixtureToken})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.CheckOwnershipEligibility(context.Background(), in)
			if tc.valid && err != nil {
				t.Fatal(err)
			}
			if !tc.valid && !errors.Is(err, ErrUnavailable) {
				t.Fatalf("invalid response accepted: %v", err)
			}
		})
	}
}

func TestOwnershipEligibilityRejectsInvalidRequestsAndHTTPFailures(t *testing.T) {
	in, out := eligibilityFixture()
	var nilClient *Client
	if _, err := nilClient.CheckOwnershipEligibility(context.Background(), in); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	for _, mode := range []string{"unauthorized", "cacheable", "oversized", "malformed", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "no-store")
				switch mode {
				case "unauthorized":
					w.WriteHeader(401)
				case "cacheable":
					w.Header().Del("Cache-Control")
					_ = json.NewEncoder(w).Encode(out)
				case "oversized":
					_, _ = w.Write([]byte(strings.Repeat(" ", 65537)))
				default:
					_, _ = w.Write([]byte("{"))
				}
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, Token: fixtureToken})
			if err != nil {
				t.Fatal(err)
			}
			for _, mutate := range []func(*OwnershipEligibilityRequest){func(r *OwnershipEligibilityRequest) { r.Action = "commit" }, func(r *OwnershipEligibilityRequest) { r.Action = "accept" }, func(r *OwnershipEligibilityRequest) { r.TransferID = out.ReceiptID }, func(r *OwnershipEligibilityRequest) { r.TargetUserID = r.SourceUserID }} {
				bad := in
				mutate(&bad)
				if _, err := client.CheckOwnershipEligibility(context.Background(), bad); !errors.Is(err, ErrInvalid) {
					t.Fatalf("bad request: %v", err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			if _, err := client.CheckOwnershipEligibility(ctx, in); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("HTTP failure accepted: %v", err)
			}
		})
	}
}

func TestLiveOwnershipEligibilityTransport(t *testing.T) {
	base := os.Getenv("TEST_BILLING_HANDOFF_URL")
	if base == "" {
		t.Skip("requires isolated live Billing fixture")
	}
	client, err := New(Config{BaseURL: base, Token: os.Getenv("TEST_BILLING_HANDOFF_TOKEN")})
	if err != nil {
		t.Fatal(err)
	}
	in := OwnershipEligibilityRequest{CloudID: os.Getenv("TEST_HANDOFF_CLOUD"), SourceUserID: os.Getenv("TEST_HANDOFF_SOURCE"), TargetUserID: os.Getenv("TEST_HANDOFF_TARGET"), OwnershipVersion: 1, Action: "request"}
	balance, err := strconv.ParseInt(os.Getenv("TEST_HANDOFF_BALANCE"), 10, 64)
	if err != nil || balance < -1 || balance > 1 {
		t.Fatal("fixture balance must be -1, 0 or 1")
	}
	for _, action := range []string{"request", "accept"} {
		in.Action = action
		if action == "accept" {
			in.TransferID = fixtureBinding().OperationID
		}
		out, err := client.CheckOwnershipEligibility(context.Background(), in)
		if err != nil || !out.Complete || out.BalanceMinor != balance || (len(out.Blockers) == 0) != (balance >= 0) || out.Request != in {
			t.Fatalf("live eligibility: %+v %v", out, err)
		}
		if balance < 0 && !slices.Contains(out.Blockers, "balance_negative") {
			t.Fatalf("negative balance blocker missing: %+v", out)
		}
	}
	in.OwnershipVersion++
	if _, err := client.CheckOwnershipEligibility(context.Background(), in); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("wrong version accepted: %v", err)
	}
}
