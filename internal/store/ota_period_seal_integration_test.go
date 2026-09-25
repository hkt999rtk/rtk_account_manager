package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestIntegrationOTAPeriodGrantSealUsesHistoricalGrantRows(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "ota-period-grant")
	product, err := env.store.CreateDeviceItemProfileAsUser(ctx,
		authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "ota-period-product"))
	if err != nil {
		t.Fatal(err)
	}
	for _, revision := range []struct {
		number int64
		date   time.Time
		opts   []string
	}{
		{1, time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC), []string{"mqtt", "ota"}},
		{2, time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC), []string{"mqtt"}},
	} {
		optionsJSON, bindingsJSON, digest, err := encodeProductServiceGrant(
			revision.opts, []PlatformServiceCatalogOption{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(ctx, `
			INSERT INTO product_service_grants
			  (product_id,revision,brand_cloud_id,catalog_revision,options,bindings,snapshot_sha256,legacy,created_at)
			VALUES ($1,$2,$3,0,$4,$5,$6,true,$7)
		`, product.ID, revision.number, owner.BrandCloud.ID, optionsJSON,
			bindingsJSON, digest, revision.date); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	seal, err := env.store.BuildOTAPeriodGrantSeal(ctx, owner.BrandCloud.ID, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seal.ProductIDs, []string{product.ID}) {
		t.Fatalf("disabled-before-month-end Product omitted: %v", seal.ProductIDs)
	}
	nextStart := end
	nextSeal, err := env.store.BuildOTAPeriodGrantSeal(ctx, owner.BrandCloud.ID, nextStart, nextStart.AddDate(0, 1, 0))
	if err != nil || !reflect.DeepEqual(nextSeal.ProductIDs, []string{product.ID}) {
		t.Fatalf("retired OTA Product omitted from later period: %+v %v", nextSeal, err)
	}
	replay, err := env.store.BuildOTAPeriodGrantSeal(ctx, owner.BrandCloud.ID, start, end)
	if err != nil || replay.SealID != seal.SealID || replay.SourceSHA256 != seal.SourceSHA256 {
		t.Fatalf("historical seal did not replay exactly: %+v %+v %v", seal, replay, err)
	}
	future := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := env.store.BuildOTAPeriodGrantSeal(ctx, owner.BrandCloud.ID,
		future, future.AddDate(0, 1, 0)); err == nil {
		t.Fatal("future UTC month was sealed")
	}
}
