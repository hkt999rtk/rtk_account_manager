package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/billinghandoff"
	"rtk_account_manager/internal/store"
)

func TestLiveCloudDeletionPublicAPIWithBilling(t *testing.T) {
	base := os.Getenv("TEST_BILLING_HANDOFF_URL")
	if base == "" {
		t.Skip("requires isolated Billing closure fixture")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" {
		t.Fatal("fixture requires literal loopback")
	}
	env := newIntegrationEnv(t)
	owner := verifiedDeveloperForTest(t, env, "delete-live@example.com")
	token := os.Getenv("TEST_BILLING_HANDOFF_TOKEN")
	body, _ := json.Marshal(map[string]string{"cloud_id": owner.BrandCloudID, "owner_user_id": owner.UserID})
	req, _ := http.NewRequest("POST", base+"/test-fixture/bind-deletion", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	httpClient := &http.Client{Timeout: 15 * time.Second}
	res, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 204 {
		t.Fatalf("fixture bind %d", res.StatusCode)
	}
	client, err := billinghandoff.New(billinghandoff.Config{BaseURL: base, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	f := apiDeletionFixture{}
	configure := func(s *store.Store) {
		if err := s.ConfigureCloudDeletionPreflight(store.CloudDeletionPreflightOptions{Billing: client, Resources: map[string]store.CloudDeletionResourceObserver{"test_resources": f}}); err != nil {
			t.Fatal(err)
		}
		if err := s.ConfigureCloudDeletion(store.CloudDeletionOptions{Billing: client, Producers: map[string]store.CloudDeletionProducer{"test_resources": f}}); err != nil {
			t.Fatal(err)
		}
	}
	configure(env.store)
	path := "/v1/developer/brand-clouds/" + owner.BrandCloudID
	req = httptest.NewRequest("DELETE", path, nil)
	req.Header.Set("Authorization", "Bearer "+owner.AccessToken)
	req.Header.Set("Idempotency-Key", "live-delete")
	out := httptest.NewRecorder()
	env.router.ServeHTTP(out, req)
	if out.Code != 202 {
		t.Fatalf("admission %d %s", out.Code, out.Body.String())
	}
	op := decodeBody[struct {
		Operation store.CloudDeletionOperation `json:"operation"`
	}](t, out).Operation
	if op, err = env.store.AdvanceCloudDeletion(context.Background(), owner.BrandCloudID, op.ID); err == nil || op.Phase != "closing" {
		t.Fatalf("expected real Billing lost-reply window: %+v %v", op, err)
	}
	restarted := store.New(env.db)
	configure(restarted)
	jobs, err := restarted.ClaimCloudDeletionJobs(context.Background(), 1, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("durable job %+v %v", jobs, err)
	}
	op, err = restarted.ProcessCloudDeletionJob(context.Background(), jobs[0])
	if err != nil || op.State != "succeeded" {
		t.Fatalf("real Billing forward retry %+v %v", op, err)
	}
	status := performJSON(env.router, "GET", path+"/operations/"+op.ID, nil, owner.AccessToken)
	if status.Code != 200 || !strings.Contains(status.Body.String(), `"state":"succeeded"`) {
		t.Fatalf("operation %d %s", status.Code, status.Body.String())
	}
	if got := performJSON(env.router, "GET", path, nil, owner.AccessToken); got.Code != 404 {
		t.Fatalf("deleted cloud %d", got.Code)
	}
}

type apiDeletionFixture struct{}

func (apiDeletionFixture) CloudDeletionPreflight(_ context.Context, in billinghandoff.CloudDeletionScope) (billinghandoff.CloudDeletionPreflight, error) {
	now := time.Now()
	return billinghandoff.CloudDeletionPreflight{CloudDeletionScope: in, Eligible: true, Blockers: []string{}, Currency: "TWD", ObservedAt: now, ExpiresAt: now.Add(time.Minute)}, nil
}
func (apiDeletionFixture) ObserveCloudDeletion(_ context.Context, in store.CloudDeletionResourceScope) (store.CloudDeletionResourceEvidence, error) {
	now := time.Now()
	return store.CloudDeletionResourceEvidence{Scope: in, Complete: true, ReceiptID: "synthetic", EvidenceSHA256: strings.Repeat("a", 64), Blockers: []string{}, ObservedAt: now, ExpiresAt: now.Add(time.Minute)}, nil
}
func (apiDeletionFixture) PrepareCloudDeletion(_ context.Context, in billinghandoff.ClosureBinding, version int64) (store.CloudDeletionHold, error) {
	return store.CloudDeletionHold{Binding: in, AuthorizationVersion: version, Participant: "test_resources", Held: true, Empty: true, ReceiptSHA256: strings.Repeat("a", 64)}, nil
}
func (apiDeletionFixture) PrepareCloudClosure(_ context.Context, in billinghandoff.ClosureBinding, _ string) (billinghandoff.ClosureOperation, error) {
	return billinghandoff.ClosureOperation{ID: in.OperationID, OwnerUserID: in.OwnerUserID, OwnershipVersion: in.OwnershipVersion, Phase: "preparing"}, nil
}
func (apiDeletionFixture) CloudClosureStatus(_ context.Context, in billinghandoff.ClosureBinding) (billinghandoff.ClosureStatus, error) {
	return billinghandoff.ClosureStatus{Ready: true, ReceiptID: in.OperationID}, nil
}
func (apiDeletionFixture) CloseCloud(_ context.Context, in billinghandoff.ClosureBinding, _, _ string) (billinghandoff.ClosureAcknowledgment, error) {
	return billinghandoff.ClosureAcknowledgment{OperationID: in.OperationID, Phase: "closed", ClosedAt: time.Now(), ReceiptSHA256: strings.Repeat("b", 64)}, nil
}

func TestCloudDeletionHTTPContract(t *testing.T) {
	env := newIntegrationEnv(t)
	owner := verifiedDeveloperForTest(t, env, "delete-http@example.com")
	other := verifiedDeveloperForTest(t, env, "delete-http-other@example.com")
	path := "/v1/developer/brand-clouds/" + owner.BrandCloudID
	request := func(token, key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("DELETE", path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		res := httptest.NewRecorder()
		env.router.ServeHTTP(res, req)
		return res
	}
	for _, tc := range []struct {
		token, key, body string
		status           int
	}{{"", "key", "", 401}, {owner.AccessToken, "", "", 400}, {owner.AccessToken, "key", "{}", 400}, {other.AccessToken, "key", "", 404}, {owner.AccessToken, "key", "", 503}} {
		res := request(tc.token, tc.key, tc.body)
		if res.Code != tc.status {
			t.Fatalf("boundary: %d want %d: %s", res.Code, tc.status, res.Body.String())
		}
	}
	f := apiDeletionFixture{}
	if err := env.store.ConfigureCloudDeletionPreflight(store.CloudDeletionPreflightOptions{Billing: f, Resources: map[string]store.CloudDeletionResourceObserver{"test_resources": f}}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.ConfigureCloudDeletion(store.CloudDeletionOptions{Billing: f, Producers: map[string]store.CloudDeletionProducer{"test_resources": f}}); err != nil {
		t.Fatal(err)
	}
	res := request(owner.AccessToken, "key", "")
	if res.Code != 202 || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("admit: %d %s", res.Code, res.Body.String())
	}
	newResponseContract(t).validate(t, "DELETE", path, res)
	var out struct {
		Operation store.CloudDeletionOperation `json:"operation"`
	}
	out = decodeBody[struct {
		Operation store.CloudDeletionOperation `json:"operation"`
	}](t, res)
	location := res.Header().Get("Location")
	if location != path+"/operations/"+out.Operation.ID {
		t.Fatalf("wrong poll target: %s", location)
	}
	if _, err := env.store.AdvanceCloudDeletion(context.Background(), owner.BrandCloudID, out.Operation.ID); err != nil {
		t.Fatal(err)
	}
	status := performJSON(env.router, http.MethodGet, location, nil, owner.AccessToken)
	if status.Code != 200 || !strings.Contains(status.Body.String(), `"state":"succeeded"`) {
		t.Fatalf("poll: %d %s", status.Code, status.Body.String())
	}
	newResponseContract(t).validate(t, "GET", location, status)
	if res := request(owner.AccessToken, "key", ""); res.Code != 202 {
		t.Fatalf("deleted replay: %d %s", res.Code, res.Body.String())
	}
	if res := performJSON(env.router, "GET", location, nil, other.AccessToken); res.Code != 404 {
		t.Fatalf("cross-owner operation read: %d", res.Code)
	}
	if res := performJSON(env.router, "GET", path, nil, owner.AccessToken); res.Code != 404 {
		t.Fatalf("deleted cloud visible: %d", res.Code)
	}
}
