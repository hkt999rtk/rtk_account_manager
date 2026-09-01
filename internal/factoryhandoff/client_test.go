package factoryhandoff

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/billinghandoff"
	"rtk_account_manager/internal/store"
)

const testToken = "isolated-factory-handoff-test-credential"

func fixtureBinding() billinghandoff.Binding {
	return billinghandoff.Binding{CloudID: "11111111-1111-4111-8111-111111111111", OperationID: "22222222-2222-4222-8222-222222222222", SourceUserID: "33333333-3333-4333-8333-333333333333", TargetUserID: "44444444-4444-4444-8444-444444444444", OwnershipVersion: 2, Cutoff: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
}
func fixtureRecord(b binding) record {
	return fixtureRecordFor(b, "factory-cloud-handoff-v1")
}
func fixtureRecordFor(b binding, domain string) record {
	zero := int64(0)
	return record{binding: b, Phase: "prepared", Hold: b.domainDigest(domain, "hold"), Drain: b.domainDigest(domain, "drained", "0", "0"), Completed: &zero, Canceled: &zero}
}

func TestReviewedResourceParticipantsUseFixedRoutesAndDigestDomains(t *testing.T) {
	in := fixtureBinding()
	b := wireBinding(in)
	for _, tc := range []struct {
		participant, path, domain string
	}{
		{ParticipantVideoControlPlane, "/v1/internal/video-control-plane-handoffs/prepare", "video-control-plane-cloud-handoff-v1"},
		{ParticipantMQTTUsage, "/v1/internal/mqtt-usage-handoffs/prepare", "mqtt-usage-cloud-handoff-v1"},
	} {
		t.Run(tc.participant, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != tc.path || req.Header.Get("Authorization") != "Bearer "+testToken {
					t.Fatalf("unexpected authenticated route %s", req.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(fixtureRecordFor(b, tc.domain))
			}))
			defer server.Close()
			client, err := NewParticipant(tc.participant, Config{BaseURL: server.URL, Token: testToken})
			if err != nil {
				t.Fatal(err)
			}
			ack, err := client.Prepare(context.Background(), in)
			if err != nil || ack.Participant != tc.participant || ack.HoldReceiptSHA256 != b.domainDigest(tc.domain, "hold") {
				t.Fatalf("participant receipt %+v %v", ack, err)
			}
		})
	}
	if _, err := NewParticipant("unreviewed", Config{BaseURL: "https://example.com", Token: testToken}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unreviewed participant accepted: %v", err)
	}
}

func TestVideoControlPlaneDeletionObserverBindsScopeAndEvidence(t *testing.T) {
	scope := store.CloudDeletionResourceScope{CloudDeletionScope: billinghandoff.CloudDeletionScope{CloudID: fixtureBinding().CloudID, OwnerUserID: fixtureBinding().SourceUserID, OwnershipVersion: 2}, AuthorizationVersion: 9}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/internal/video-control-plane-handoffs/deletion-preflight" || req.Header.Get("Authorization") != "Bearer "+testToken {
			t.Fatalf("unexpected authenticated route %s", req.URL.Path)
		}
		var got deletionScope
		if json.NewDecoder(req.Body).Decode(&got) != nil || got.CloudID != scope.CloudID || got.AuthorizationVersion != scope.AuthorizationVersion {
			t.Fatal("wrong deletion scope")
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		evidence := deletionEvidence{deletionScope: got, Complete: true, ReceiptID: "55555555-5555-4555-8555-555555555555", Blockers: []string{"jobs_running"}, ObservedAt: now, ExpiresAt: now.Add(time.Minute)}
		evidence.EvidenceSHA256 = evidence.digest()
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(evidence)
	}))
	defer server.Close()
	client, err := NewParticipant(ParticipantVideoControlPlane, Config{BaseURL: server.URL, Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := client.ObserveCloudDeletion(context.Background(), scope)
	if err != nil || evidence.Scope != scope || len(evidence.Blockers) != 1 || evidence.Blockers[0] != "jobs_running" {
		t.Fatalf("evidence %+v: %v", evidence, err)
	}
	client.participant = ParticipantMQTTUsage
	if _, err := client.ObserveCloudDeletion(context.Background(), scope); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unreviewed observer accepted: %v", err)
	}
}

func TestVideoControlPlaneDeletionObserverRejectsUntrustedEvidence(t *testing.T) {
	scope := store.CloudDeletionResourceScope{CloudDeletionScope: billinghandoff.CloudDeletionScope{CloudID: fixtureBinding().CloudID, OwnerUserID: fixtureBinding().SourceUserID, OwnershipVersion: 2}, AuthorizationVersion: 9}
	for _, fault := range []string{"status", "cache", "scope", "digest", "blocker", "unknown", "extra"} {
		t.Run(fault, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var got deletionScope
				_ = json.NewDecoder(req.Body).Decode(&got)
				now := time.Now().UTC().Truncate(time.Microsecond)
				evidence := deletionEvidence{deletionScope: got, Complete: true, ReceiptID: "55555555-5555-4555-8555-555555555555", Blockers: []string{}, ObservedAt: now, ExpiresAt: now.Add(time.Minute)}
				if fault == "scope" {
					evidence.AuthorizationVersion++
				}
				if fault == "blocker" {
					evidence.Blockers = []string{"unknown"}
				}
				evidence.EvidenceSHA256 = evidence.digest()
				if fault == "digest" {
					evidence.EvidenceSHA256 = strings.Repeat("f", 64)
				}
				if fault != "cache" {
					w.Header().Set("Cache-Control", "no-store")
				}
				if fault == "status" {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
				raw, _ := json.Marshal(evidence)
				if fault == "unknown" {
					var object map[string]any
					_ = json.Unmarshal(raw, &object)
					object["private"] = true
					raw, _ = json.Marshal(object)
				}
				_, _ = w.Write(raw)
				if fault == "extra" {
					_, _ = w.Write([]byte(` {}`))
				}
			}))
			defer server.Close()
			client, err := NewParticipant(ParticipantVideoControlPlane, Config{BaseURL: server.URL, Token: testToken})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = client.ObserveCloudDeletion(context.Background(), scope); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("untrusted evidence accepted: %v", err)
			}
		})
	}
}

func TestVideoControlPlaneDeletionProducerHoldsAndCancelsExactBinding(t *testing.T) {
	in := billinghandoff.ClosureBinding{CloudDeletionScope: billinghandoff.CloudDeletionScope{CloudID: fixtureBinding().CloudID, OwnerUserID: fixtureBinding().SourceUserID, OwnershipVersion: 2}, OperationID: fixtureBinding().OperationID, Cutoff: fixtureBinding().Cutoff}
	const authorizationVersion = int64(10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		switch req.URL.Path {
		case "/v1/internal/video-control-plane-handoffs/deletion-hold":
			var binding deletionBinding
			if json.NewDecoder(req.Body).Decode(&binding) != nil {
				t.Fatal("invalid hold body")
			}
			_ = json.NewEncoder(w).Encode(deletionHold{deletionBinding: binding, Participant: ParticipantVideoControlPlane, Phase: "holding", Held: true, Empty: true, ReceiptSHA256: binding.digest("holding", "true")})
		case "/v1/internal/video-control-plane-handoffs/deletion-cancel":
			var decision struct {
				deletionBinding
				CancellationID     string `json:"cancellation_id"`
				CancellationSHA256 string `json:"cancellation_sha256"`
			}
			if json.NewDecoder(req.Body).Decode(&decision) != nil {
				t.Fatal("invalid cancellation body")
			}
			_ = json.NewEncoder(w).Encode(deletionHold{deletionBinding: decision.deletionBinding, Participant: ParticipantVideoControlPlane, Phase: "canceled", ReceiptSHA256: decision.digest("canceled", decision.CancellationID, decision.CancellationSHA256), CancellationID: decision.CancellationID})
		default:
			t.Fatalf("unexpected route %s", req.URL.Path)
		}
	}))
	defer server.Close()
	client, err := NewParticipant(ParticipantVideoControlPlane, Config{BaseURL: server.URL, Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	hold, err := client.PrepareCloudDeletion(context.Background(), in, authorizationVersion)
	if err != nil || !hold.Held || !hold.Empty || hold.AuthorizationVersion != authorizationVersion {
		t.Fatalf("hold = %+v, %v", hold, err)
	}
	cancellationID, cancellationSHA := "77777777-7777-4777-8777-777777777777", strings.Repeat("d", 64)
	release, err := client.CancelCloudDeletion(context.Background(), in, authorizationVersion, cancellationID, cancellationSHA)
	if err != nil || !release.Released || release.CancellationID != cancellationID || release.Participant != ParticipantVideoControlPlane {
		t.Fatalf("release = %+v, %v", release, err)
	}
}

func TestFactoryHandoffClientValidatesConfigurationAndExactReceipts(t *testing.T) {
	for _, base := range []string{"", "http://cloud.example", "http://localhost:123", "ftp://127.0.0.1", "https://user:secret@example.com", "https://example.com/path", "https://example.com?", "https://example.com?q=1", "https://example.com#fragment", "https://example.com/%2f"} {
		if _, err := New(Config{BaseURL: base, Token: testToken}); !errors.Is(err, ErrInvalid) {
			t.Fatal("unsafe origin", base)
		}
	}
	for _, token := range []string{"short", testToken + " ", " " + testToken, testToken + "\n", testToken + " x"} {
		if _, err := New(Config{BaseURL: "https://example.com", Token: token}); err == nil {
			t.Fatal("unsafe credential")
		}
	}
	if _, err := New(Config{BaseURL: "http://factoryenroll.stack-video-cloud.svc.cluster.local:80", Token: testToken}); err != nil {
		t.Fatalf("cluster-local participant URL rejected: %v", err)
	}
	in := fixtureBinding()
	b := wireBinding(in)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls++
		if req.Method != "POST" || req.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("wrong request authentication")
		}
		var d decision
		if json.NewDecoder(req.Body).Decode(&d) != nil || !d.binding.equal(b) {
			t.Error("wrong request scope")
		}
		r := fixtureRecord(b)
		switch req.URL.Path {
		case "/v1/internal/factory-handoffs/prepare":
		case "/v1/internal/factory-handoffs/abort":
			r.Phase = "aborted"
			r.DecisionID = d.ID
			r.DecisionSHA256 = d.SHA256
			r.DecidedAt = &d.At
		case "/v1/internal/factory-handoffs/release":
			r.Phase = "released"
			r.DecisionID = d.ID
			r.DecisionSHA256 = d.SHA256
			r.DecidedAt = &d.At
			r.CommittedVersion = d.CommittedVersion
		default:
			t.Error("unexpected route")
		}
		json.NewEncoder(w).Encode(r)
	}))
	defer server.Close()
	c, err := New(Config{BaseURL: server.URL + "/", Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	ack, err := c.Prepare(context.Background(), in)
	if err != nil || ack.Participant != Participant || ack.HoldReceiptSHA256 != b.digest("hold") || ack.DrainCheckpointSHA256 != b.digest("drained", "0", "0") {
		t.Fatal("prepare", err)
	}
	abort := store.HandoffCanceledDecision{Binding: in, CancellationID: in.OperationID, DecisionSHA256: strings.Repeat("a", 64), CanceledAt: in.Cutoff}
	a, err := c.Abort(context.Background(), abort)
	if err != nil || a.Participant != Participant || !validDigest(a.ReceiptSHA256) {
		t.Fatal("abort", err)
	}
	release := store.HandoffCommittedDecision{Binding: in, AuthorizationID: in.OperationID, DecisionSHA256: strings.Repeat("b", 64), CommittedAt: in.Cutoff, CommittedOwnershipVersion: 3}
	r, err := c.Release(context.Background(), release)
	if err != nil || r.OwnershipVersion != 2 || r.DecisionSHA256 != release.DecisionSHA256 || r.ReceiptSHA256 == a.ReceiptSHA256 {
		t.Fatal("release", err)
	}
	if calls != 3 {
		t.Fatal("unexpected retries", calls)
	}
}

func TestFactoryHandoffClientRejectsIncompleteOrMismatchedProof(t *testing.T) {
	in := fixtureBinding()
	b := wireBinding(in)
	for _, fault := range []string{"cloud", "operation", "source", "target", "version", "cutoff", "hold", "drain", "negative", "missing_count", "null_count", "unknown", "extra_json", "oversize", "status", "phase", "decision", "time", "committed"} {
		t.Run(fault, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				r := fixtureRecord(b)
				switch fault {
				case "cloud":
					r.CloudID = in.OperationID
				case "operation":
					r.OperationID = in.CloudID
				case "source":
					r.SourceUserID = in.TargetUserID
				case "target":
					r.TargetUserID = in.SourceUserID
				case "version":
					r.OwnershipVersion++
				case "cutoff":
					r.Cutoff = r.Cutoff.Add(time.Microsecond)
				case "hold":
					r.Hold = strings.Repeat("a", 64)
				case "drain":
					r.Drain = ""
				case "negative":
					negative := int64(-1)
					r.Completed = &negative
				case "null_count":
					r.Completed = nil
				case "status":
					w.WriteHeader(404)
					w.Write([]byte("private backend failure"))
					return
				case "phase":
					r.Phase = "holding"
				case "decision":
					r.DecisionID = in.OperationID
				case "time":
					r.DecidedAt = &in.Cutoff
				case "committed":
					r.CommittedVersion = 3
				}
				raw, _ := json.Marshal(r)
				if fault == "missing_count" || fault == "unknown" {
					var m map[string]any
					json.Unmarshal(raw, &m)
					if fault == "missing_count" {
						delete(m, "completed_count")
					} else {
						m["extra"] = true
					}
					raw, _ = json.Marshal(m)
				}
				w.Write(raw)
				if fault == "extra_json" {
					w.Write([]byte(` {}`))
				}
				if fault == "oversize" {
					w.Write([]byte(strings.Repeat(" ", 16<<10)))
				}
			}))
			defer server.Close()
			c, _ := New(Config{BaseURL: server.URL, Token: testToken})
			if _, err := c.Prepare(context.Background(), in); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "private") {
				t.Fatal("invalid proof accepted", err)
			}
		})
	}
}

func TestFactoryHandoffClientBindsTerminalDecisionsAndRejectsTransportUncertainty(t *testing.T) {
	in := fixtureBinding()
	b := wireBinding(in)
	for _, action := range []string{"abort", "release"} {
		for _, fault := range []string{"phase", "id", "digest", "time", "version", "missing_drain"} {
			if action == "abort" && fault == "missing_drain" {
				continue
			}
			t.Run(action+"/"+fault, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					var d decision
					json.NewDecoder(req.Body).Decode(&d)
					r := fixtureRecord(b)
					r.Phase = "aborted"
					if action == "release" {
						r.Phase = "released"
					}
					r.DecisionID = d.ID
					r.DecisionSHA256 = d.SHA256
					r.DecidedAt = &d.At
					r.CommittedVersion = d.CommittedVersion
					switch fault {
					case "phase":
						r.Phase = "holding"
					case "id":
						r.DecisionID = in.CloudID
					case "digest":
						r.DecisionSHA256 = strings.Repeat("c", 64)
					case "time":
						r.DecidedAt = nil
					case "version":
						r.CommittedVersion++
					case "missing_drain":
						r.Drain = ""
					}
					json.NewEncoder(w).Encode(r)
				}))
				defer server.Close()
				c, _ := New(Config{BaseURL: server.URL, Token: testToken})
				var err error
				if action == "abort" {
					_, err = c.Abort(context.Background(), store.HandoffCanceledDecision{Binding: in, CancellationID: in.OperationID, DecisionSHA256: strings.Repeat("b", 64), CanceledAt: in.Cutoff})
				} else {
					_, err = c.Release(context.Background(), store.HandoffCommittedDecision{Binding: in, AuthorizationID: in.OperationID, DecisionSHA256: strings.Repeat("b", 64), CommittedAt: in.Cutoff, CommittedOwnershipVersion: 3})
				}
				if !errors.Is(err, ErrUnavailable) {
					t.Fatal("mismatched decision accepted", err)
				}
			})
		}
	}
	redirectCalls := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirectCalls++ }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer redirect.Close()
	c, _ := New(Config{BaseURL: redirect.URL, Token: testToken})
	if _, err := c.Prepare(context.Background(), in); !errors.Is(err, ErrUnavailable) || redirectCalls != 0 {
		t.Fatal("redirect followed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Prepare(ctx, in); !errors.Is(err, ErrUnavailable) {
		t.Fatal("canceled request accepted")
	}
	if _, err := (*Client)(nil).Prepare(context.Background(), in); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing client")
	}
	if _, err := c.Prepare(context.Background(), billinghandoff.Binding{}); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid binding")
	}
	if _, err := c.Release(context.Background(), store.HandoffCommittedDecision{Binding: in, AuthorizationID: in.OperationID, DecisionSHA256: strings.Repeat("a", 64), CommittedAt: in.Cutoff, CommittedOwnershipVersion: 2}); !errors.Is(err, ErrInvalid) {
		t.Fatal("wrong new version")
	}
	if _, err := c.Abort(context.Background(), store.HandoffCanceledDecision{Binding: in}); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid decision")
	}
	c, _ = New(Config{BaseURL: "https://factory.example", Token: testToken, Transport: failingTransport{}})
	if _, err := c.Prepare(context.Background(), in); !errors.Is(err, ErrUnavailable) {
		t.Fatal("transport failure accepted")
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, io.ErrUnexpectedEOF
}
