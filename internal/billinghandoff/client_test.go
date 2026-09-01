package billinghandoff

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fixtureToken = "account-manager-handoff-test-token-0001"

func fixtureBinding() Binding {
	return Binding{CloudID: "11111111-1111-1111-1111-111111111111", OperationID: "22222222-2222-2222-2222-222222222222",
		SourceUserID: "33333333-3333-3333-3333-333333333333", TargetUserID: "44444444-4444-4444-4444-444444444444",
		OwnershipVersion: 1, Cutoff: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}
func settlementEnvelope(in Binding, balance int64) map[string]any {
	return map[string]any{"cloud_id": in.CloudID, "operation_id": in.OperationID, "ownership_version": in.OwnershipVersion,
		"settlement": map[string]any{"operation_id": in.OperationID, "phase": "prepared", "blockers": []string{}, "snapshot": map[string]any{
			"version": int64(2), "balance_minor": balance, "currency": "TWD", "cutoff": in.Cutoff, "source_confirmed": true, "target_confirmed": false}}}
}

func TestClientRejectsUnsafeConfiguration(t *testing.T) {
	for _, base := range []string{"", "http://billing.example", "http://localhost:1234", "https://user:pass@billing.example", "https://billing.example/path", "https://billing.example?x=1", "https://billing.example/#fragment", "file:///billing"} {
		if _, err := New(Config{BaseURL: base, Token: fixtureToken}); err == nil {
			t.Fatalf("unsafe URL accepted: %s", base)
		}
	}
	for _, token := range []string{"", "short", fixtureToken + "\r\nInjected: value"} {
		if _, err := New(Config{BaseURL: "https://billing.example", Token: token}); err == nil {
			t.Fatal("unsafe credential accepted")
		}
	}
}

func TestClientValidatesScopeSnapshotAndExactNonnegativeIntegers(t *testing.T) {
	in := fixtureBinding()
	for _, balance := range []int64{0, 1, math.MaxInt64} {
		t.Run(strconv.FormatInt(balance, 10), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+fixtureToken || r.Header.Get("X-Billing-Ownership-Version") != "1" || r.URL.Path != "/v1/internal/billing/clouds/"+in.CloudID+"/ownership-handoffs/"+in.OperationID+"/confirm" {
					t.Error("lost request scope or credential")
				}
				var confirmation Confirmation
				if err := json.NewDecoder(r.Body).Decode(&confirmation); err != nil || confirmation.BalanceMinor != balance || confirmation.UserID != in.SourceUserID {
					t.Errorf("amount rounded or actor changed: %+v %v", confirmation, err)
				}
				w.Header().Set("Cache-Control", "no-store")
				_ = json.NewEncoder(w).Encode(settlementEnvelope(in, balance))
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, Token: fixtureToken, Transport: server.Client().Transport})
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Confirm(context.Background(), in, Confirmation{UserID: in.SourceUserID, SnapshotVersion: 2, BalanceMinor: balance, Currency: "TWD"})
			if err != nil || result.Snapshot == nil || result.Snapshot.BalanceMinor != balance {
				t.Fatalf("valid exact amount: %+v %v", result, err)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong cloud", func(m map[string]any) { m["cloud_id"] = in.OperationID }},
		{"wrong operation", func(m map[string]any) { m["operation_id"] = in.CloudID }},
		{"wrong ownership", func(m map[string]any) { m["ownership_version"] = 2 }},
		{"nested wrong operation", func(m map[string]any) { m["settlement"].(map[string]any)["operation_id"] = in.CloudID }},
		{"missing zero amount", func(m map[string]any) {
			delete(m["settlement"].(map[string]any)["snapshot"].(map[string]any), "balance_minor")
		}},
		{"negative", func(m map[string]any) {
			m["settlement"].(map[string]any)["snapshot"].(map[string]any)["balance_minor"] = -1
		}},
		{"missing confirmation", func(m map[string]any) {
			delete(m["settlement"].(map[string]any)["snapshot"].(map[string]any), "target_confirmed")
		}},
		{"wrong cutoff", func(m map[string]any) {
			m["settlement"].(map[string]any)["snapshot"].(map[string]any)["cutoff"] = in.Cutoff.Add(time.Second)
		}},
		{"blocked snapshot", func(m map[string]any) { m["settlement"].(map[string]any)["blockers"] = []string{"usage_unsettled"} }},
		{"missing snapshot without blocker", func(m map[string]any) { delete(m["settlement"].(map[string]any), "snapshot") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := settlementEnvelope(in, 0)
			tc.mutate(body)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "no-store")
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, Token: fixtureToken})
			if _, err := client.Settlement(context.Background(), in); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("invalid settlement accepted: %v", err)
			}
		})
	}
}

func TestClientNeverFollowsRedirectOrLeaksRemoteDiagnostics(t *testing.T) {
	var leaked atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client, _ := New(Config{BaseURL: redirect.URL, Token: fixtureToken})
	if _, err := client.Settlement(context.Background(), fixtureBinding()); err == nil || leaked.Load() != 0 {
		t.Fatalf("redirect followed: %v calls=%d", err, leaked.Load())
	}
	for _, tc := range []struct {
		status      int
		body, cache string
	}{
		{503, `{"error":{"code":"private-diagnostics","message":"` + fixtureToken + `"}}`, ""},
		{200, strings.Repeat("x", 65537), "no-store"},
		{200, `{} {}`, "no-store"},
		{200, `{}`, "public,max-age=3600"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", tc.cache)
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		client, _ := New(Config{BaseURL: server.URL, Token: fixtureToken})
		_, err := client.Settlement(context.Background(), fixtureBinding())
		server.Close()
		if err == nil || strings.Contains(err.Error(), fixtureToken) || strings.Contains(err.Error(), "private-diagnostics") {
			t.Fatalf("unsafe error: %v", err)
		}
	}
}

// Invoked by Billing's separate-repository integration fixture. Only literal
// loopback HTTP is allowed; never run this mutating test against staging.
func TestLiveBillingTransportContract(t *testing.T) {
	base := os.Getenv("TEST_BILLING_HANDOFF_URL")
	if base == "" {
		t.Skip("requires isolated Billing HTTP fixture")
	}
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatal("live fixture must be loopback HTTP")
	}
	in := Binding{CloudID: os.Getenv("TEST_HANDOFF_CLOUD"), OperationID: os.Getenv("TEST_HANDOFF_OPERATION"), SourceUserID: os.Getenv("TEST_HANDOFF_SOURCE"), TargetUserID: os.Getenv("TEST_HANDOFF_TARGET"), OwnershipVersion: 1}
	var err error
	in.Cutoff, err = time.Parse(time.RFC3339Nano, os.Getenv("TEST_HANDOFF_CUTOFF"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{BaseURL: base, Token: os.Getenv("TEST_BILLING_HANDOFF_TOKEN")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.Prepare(ctx, in); err != nil {
		t.Fatal(err)
	}
	status, err := client.Settlement(ctx, in)
	if err != nil || status.Snapshot == nil {
		t.Fatalf("real Billing snapshot: %+v %v", status, err)
	}
	confirmation := Confirmation{UserID: in.SourceUserID, SnapshotVersion: status.Snapshot.Version, BalanceMinor: status.Snapshot.BalanceMinor, Currency: status.Snapshot.Currency}
	for i := 0; i < 2; i++ {
		if _, err := client.Confirm(ctx, in, confirmation); err != nil {
			t.Fatal(err)
		}
	}
	confirmation.UserID = in.TargetUserID
	if _, err := client.Confirm(ctx, in, confirmation); err != nil {
		t.Fatal(err)
	}
	grantID := "55555555-5555-5555-5555-555555555555"
	if _, err := client.AuthorizeCommit(ctx, in, grantID, status.Snapshot.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Finalize(ctx, in, grantID, in.Cutoff.Add(time.Second), strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Finalize(ctx, in, grantID, in.Cutoff.Add(time.Second), strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	_, err = client.Abort(ctx, in, "66666666-6666-6666-6666-666666666666", grantID, strings.Repeat("e", 64))
	var rejected *HTTPError
	if !errors.As(err, &rejected) || rejected.Status != http.StatusConflict {
		t.Fatalf("known commit allowed cancellation: %v", err)
	}
}
