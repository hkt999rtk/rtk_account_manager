package store

import (
	"context"
	"errors"
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

func TestIntegrationOTAPeriodGrantSealWaitsForInFlightGrantInsert(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := handoffDeveloper(t, env, "ota-period-inflight")
	product, err := env.store.CreateDeviceItemProfileAsUser(ctx,
		authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "ota-period-inflight-product"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	optionsJSON, bindingsJSON, digest, err := encodeProductServiceGrant(
		[]string{"mqtt", "ota"}, []PlatformServiceCatalogOption{})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())
	if _, err := writer.Exec(ctx, `INSERT INTO product_service_grants
		(product_id,revision,brand_cloud_id,catalog_revision,options,bindings,snapshot_sha256,legacy,created_at)
		VALUES($1,1,$2,0,$3,$4,$5,true,$6)`, product.ID, owner.BrandCloud.ID,
		optionsJSON, bindingsJSON, digest, start.AddDate(0, 0, 10)); err != nil {
		t.Fatal(err)
	}
	writerPID := transactionBackendPID(t, ctx, writer)
	var seal OTAPeriodGrantSeal
	results := make(chan error, 1)
	go func() {
		var sealErr error
		seal, sealErr = env.store.BuildOTAPeriodGrantSeal(ctx, owner.BrandCloud.ID, start, end)
		results <- sealErr
	}()
	awaitBlockedConnections(t, ctx, writer, writerPID, 1, results)
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-results:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !reflect.DeepEqual(seal.ProductIDs, []string{product.ID}) {
		t.Fatalf("seal omitted the committed in-flight grant: %v", seal.ProductIDs)
	}
	replay, err := env.store.BuildOTAPeriodGrantSeal(ctx, owner.BrandCloud.ID, start, end)
	if err != nil || replay.SealID != seal.SealID || replay.SourceSHA256 != seal.SourceSHA256 {
		t.Fatalf("seal changed after the writer committed: %+v %+v %v", seal, replay, err)
	}
}

func TestIntegrationOTAPeriodGrantSealExcludesPostSealInsertFromEarlierTransaction(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "ota-period-insert-clock")
	product, err := env.store.CreateDeviceItemProfileAsUser(ctx,
		authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "ota-period-insert-clock-product"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	writer, err := env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())
	var writerStartedAt time.Time
	if err := writer.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&writerStartedAt); err != nil {
		t.Fatal(err)
	}
	before, err := env.store.BuildOTAPeriodGrantSeal(ctx, owner.BrandCloud.ID, start, end)
	if err != nil {
		t.Fatal(err)
	}
	optionsJSON, bindingsJSON, digest, err := encodeProductServiceGrant(
		[]string{"mqtt", "ota"}, []PlatformServiceCatalogOption{})
	if err != nil {
		t.Fatal(err)
	}
	var insertedAt time.Time
	if err := writer.QueryRow(ctx, `INSERT INTO product_service_grants
		(product_id,revision,brand_cloud_id,catalog_revision,options,bindings,snapshot_sha256,legacy)
		VALUES($1,1,$2,0,$3,$4,$5,true) RETURNING created_at`,
		product.ID, owner.BrandCloud.ID, optionsJSON, bindingsJSON, digest).Scan(&insertedAt); err != nil {
		t.Fatal(err)
	}
	if !insertedAt.After(writerStartedAt) {
		t.Fatalf("grant time %s reused transaction start %s", insertedAt, writerStartedAt)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := env.store.BuildOTAPeriodGrantSeal(ctx, owner.BrandCloud.ID, start, end)
	if err != nil || after.SealID != before.SealID || after.SourceSHA256 != before.SourceSHA256 {
		t.Fatalf("post-seal grant changed historical seal: %+v %+v %v", before, after, err)
	}
}

func TestIntegrationOTAPeriodGrantSealRejectsCorruptGrantSnapshot(t *testing.T) {
	for _, corrupt := range []string{"options", "bindings"} {
		t.Run(corrupt, func(t *testing.T) {
			env := newStoreIntegrationEnv(t)
			ctx := context.Background()
			owner := handoffDeveloper(t, env, "ota-period-corrupt-"+corrupt)
			product, err := env.store.CreateDeviceItemProfileAsUser(ctx,
				authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "ota-period-corrupt-product-"+corrupt))
			if err != nil {
				t.Fatal(err)
			}
			optionsJSON, bindingsJSON, digest, err := encodeProductServiceGrant(
				[]string{"mqtt"}, []PlatformServiceCatalogOption{})
			if err != nil {
				t.Fatal(err)
			}
			if corrupt == "options" {
				optionsJSON = []byte(`["mqtt","ota"]`)
			} else {
				bindingsJSON = []byte(`[{"code":"mqtt"}]`)
			}
			start := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
			end := start.AddDate(0, 1, 0)
			if _, err := env.db.Exec(ctx, `INSERT INTO product_service_grants
				(product_id,revision,brand_cloud_id,catalog_revision,options,bindings,snapshot_sha256,legacy,created_at)
				VALUES($1,1,$2,0,$3,$4,$5,true,$6)`, product.ID, owner.BrandCloud.ID,
				optionsJSON, bindingsJSON, digest, start.AddDate(0, 0, 10)); err != nil {
				t.Fatal(err)
			}
			if _, err := env.store.BuildOTAPeriodGrantSeal(ctx, owner.BrandCloud.ID, start, end); !errors.Is(err, ErrOTAPeriodSealInvalid) {
				t.Fatalf("corrupt %s was sealed: %v", corrupt, err)
			}
		})
	}
}
