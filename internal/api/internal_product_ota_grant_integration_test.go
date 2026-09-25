package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

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
