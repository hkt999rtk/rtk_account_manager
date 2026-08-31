package billinghandoff

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestCloudDeletionPreflightTransportRejectsInvalidEvidence(t *testing.T) {
	b := fixtureBinding()
	in := CloudDeletionScope{CloudID: b.CloudID, OwnerUserID: b.SourceUserID, OwnershipVersion: 1}
	for _, mutate := range []func(map[string]any){
		func(m map[string]any) { m["owner_user_id"] = b.TargetUserID }, func(m map[string]any) { m["ownership_version"] = 2 }, func(m map[string]any) { delete(m, "balance_minor") },
		func(m map[string]any) { m["balance_minor"] = nil }, func(m map[string]any) { m["balance_minor"] = 1 }, func(m map[string]any) { m["blockers"] = []string{"unknown"}; m["eligible"] = false },
		func(m map[string]any) { m["expires_at"] = time.Now().Add(-time.Second) }, func(m map[string]any) { m["observed_at"] = time.Now().Add(time.Hour) },
		func(m map[string]any) { m["blockers"] = []string{"payment_pending"} }, func(m map[string]any) { m["blockers"] = nil },
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.Header.Get("X-Billing-Owner-User-ID") != in.OwnerUserID || r.Header.Get("Authorization") != "Bearer "+fixtureToken {
				t.Error("scope/credential lost")
			}
			body := map[string]any{"cloud_id": in.CloudID, "owner_user_id": in.OwnerUserID, "ownership_version": int64(1), "eligible": true, "blockers": []string{}, "balance_minor": int64(0), "currency": "TWD", "observed_at": time.Now(), "expires_at": time.Now().Add(time.Minute)}
			mutate(body)
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(body)
		}))
		client, err := New(Config{BaseURL: server.URL, Token: fixtureToken})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.CloudDeletionPreflight(context.Background(), in); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("invalid readiness accepted: %v", err)
		}
		server.Close()
	}
}

func TestLiveCloudDeletionPreflightTransport(t *testing.T) {
	base := os.Getenv("TEST_BILLING_HANDOFF_URL")
	if base == "" {
		t.Skip("requires live isolated Billing fixture")
	}
	client, err := New(Config{BaseURL: base, Token: os.Getenv("TEST_BILLING_HANDOFF_TOKEN")})
	if err != nil {
		t.Fatal(err)
	}
	in := CloudDeletionScope{CloudID: os.Getenv("TEST_HANDOFF_CLOUD"), OwnerUserID: os.Getenv("TEST_HANDOFF_SOURCE"), OwnershipVersion: 1}
	result, err := client.CloudDeletionPreflight(context.Background(), in)
	if err != nil || !result.Eligible || result.BalanceMinor != 0 || len(result.Blockers) != 0 {
		t.Fatalf("live Billing: %+v %v", result, err)
	}
	in.OwnershipVersion++
	if _, err := client.CloudDeletionPreflight(context.Background(), in); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("wrong version accepted: %v", err)
	}
}
