package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

func TestIntegrationInternalProductOTAGrantFailsClosed(t *testing.T) {
	env := newIntegrationEnv(t)
	env.server.ConfigureInternalAuthToken("ota-internal-token")
	owner := verifiedDeveloperForTest(t, env, "ota-grant-owner@example.test")
	ctx := context.Background()
	product, err := env.store.CreateDeviceItemProfile(ctx, store.DeviceItemProfileCreateInput{
		BrandCloudID: owner.BrandCloudID, ProfileKey: "ota-grant",
		DisplayName: "OTA Grant Product", Category: model.DeviceCategoryMQTT,
		CAProfile: "fixture-ca", IssuerProfile: "fixture-issuer",
		ServiceOptions: []string{"mqtt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pathFor := func(productID string) string {
		return "/v1/internal/brand-clouds/" + owner.BrandCloudID + "/products/" + productID + "/ota-grant"
	}
	path := pathFor(product.ID)
	if r := performJSON(env.router, http.MethodGet, path, nil, "wrong-token"); r.Code != http.StatusUnauthorized {
		t.Fatalf("untrusted status=%d body=%s", r.Code, r.Body.String())
	}
	get := func(productID string) struct {
		Enabled  bool   `json:"enabled"`
		Active   bool   `json:"active"`
		Product  string `json:"product_id"`
		Revision int64  `json:"product_service_revision"`
	} {
		t.Helper()
		r := performJSON(env.router, http.MethodGet, pathFor(productID), nil, "ota-internal-token")
		if r.Code != http.StatusOK {
			t.Fatalf("grant status=%d body=%s", r.Code, r.Body.String())
		}
		var body struct {
			Enabled  bool   `json:"enabled"`
			Active   bool   `json:"active"`
			Product  string `json:"product_id"`
			Revision int64  `json:"product_service_revision"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if got := get(product.ID); got.Enabled || !got.Active || got.Product != product.ID {
		t.Fatalf("ungranted Product = %+v", got)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_item_profiles SET service_options='["mqtt","ota"]'::jsonb WHERE id=$1`, product.ID); err != nil {
		t.Fatal(err)
	}
	grantPayload := []byte(`{"options":["mqtt","ota"],"bindings":[]}`)
	grantSHA := sha256.Sum256(grantPayload)
	if _, err := env.db.Exec(ctx, `
		INSERT INTO product_service_grants
		  (product_id, revision, brand_cloud_id, catalog_revision, options, bindings, snapshot_sha256)
		VALUES ($1, 1, $2, 1, '["mqtt","ota"]'::jsonb, '[]'::jsonb, $3)
	`, product.ID, owner.BrandCloudID, hex.EncodeToString(grantSHA[:])); err != nil {
		t.Fatal(err)
	}
	if got := get(product.ID); !got.Enabled || got.Revision != 1 {
		t.Fatalf("active OTA grant = %+v", got)
	}
	corruptProduct, err := env.store.CreateDeviceItemProfile(ctx, store.DeviceItemProfileCreateInput{
		BrandCloudID: owner.BrandCloudID, ProfileKey: "ota-corrupt-grant",
		DisplayName: "Corrupt OTA Grant Product", Category: model.DeviceCategoryMQTT,
		CAProfile: "fixture-ca", IssuerProfile: "fixture-issuer",
		ServiceOptions: []string{"mqtt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_item_profiles SET service_options='["mqtt","ota"]'::jsonb WHERE id=$1`, corruptProduct.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `
		INSERT INTO product_service_grants
		  (product_id, revision, brand_cloud_id, catalog_revision, options, bindings, snapshot_sha256)
		VALUES ($1, 1, $2, 1, '["mqtt","ota"]'::jsonb, '[]'::jsonb, repeat('f',64))
	`, corruptProduct.ID, owner.BrandCloudID); err != nil {
		t.Fatal(err)
	}
	if got := get(corruptProduct.ID); got.Enabled {
		t.Fatalf("corrupt grant digest enabled OTA: %+v", got)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_item_profiles SET service_options='["mqtt"]'::jsonb WHERE id=$1`, product.ID); err != nil {
		t.Fatal(err)
	}
	if got := get(product.ID); got.Enabled {
		t.Fatalf("stale grant enabled OTA: %+v", got)
	}
	historyPath := "/v1/internal/brand-clouds/" + owner.BrandCloudID + "/products/" + product.ID + "/ota-grants/1"
	if r := performJSON(env.router, http.MethodGet, historyPath, nil, "wrong-token"); r.Code != http.StatusUnauthorized {
		t.Fatalf("untrusted historical grant status=%d", r.Code)
	}
	if r := performJSON(env.router, http.MethodGet, strings.TrimSuffix(historyPath, "1")+"invalid", nil, "ota-internal-token"); r.Code != http.StatusBadRequest {
		t.Fatalf("invalid historical revision status=%d", r.Code)
	}
	if r := performJSON(env.router, http.MethodGet, strings.TrimSuffix(historyPath, "1")+"9", nil, "ota-internal-token"); r.Code != http.StatusNotFound {
		t.Fatalf("missing historical revision status=%d", r.Code)
	}
	var firstCreated time.Time
	if err := env.db.QueryRow(ctx, `SELECT created_at FROM product_service_grants WHERE product_id=$1 AND revision=1`, product.ID).Scan(&firstCreated); err != nil {
		t.Fatal(err)
	}
	secondCreated := firstCreated.Add(time.Second)
	secondPayload := []byte(`{"options":["mqtt"],"bindings":[]}`)
	secondSHA := sha256.Sum256(secondPayload)
	if _, err := env.db.Exec(ctx, `INSERT INTO product_service_grants
		(product_id,revision,brand_cloud_id,catalog_revision,options,bindings,snapshot_sha256,created_at)
		VALUES($1,2,$2,1,'["mqtt"]'::jsonb,'[]'::jsonb,$3,$4)`, product.ID, owner.BrandCloudID, hex.EncodeToString(secondSHA[:]), secondCreated); err != nil {
		t.Fatal(err)
	}
	readHistory := func(path string) store.HistoricalProductOTAGrant {
		t.Helper()
		r := performJSON(env.router, http.MethodGet, path, nil, "ota-internal-token")
		if r.Code != http.StatusOK || r.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("historical grant status=%d cache=%q body=%s", r.Code, r.Header().Get("Cache-Control"), r.Body.String())
		}
		var grant store.HistoricalProductOTAGrant
		if err := json.Unmarshal(r.Body.Bytes(), &grant); err != nil {
			t.Fatal(err)
		}
		return grant
	}
	old := readHistory(historyPath)
	if !old.Enabled || old.ProductServiceRevision != 1 || old.ServiceGrantSHA256 != hex.EncodeToString(grantSHA[:]) ||
		!old.ValidFrom.Equal(firstCreated) || old.ValidUntil == nil || !old.ValidUntil.Equal(secondCreated) {
		t.Fatalf("historical OTA grant was not retained with its revision interval: %+v", old)
	}
	newGrant := readHistory(strings.TrimSuffix(historyPath, "1") + "2")
	if newGrant.Enabled || newGrant.ProductServiceRevision != 2 || newGrant.ValidUntil != nil {
		t.Fatalf("disabled OTA revision = %+v", newGrant)
	}
	corruptHistory := performJSON(env.router, http.MethodGet, "/v1/internal/brand-clouds/"+owner.BrandCloudID+"/products/"+corruptProduct.ID+"/ota-grants/1", nil, "ota-internal-token")
	if corruptHistory.Code != http.StatusConflict {
		t.Fatalf("corrupt historical grant status=%d body=%s", corruptHistory.Code, corruptHistory.Body.String())
	}
	loggingProduct, err := env.store.CreateDeviceItemProfile(ctx, store.DeviceItemProfileCreateInput{
		BrandCloudID: owner.BrandCloudID, ProfileKey: "ota-logging-grant",
		DisplayName: "OTA and Logging Product", Category: model.DeviceCategoryMQTT,
		CAProfile: "fixture-ca", IssuerProfile: "fixture-issuer",
		ServiceOptions: []string{"mqtt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	loggingPayload := []byte(`{"options":["device_logging","mqtt","ota"],"bindings":[],"log_retention_days":30}`)
	loggingSHA := sha256.Sum256(loggingPayload)
	if _, err := env.db.Exec(ctx, `INSERT INTO product_service_grants
		(product_id,revision,brand_cloud_id,catalog_revision,options,bindings,log_retention_days,snapshot_sha256)
		VALUES($1,1,$2,1,'["device_logging","mqtt","ota"]'::jsonb,'[]'::jsonb,30,$3)`,
		loggingProduct.ID, owner.BrandCloudID, hex.EncodeToString(loggingSHA[:])); err != nil {
		t.Fatal(err)
	}
	loggingHistory := readHistory("/v1/internal/brand-clouds/" + owner.BrandCloudID + "/products/" + loggingProduct.ID + "/ota-grants/1")
	if !loggingHistory.Enabled || loggingHistory.ServiceGrantSHA256 != hex.EncodeToString(loggingSHA[:]) {
		t.Fatalf("OTA grant with logging retention digest = %+v", loggingHistory)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_item_profiles SET service_options='["mqtt","ota"]'::jsonb WHERE id=$1`, product.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_item_profiles SET status='disabled' WHERE id=$1`, product.ID); err != nil {
		t.Fatal(err)
	}
	if got := get(product.ID); got.Enabled || got.Active {
		t.Fatalf("disabled Product grant = %+v", got)
	}
}
