package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestIntegrationInternalOTAPeriodBrandCloudInventory(t *testing.T) {
	env := newIntegrationEnv(t)
	env.server.ConfigureInternalAuthToken("ota-inventory-token")
	first := verifiedDeveloperForTest(t, env, "ota-inventory-first@example.test")
	second := verifiedDeveloperForTest(t, env, "ota-inventory-second@example.test")
	now := time.Now().UTC()
	current := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	previous := current.AddDate(0, -1, 0)
	if _, err := env.db.Exec(context.Background(), `UPDATE organizations SET created_at=$2 WHERE id IN ($1,$3)`, first.BrandCloudID, previous.Add(time.Hour), second.BrandCloudID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(context.Background(), `UPDATE organizations SET status='disabled' WHERE id=$1`, second.BrandCloudID); err != nil {
		t.Fatal(err)
	}
	path := "/v1/internal/ota-period-brand-clouds?month=" + previous.Format("2006-01")
	if r := performJSON(env.router, http.MethodGet, path, nil, "wrong-token"); r.Code != http.StatusUnauthorized {
		t.Fatalf("untrusted inventory status=%d", r.Code)
	}
	if r := performJSON(env.router, http.MethodGet, "/v1/internal/ota-period-brand-clouds?month="+current.Format("2006-01"), nil, "ota-inventory-token"); r.Code != http.StatusBadRequest {
		t.Fatalf("open month inventory status=%d", r.Code)
	}
	r := performJSON(env.router, http.MethodGet, path, nil, "ota-inventory-token")
	if r.Code != http.StatusOK || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("inventory status=%d cache=%q body=%s", r.Code, r.Header().Get("Cache-Control"), r.Body.String())
	}
	var got struct {
		Month         string   `json:"month"`
		BrandCloudIDs []string `json:"brand_cloud_ids"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Month != previous.Format("2006-01") || len(got.BrandCloudIDs) != 2 || got.BrandCloudIDs[0] == got.BrandCloudIDs[1] {
		t.Fatalf("incomplete Brand Cloud inventory: %+v", got)
	}
	seen := map[string]bool{}
	for _, id := range got.BrandCloudIDs {
		seen[id] = true
	}
	if !seen[first.BrandCloudID] || !seen[second.BrandCloudID] {
		t.Fatalf("disabled or zero-use Brand Cloud omitted")
	}
}
