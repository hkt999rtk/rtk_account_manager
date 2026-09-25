package billingotaseal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/store"
)

func TestSubmitRequiresAuthenticatedExactBillingEcho(t *testing.T) {
	token := strings.Repeat("s", 32)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	seal := store.OTAPeriodGrantSeal{
		OrganizationID: "11111111-1111-4111-8111-111111111111",
		PeriodStart:    start, PeriodEnd: end, IssuerKind: "platform_grants",
		SealID:          "22222222-2222-8222-8222-222222222222",
		SourceHighWater: json.RawMessage(`{"grant_row_count":0}`),
		ProductIDs:      []string{}, MetricCounts: map[string]int64{},
		SourceSHA256: strings.Repeat("a", 64), SealedAt: end,
	}
	status := http.StatusCreated
	changeEcho := false
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost ||
			r.URL.Path != "/v1/internal/billing/ota-period-seals" ||
			r.Header.Get("Authorization") != "Bearer "+token ||
			r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, nil
		}
		if status == http.StatusConflict {
			return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, nil
		}
		echo := seal
		if changeEcho {
			echo.SourceSHA256 = strings.Repeat("b", 64)
		}
		raw, err := json.Marshal(map[string]any{
			"ota_period_seal": echo,
			"duplicate":       status == http.StatusOK,
		})
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(raw)), Header: make(http.Header)}, nil
	})
	client, err := New(Config{BaseURL: "https://billing.example", Token: token, Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := client.Submit(context.Background(), seal)
	if err != nil || duplicate {
		t.Fatalf("new seal: duplicate=%v err=%v", duplicate, err)
	}
	status = http.StatusOK
	duplicate, err = client.Submit(context.Background(), seal)
	if err != nil || !duplicate {
		t.Fatalf("replay: duplicate=%v err=%v", duplicate, err)
	}
	changeEcho = true
	if _, err := client.Submit(context.Background(), seal); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("changed echo must fail: %v", err)
	}
	status = http.StatusConflict
	if _, err := client.Submit(context.Background(), seal); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed replay conflict: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
