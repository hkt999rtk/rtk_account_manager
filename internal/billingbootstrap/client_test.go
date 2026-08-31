package billingbootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/store"
)

const fixtureToken = "isolated-billing-creation-service-token"

func creationEvent() store.BillingCloudCreation {
	e := store.BillingCloudCreation{EventID: "11111111-1111-4111-8111-111111111111", CloudID: "22222222-2222-4222-8222-222222222222", OwnerUserID: "33333333-3333-4333-8333-333333333333", OwnershipVersion: 1, OccurredAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	e.EvidenceSHA256 = e.Digest()
	return e
}
func TestBillingBootstrapClientRequiresExactAuthenticatedCreationReceipt(t *testing.T) {
	e := creationEvent()
	for _, fault := range []string{"success", "cloud", "owner", "event", "version", "time", "digest", "account", "null", "extra", "oversize", "status"} {
		t.Run(fault, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/internal/billing/cloud-creations" || r.Header.Get("Authorization") != "Bearer "+fixtureToken {
					t.Error("wrong endpoint/credential")
				}
				var in store.BillingCloudCreation
				if json.NewDecoder(r.Body).Decode(&in) != nil || !in.Valid() || in.EvidenceSHA256 != e.EvidenceSHA256 {
					t.Error("bad event")
				}
				out := store.BillingCloudCreationReceipt{BillingCloudCreation: e, AccountID: e.CloudID}
				switch fault {
				case "cloud":
					out.CloudID = e.EventID
				case "owner":
					out.OwnerUserID = e.CloudID
				case "event":
					out.EventID = e.CloudID
				case "version":
					out.OwnershipVersion = 2
				case "time":
					out.OccurredAt = out.OccurredAt.Add(time.Second)
				case "digest":
					out.EvidenceSHA256 = strings.Repeat("a", 64)
				case "account":
					out.AccountID = "bad"
				case "null":
					w.Write([]byte(`null`))
					return
				case "status":
					w.WriteHeader(404)
					w.Write([]byte("private upstream failure"))
					return
				}
				json.NewEncoder(w).Encode(out)
				if fault == "extra" {
					w.Write([]byte(` {}`))
				}
				if fault == "oversize" {
					w.Write([]byte(strings.Repeat(" ", 16<<10)))
				}
			}))
			defer server.Close()
			c, err := New(Config{BaseURL: server.URL, Token: fixtureToken})
			if err != nil {
				t.Fatal(err)
			}
			r, err := c.Bootstrap(context.Background(), e)
			if fault == "success" {
				if err != nil || !r.Valid() {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "private") {
				t.Fatal("unsafe receipt", err)
			}
		})
	}
	for _, url := range []string{"", "http://localhost:123", "http://example.com", "https://user:pass@example.com", "https://example.com/path", "https://example.com?q=x", "https://example.com#x"} {
		if _, err := New(Config{BaseURL: url, Token: fixtureToken}); err == nil {
			t.Fatal("unsafe origin")
		}
	}
	for _, token := range []string{"short", fixtureToken + " x", " " + fixtureToken} {
		if _, err := New(Config{BaseURL: "https://example.com", Token: token}); err == nil {
			t.Fatal("unsafe token")
		}
	}
	c, _ := New(Config{BaseURL: "https://example.com", Token: fixtureToken})
	if _, err := c.Bootstrap(context.Background(), store.BillingCloudCreation{}); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid event")
	}
	if _, err := (*Client)(nil).Bootstrap(context.Background(), e); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing transport")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Bootstrap(ctx, e); !errors.Is(err, ErrUnavailable) {
		t.Fatal("canceled request accepted")
	}
	calls := 0
	dest := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer dest.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, dest.URL, 307) }))
	defer redirect.Close()
	c, _ = New(Config{BaseURL: redirect.URL, Token: fixtureToken})
	if _, err := c.Bootstrap(context.Background(), e); !errors.Is(err, ErrUnavailable) || calls != 0 {
		t.Fatal("credential redirect followed")
	}
}

type queueFixture struct {
	claimErr, finishErr error
	proof               *store.BillingCloudCreationReceipt
	calls               int
}

func (q *queueFixture) ClaimBillingCloudCreations(context.Context) ([]store.BillingCloudCreationJob, error) {
	return []store.BillingCloudCreationJob{{BillingCloudCreation: creationEvent(), LeaseID: "lease"}}, q.claimErr
}
func (q *queueFixture) FinishBillingCloudCreation(_ context.Context, _ store.BillingCloudCreationJob, p *store.BillingCloudCreationReceipt) (bool, error) {
	q.calls++
	q.proof = p
	return q.finishErr == nil, q.finishErr
}

type receiverFixture struct{ err error }

func (r receiverFixture) Bootstrap(_ context.Context, e store.BillingCloudCreation) (store.BillingCloudCreationReceipt, error) {
	return store.BillingCloudCreationReceipt{BillingCloudCreation: e, AccountID: e.CloudID}, r.err
}
func TestBillingBootstrapWorkerRetainsFailuresAndStopsOnCancellation(t *testing.T) {
	for _, fault := range []string{"claim", "receiver", "finish", "context", "success"} {
		q := &queueFixture{}
		r := receiverFixture{}
		ctx := context.Background()
		switch fault {
		case "claim":
			q.claimErr = ErrUnavailable
		case "receiver":
			r.err = ErrUnavailable
		case "finish":
			q.finishErr = ErrUnavailable
		case "context":
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel()
		}
		w, _ := NewWorker(q, r, nil)
		n, err := w.RunOnce(ctx)
		if fault == "success" {
			if err != nil || n != 1 || q.proof == nil {
				t.Fatal("delivery", err)
			}
		} else if fault == "receiver" {
			if err != nil || n != 0 || q.proof != nil || q.calls != 1 {
				t.Fatal("uncertain response acknowledged")
			}
		} else if err == nil || n != 0 {
			t.Fatal("failed batch succeeded")
		}
	}
	if _, err := NewWorker(nil, nil, nil); err == nil {
		t.Fatal("missing dependencies")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w, _ := NewWorker(&queueFixture{claimErr: context.Canceled}, receiverFixture{}, nil)
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}
