package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"rtk_account_manager/internal/store"
)

func TestIntegrationInternalBrandCloudBillingTierHistory(t *testing.T) {
	env := newIntegrationEnv(t)
	env.server.ConfigureInternalAuthToken("tier-internal-token")
	owner := verifiedDeveloperForTest(t, env, "ota-tier-history@example.test")
	markEvaluationOrg(t, env, owner.BrandCloudID, 5)
	ctx := context.Background()
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	end := start.AddDate(0, 1, 0)
	path := func(from, until time.Time) string {
		return "/v1/internal/brand-clouds/" + owner.BrandCloudID + "/billing-tier?period_start=" +
			url.QueryEscape(from.Format(time.RFC3339Nano)) + "&period_end=" + url.QueryEscape(until.Format(time.RFC3339Nano))
	}
	if r := performJSON(env.router, http.MethodGet, path(start, end), nil, "wrong-token"); r.Code != http.StatusUnauthorized {
		t.Fatalf("untrusted tier status=%d", r.Code)
	}
	if r := performJSON(env.router, http.MethodGet, path(start, end.Add(time.Second)), nil, "tier-internal-token"); r.Code != http.StatusBadRequest {
		t.Fatalf("partial UTC month status=%d", r.Code)
	}
	get := func(from, until time.Time) store.BrandCloudBillingTierPeriod {
		t.Helper()
		r := performJSON(env.router, http.MethodGet, path(from, until), nil, "tier-internal-token")
		if r.Code != http.StatusOK || r.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("tier status=%d cache=%q body=%s", r.Code, r.Header().Get("Cache-Control"), r.Body.String())
		}
		var result store.BrandCloudBillingTierPeriod
		if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := get(start.AddDate(0, -1, 0), start); result.HistoryCoversStart || result.CommercialForFullPeriod {
		t.Fatalf("migration history claimed an earlier month: %+v", result)
	}
	if result := get(start, end); !result.HistoryCoversStart || result.Tier != "evaluation" || result.CommercialForFullPeriod {
		t.Fatalf("evaluation tier became billable: %+v", result)
	}
	if _, err := env.db.Exec(ctx, `UPDATE organizations SET tier='commercial' WHERE id=$1`, owner.BrandCloudID); err != nil {
		t.Fatal(err)
	}
	if result := get(start, end); !result.CommercialForFullPeriod || result.ChangedWithinPeriod {
		t.Fatalf("complete future commercial month was not proven: %+v", result)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO brand_cloud_tier_events (brand_cloud_id,tier,observed_at) VALUES ($1,'evaluation',$2)`, owner.BrandCloudID, start.AddDate(0, 0, 15)); err != nil {
		t.Fatal(err)
	}
	if result := get(start, end); result.CommercialForFullPeriod || !result.ChangedWithinPeriod {
		t.Fatalf("mid-month tier change was chargeable: %+v", result)
	}
	if _, err := env.db.Exec(ctx, `UPDATE brand_cloud_tier_events SET tier='commercial' WHERE brand_cloud_id=$1`, owner.BrandCloudID); err == nil {
		t.Fatal("tier history was mutable")
	}
}
