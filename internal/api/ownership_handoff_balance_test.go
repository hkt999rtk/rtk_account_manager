package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/billinghandoff"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
	"rtk_account_manager/internal/worker/handoff"
)

type publicHandoffFixture struct {
	env            integrationEnv
	source, target verifiedDeveloperFixture
	binding        billinghandoff.Binding
	path           string
}

func newPublicHandoffFixture(t *testing.T) publicHandoffFixture {
	t.Helper()
	env := newIntegrationEnv(t)
	configureAPIHandoffFixture(t, env)
	source := verifiedDeveloperForTest(t, env, "balance-api-source@example.test")
	target := verifiedDeveloperForTest(t, env, "balance-api-target@example.test")
	path := "/v1/developer/brand-clouds/" + source.BrandCloudID + "/owner-transfer"
	res := performJSON(env.router, http.MethodPost, path, map[string]any{"target_email": "balance-api-target@example.test"}, source.AccessToken)
	if res.Code != http.StatusAccepted {
		t.Fatalf("request %d %s", res.Code, res.Body.String())
	}
	contract := newResponseContract(t)
	contract.validate(t, http.MethodPost, path, res)
	view := decodeBody[brandCloudOwnerTransferResponse](t, res).OwnerTransfer
	token := latestAuthToken(t, env.tokenObserver, "balance-api-target@example.test", "brand_cloud_owner_transfer")
	res = performJSON(env.router, http.MethodPost, "/v1/developer/brand-cloud-owner-transfers/accept", map[string]any{"token": token}, target.AccessToken)
	if res.Code != http.StatusAccepted {
		t.Fatalf("accept %d %s", res.Code, res.Body.String())
	}
	contract.validate(t, http.MethodPost, "/v1/developer/brand-cloud-owner-transfers/accept", res)
	binding := billinghandoff.Binding{CloudID: source.BrandCloudID, OperationID: view.ID, SourceUserID: source.UserID, TargetUserID: target.UserID, OwnershipVersion: 1}
	if err := env.db.QueryRow(context.Background(), `SELECT cutoff FROM cloud_ownership_handoffs WHERE id=$1`, view.ID).Scan(&binding.Cutoff); err != nil {
		t.Fatal(err)
	}
	for _, participant := range []string{"billing", "test_resources"} {
		if _, err := env.store.RecordCloudHandoffPrepareAck(context.Background(), store.HandoffPrepareAck{OperationID: view.ID, CloudID: source.BrandCloudID, SourceUserID: source.UserID,
			TargetUserID: target.UserID, OwnershipVersion: 1, Cutoff: binding.Cutoff, Participant: participant, HoldReceiptSHA256: strings.Repeat("a", 64), DrainCheckpointSHA256: strings.Repeat("b", 64)}); err != nil {
			t.Fatal(err)
		}
	}
	return publicHandoffFixture{env: env, source: source, target: target, binding: binding, path: path + "/" + view.ID}
}

type apiBalanceBilling struct {
	mu        sync.Mutex
	confirmed map[string]bool
}

func (b *apiBalanceBilling) status(in billinghandoff.Binding) billinghandoff.Settlement {
	return billinghandoff.Settlement{OperationID: in.OperationID, Phase: "prepared", Blockers: []string{}, Snapshot: &billinghandoff.Snapshot{Version: 2, BalanceMinor: 0, Currency: "TWD", Cutoff: in.Cutoff, SourceConfirmed: b.confirmed[in.SourceUserID], TargetConfirmed: b.confirmed[in.TargetUserID]}}
}
func (b *apiBalanceBilling) Settlement(_ context.Context, in billinghandoff.Binding) (billinghandoff.Settlement, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status(in), nil
}
func (b *apiBalanceBilling) Confirm(_ context.Context, in billinghandoff.Binding, req billinghandoff.Confirmation) (billinghandoff.Settlement, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.confirmed[req.UserID] = true
	return b.status(in), nil
}

func handoffConfirmationHTTP(t *testing.T, f publicHandoffFixture, token, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if value, ok := body.(string); ok {
		raw = []byte(value)
	} else {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, f.path+"/confirm", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	res := httptest.NewRecorder()
	f.env.router.ServeHTTP(res, req)
	return res
}

func exercisePublicHandoffConfirmation(t *testing.T, f publicHandoffFixture) {
	t.Helper()
	contract := newResponseContract(t)
	res := performJSON(f.env.router, http.MethodGet, f.path+"/preview", nil, f.target.AccessToken)
	if res.Code != http.StatusOK {
		t.Fatalf("target preview %d %s", res.Code, res.Body.String())
	}
	contract.validate(t, http.MethodGet, f.path+"/preview", res)
	view := decodeBody[brandCloudOwnerTransferResponse](t, res).OwnerTransfer
	if view.BalanceSnapshot == nil || view.BalanceSnapshot.BalanceMinor != 0 || !view.HasSettledSnapshot || view.SourceConfirmed == nil || *view.SourceConfirmed {
		t.Fatalf("invalid zero preview: %+v", view)
	}
	for _, actor := range []verifiedDeveloperFixture{f.source, f.target} {
		for i := 0; i < 2; i++ {
			res = handoffConfirmationHTTP(t, f, actor.AccessToken, "same-key-per-actor", view.BalanceSnapshot)
			if res.Code != http.StatusAccepted {
				t.Fatalf("confirmation %d %s", res.Code, res.Body.String())
			}
			contract.validate(t, http.MethodPost, f.path+"/confirm", res)
		}
	}
	res = performJSON(f.env.router, http.MethodGet, f.path, nil, f.target.AccessToken)
	if res.Code != http.StatusOK {
		t.Fatalf("reload %d %s", res.Code, res.Body.String())
	}
	contract.validate(t, http.MethodGet, f.path, res)
	view = decodeBody[brandCloudOwnerTransferResponse](t, res).OwnerTransfer
	if !*view.SourceConfirmed || !*view.TargetConfirmed || view.Phase != "awaiting_balance_confirmation" {
		t.Fatalf("reload lost confirmations: %+v", view)
	}
	if res.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("handoff can be cached")
	}
	var owner string
	if err := f.env.db.QueryRow(context.Background(), `SELECT user_id::text FROM organization_members WHERE organization_id=$1 AND role='owner'`, f.binding.CloudID).Scan(&owner); err != nil || owner != f.source.UserID {
		t.Fatalf("balance consent switched ownership: %s %v", owner, err)
	}
	if _, err := f.env.store.GetDeveloperBrandCloudMember(context.Background(), f.binding.CloudID, f.target.UserID); err == nil {
		t.Fatal("target gained cloud membership before commit")
	}
	var requests, acks int
	if err := f.env.db.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM cloud_handoff_confirmation_requests),(SELECT count(*) FROM cloud_handoff_confirmation_acknowledgments)`).Scan(&requests, &acks); err != nil || requests != 2 || acks != 2 {
		t.Fatalf("replay duplicated acknowledgments %d %d %v", requests, acks, err)
	}
}

func TestPublicOwnerHandoffPreviewConfirmationAndReplay(t *testing.T) {
	f := newPublicHandoffFixture(t)
	if err := f.env.store.ConfigureHandoffBilling(&apiBalanceBilling{confirmed: map[string]bool{}}); err != nil {
		t.Fatal(err)
	}
	exercisePublicHandoffConfirmation(t, f)
	contract := newResponseContract(t)
	outsider := verifiedDeveloperForTest(t, f.env, "balance-api-outsider@example.test")
	for _, suffix := range []string{"", "/preview"} {
		res := performJSON(f.env.router, http.MethodGet, f.path+suffix, nil, outsider.AccessToken)
		if res.Code != http.StatusNotFound {
			t.Fatalf("outsider read %d %s", res.Code, res.Body.String())
		}
		contract.validate(t, http.MethodGet, f.path+suffix, res)
	}
	good := model.CloudBalanceSnapshot{OwnershipVersion: 1, BillingSnapshotVersion: 2, BalanceMinor: 0, Currency: "TWD"}
	res := handoffConfirmationHTTP(t, f, outsider.AccessToken, "outsider", good)
	if res.Code != http.StatusNotFound {
		t.Fatalf("outsider confirm %d %s", res.Code, res.Body.String())
	}
	for _, body := range []any{`null`, `{"ownership_version":1,"billing_snapshot_version":2,"currency":"TWD"}`, `{"ownership_version":1,"billing_snapshot_version":2,"currency":"TWD","balance_minor":0,"user_id":"spoof"}`, `{} {}`, `{"ownership_version":1,"billing_snapshot_version":2,"currency":"TWD","balance_minor":-1}`} {
		res = handoffConfirmationHTTP(t, f, f.source.AccessToken, "invalid", body)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("invalid body accepted %d %s", res.Code, res.Body.String())
		}
		contract.validate(t, http.MethodPost, f.path+"/confirm", res)
	}
	res = handoffConfirmationHTTP(t, f, f.source.AccessToken, "", good)
	if res.Code != http.StatusBadRequest {
		t.Fatal("confirmation without key accepted")
	}
	good.BalanceMinor = 1
	res = handoffConfirmationHTTP(t, f, f.source.AccessToken, "wrong-amount", good)
	if res.Code != http.StatusConflict {
		t.Fatalf("wrong amount %d %s", res.Code, res.Body.String())
	}
	contract.validate(t, http.MethodPost, f.path+"/confirm", res)
	res = performJSON(f.env.router, http.MethodPost, f.path+"/cancel", nil, f.source.AccessToken)
	if res.Code != http.StatusOK {
		t.Fatalf("cancel %d %s", res.Code, res.Body.String())
	}
	res = performJSON(f.env.router, http.MethodGet, f.path, nil, f.target.AccessToken)
	contract.validate(t, http.MethodGet, f.path, res)
	view := decodeBody[brandCloudOwnerTransferResponse](t, res).OwnerTransfer
	if !view.HasSettledSnapshot || *view.SourceConfirmed || *view.TargetConfirmed || view.Operation.Phase != "canceling" {
		t.Fatalf("cancel lost history or kept consent: %+v", view)
	}
}

func TestOwnerHandoffRequiresHumanSessionEvenForMatchingUUID(t *testing.T) {
	s := &Server{}
	for _, handler := range []gin.HandlerFunc{s.previewOwnerHandoff, s.confirmOwnerHandoff, s.getBrandCloudOwnerTransfer, s.createBrandCloudOwnerTransfer, s.acceptBrandCloudOwnerTransfer, s.cancelBrandCloudOwnerTransfer} {
		for _, subject := range []auth.SubjectType{"end_user", "brand_cloud_user", "device"} {
			res := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(res)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			c.Set("userID", "11111111-1111-1111-1111-111111111111")
			c.Set("subjectType", subject)
			handler(c)
			if res.Code != http.StatusForbidden {
				t.Fatalf("non-human subject accepted: %s %d", subject, res.Code)
			}
		}
	}
}

// Billing's separate-repository fixture exposes a TEST-ONLY bootstrap hook on
// loopback. Production Billing has no bootstrap/collector endpoint. This test
// proves two real service boundaries with AM global sessions, not real settlement.
func TestLiveOwnerHandoffPublicAPIWithBilling(t *testing.T) {
	base := os.Getenv("TEST_BILLING_HANDOFF_URL")
	if base == "" {
		t.Skip("requires isolated Billing public API contract fixture")
	}
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatal("cross-service fixture must be loopback")
	}
	f := newPublicHandoffFixture(t)
	token := os.Getenv("TEST_BILLING_HANDOFF_TOKEN")
	client, err := billinghandoff.New(billinghandoff.Config{BaseURL: base, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.env.store.ConfigureHandoffBilling(client); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(f.binding)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/test-fixture/bind", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("Billing fixture setup failed: %d", res.StatusCode)
	}
	exercisePublicHandoffConfirmation(t, f)
	if os.Getenv("TEST_HANDOFF_MODE") == "commit" {
		exerciseLiveHandoffCommit(t, f)
	} else if os.Getenv("TEST_HANDOFF_MODE") == "worker" {
		exerciseLiveHandoffWorker(t, f)
	}
}

type liveFixtureParticipant struct{}

func (liveFixtureParticipant) Prepare(context.Context, billinghandoff.Binding) (store.HandoffPrepareAck, error) {
	return store.HandoffPrepareAck{}, fmt.Errorf("fixture preparation must already exist")
}
func (liveFixtureParticipant) Abort(context.Context, store.HandoffCanceledDecision) (store.HandoffAbortAck, error) {
	return store.HandoffAbortAck{}, fmt.Errorf("unexpected fixture cancellation")
}
func (liveFixtureParticipant) Release(_ context.Context, d store.HandoffCommittedDecision) (store.HandoffFinalizationAck, error) {
	return store.HandoffFinalizationAck{CloudID: d.Binding.CloudID, OperationID: d.Binding.OperationID, OwnershipVersion: d.Binding.OwnershipVersion, DecisionSHA256: d.DecisionSHA256, Participant: "test_resources", ReceiptSHA256: strings.Repeat("e", 64)}, nil
}

func exerciseLiveHandoffWorker(t *testing.T, f publicHandoffFixture) {
	t.Helper()
	ctx := context.Background()
	if err := f.env.store.ConfigureHandoffParticipants(map[string]store.HandoffParticipant{"test_resources": liveFixtureParticipant{}}); err != nil {
		t.Fatal(err)
	}
	service, err := handoff.NewService(f.env.store, handoff.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		stats, err := service.RunOnce(ctx)
		if err != nil || stats.Progress != 1 || stats.Retrying != 0 {
			t.Fatalf("worker step %d %+v %v", i, stats, err)
		}
	}
	res := performJSON(f.env.router, http.MethodGet, f.path, nil, f.target.AccessToken)
	newResponseContract(t).validate(t, http.MethodGet, f.path, res)
	view := decodeBody[brandCloudOwnerTransferResponse](t, res).OwnerTransfer
	if view.Phase != "succeeded" || !*view.SourceConfirmed || !*view.TargetConfirmed {
		t.Fatalf("worker did not finalize: %+v", view)
	}
	var owner string
	if err := f.env.db.QueryRow(ctx, `SELECT user_id::text FROM organization_members WHERE organization_id=$1 AND role='owner'`, f.binding.CloudID).Scan(&owner); err != nil || owner != f.target.UserID {
		t.Fatal("worker owner mismatch", owner, err)
	}
	if stats, err := service.RunOnce(ctx); err != nil || stats.Claimed != 0 {
		t.Fatalf("worker rescheduled completed transfer: %+v %v", stats, err)
	}
}

func exerciseLiveHandoffCommit(t *testing.T, f publicHandoffFixture) {
	t.Helper()
	ctx := context.Background()
	decision, err := f.env.store.CommitOwnerHandoff(ctx, f.binding.CloudID, f.binding.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	contract := newResponseContract(t)
	for _, actor := range []verifiedDeveloperFixture{f.source, f.target} {
		res := performJSON(f.env.router, http.MethodGet, f.path, nil, actor.AccessToken)
		if res.Code != http.StatusOK {
			t.Fatalf("participant status after commit: %d %s", res.Code, res.Body.String())
		}
		contract.validate(t, http.MethodGet, f.path, res)
		view := decodeBody[brandCloudOwnerTransferResponse](t, res).OwnerTransfer
		if view.Phase != "finalizing" || !*view.SourceConfirmed || !*view.TargetConfirmed {
			t.Fatalf("invalid committed status: %+v", view)
		}
		var allowed bool
		if err := f.env.db.QueryRow(ctx, `SELECT user_can_access_brand_cloud($1,$2)`, actor.UserID, f.binding.CloudID).Scan(&allowed); err != nil || allowed {
			t.Fatal("commit released access before finalize", err)
		}
	}
	res := performJSON(f.env.router, http.MethodPost, f.path+"/cancel", nil, f.source.AccessToken)
	if res.Code != http.StatusConflict {
		t.Fatalf("postcommit cancellation: %d %s", res.Code, res.Body.String())
	}
	for i := 0; i < 2; i++ {
		if phase, err := f.env.store.FinalizeOwnerHandoff(ctx, f.binding.CloudID, f.binding.OperationID); err != nil || phase != "finalizing" {
			t.Fatalf("real Billing finalize: %s %v", phase, err)
		}
	}
	// Synthetic resource release only; real production adapters remain required.
	if phase, err := f.env.store.RecordHandoffFinalizationAck(ctx, store.HandoffFinalizationAck{CloudID: f.binding.CloudID, OperationID: f.binding.OperationID,
		OwnershipVersion: f.binding.OwnershipVersion, DecisionSHA256: decision.DecisionSHA256, Participant: "test_resources", ReceiptSHA256: strings.Repeat("d", 64)}); err != nil || phase != "succeeded" {
		t.Fatalf("release: %s %v", phase, err)
	}
	res = performJSON(f.env.router, http.MethodGet, f.path, nil, f.target.AccessToken)
	contract.validate(t, http.MethodGet, f.path, res)
	view := decodeBody[brandCloudOwnerTransferResponse](t, res).OwnerTransfer
	if view.Phase != "succeeded" {
		t.Fatalf("handoff not complete: %+v", view)
	}
	var owner string
	var version int64
	if err := f.env.db.QueryRow(ctx, `SELECT m.user_id::text,o.ownership_version FROM organizations o JOIN organization_members m ON m.organization_id=o.id AND m.role='owner' WHERE o.id=$1`, f.binding.CloudID).Scan(&owner, &version); err != nil || owner != f.target.UserID || version != 2 {
		t.Fatalf("owner not committed: %s %d %v", owner, version, err)
	}
	for _, actor := range []verifiedDeveloperFixture{f.source, f.target} {
		var allowed bool
		if err := f.env.db.QueryRow(ctx, `SELECT user_can_access_brand_cloud($1,$2)`, actor.UserID, f.binding.CloudID).Scan(&allowed); err != nil || allowed != (actor.UserID == f.target.UserID) {
			t.Fatalf("final access: %s %t %v", actor.UserID, allowed, err)
		}
	}
}
