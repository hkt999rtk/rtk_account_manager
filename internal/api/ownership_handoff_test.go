package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/store"
)

type apiHandoffEvidence struct {
	Balance  int64
	Blockers []string
}

func (provider apiHandoffEvidence) CheckOwnershipEligibility(_ context.Context, in store.HandoffEligibilityRequest) (store.HandoffEligibility, error) {
	now := time.Now().UTC()
	return store.HandoffEligibility{Request: in, ReceiptID: "synthetic-api-test", EvidenceSHA256: strings.Repeat("e", 64), Currency: "TWD", BalanceMinor: provider.Balance, Blockers: provider.Blockers, Complete: true, ObservedAt: now, ExpiresAt: now.Add(time.Minute)}, nil
}
func configureAPIHandoffFixture(t *testing.T, env integrationEnv) {
	t.Helper()
	if err := env.store.ConfigureOwnershipHandoff(store.OwnershipHandoffOptions{Eligibility: apiHandoffEvidence{}, Producers: store.RequiredHandoffProducers()}); err != nil {
		t.Fatal(err)
	}
}
func TestOwnerTransferWithoutBillingAdapterFailsClosed(t *testing.T) {
	env := newIntegrationEnv(t)
	source := verifiedDeveloperForTest(t, env, "no-adapter-source@example.test")
	verifiedDeveloperForTest(t, env, "no-adapter-target@example.test")
	res := performJSON(env.router, http.MethodPost, "/v1/developer/brand-clouds/"+source.BrandCloudID+"/owner-transfer", map[string]any{"target_email": "no-adapter-target@example.test"}, source.AccessToken)
	if res.Code != http.StatusServiceUnavailable || !strings.Contains(res.Body.String(), "ownership_handoff_unavailable") {
		t.Fatalf("missing adapter=%d %s", res.Code, res.Body.String())
	}
	contract := newResponseContract(t)
	contract.validate(t, http.MethodPost, "/v1/developer/brand-clouds/"+source.BrandCloudID+"/owner-transfer", res)
	var count int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM brand_cloud_owner_transfers`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("request/email created without evidence: %d %v", count, err)
	}
}

func TestOwnerTransferFinancialBlockersHaveExplicitHTTPResults(t *testing.T) {
	env := newIntegrationEnv(t)
	source := verifiedDeveloperForTest(t, env, "financial-http-source@example.test")
	verifiedDeveloperForTest(t, env, "financial-http-target@example.test")
	contract := newResponseContract(t)
	path := "/v1/developer/brand-clouds/" + source.BrandCloudID + "/owner-transfer"
	for _, tc := range []struct {
		evidence apiHandoffEvidence
		code     string
	}{{apiHandoffEvidence{Balance: -1}, "balance_negative"}, {apiHandoffEvidence{Balance: 1, Blockers: []string{"usage_unsettled"}}, "ownership_handoff_financial_blocked"}} {
		if err := env.store.ConfigureOwnershipHandoff(store.OwnershipHandoffOptions{Eligibility: tc.evidence, Producers: store.RequiredHandoffProducers()}); err != nil {
			t.Fatal(err)
		}
		res := performJSON(env.router, http.MethodPost, path, map[string]any{"target_email": "financial-http-target@example.test"}, source.AccessToken)
		if res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), tc.code) {
			t.Fatalf("financial blocker=%d %s", res.Code, res.Body.String())
		}
		contract.validate(t, http.MethodPost, path, res)
	}
}
