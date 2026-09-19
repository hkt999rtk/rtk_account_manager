package store

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"rtk_account_manager/internal/model"
)

func TestLegacyProductGrantBackfillPreservesOptionsAndExistingRuns(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "legacy-grant-owner")
	legacy := authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "legacy-shadow-only")
	legacy.ServiceOptions = []string{"iot_shadow"} // Do not infer MQTT for an existing Product.
	product, err := env.store.CreateDeviceItemProfileAsUser(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	second := authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "legacy-video")
	second.ServiceOptions = []string{"video_storage", "mqtt"}
	videoProduct, err := env.store.CreateDeviceItemProfileAsUser(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(run model.ProductionRun, profile model.DeviceItemProfile) (string, error) {
		return "signed-fixture", nil
	}
	oldRun, _, err := env.store.IssueProductionRunAsUser(ctx, authorizedProductionInput(owner.User.ID, owner.BrandCloud.ID, product.ID), issue)
	if err != nil || oldRun.ProductServiceRevision != nil {
		t.Fatalf("legacy run before backfill = %+v, %v", oldRun, err)
	}

	dry, err := env.store.BackfillLegacyProductServiceGrants(ctx, false, "")
	if err != nil || !dry.Ready || dry.Products != 2 || dry.NeedsBackfill != 2 || dry.WithoutMQTT != 1 || dry.Applied != 0 || dry.OptionCounts["iot_shadow"] != 1 {
		t.Fatalf("dry-run report = %+v, %v", dry, err)
	}
	var grants int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM product_service_grants`).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("dry-run wrote %d grants: %v", grants, err)
	}
	applied, err := env.store.BackfillLegacyProductServiceGrants(ctx, true, dry.SnapshotSHA256)
	if err != nil || !applied.Ready || applied.Applied != 2 {
		t.Fatalf("apply report = %+v, %v", applied, err)
	}
	for _, tc := range []struct {
		id      string
		options []string
	}{
		{product.ID, []string{"iot_shadow"}},
		{videoProduct.ID, []string{"mqtt", "video_storage"}},
	} {
		var revision, catalogRevision int64
		var legacy bool
		var optionsJSON, bindingsJSON []byte
		var digest string
		if err := env.db.QueryRow(ctx, `SELECT revision,catalog_revision,legacy,options,bindings,snapshot_sha256 FROM product_service_grants WHERE product_id=$1`, tc.id).Scan(&revision, &catalogRevision, &legacy, &optionsJSON, &bindingsJSON, &digest); err != nil {
			t.Fatal(err)
		}
		var options []string
		if err := json.Unmarshal(optionsJSON, &options); err != nil {
			t.Fatal(err)
		}
		_, _, wantDigest, err := encodeProductServiceGrant(tc.options, []PlatformServiceCatalogOption{})
		if err != nil || revision != 1 || catalogRevision != 0 || !legacy || !slices.Equal(options, tc.options) || string(bindingsJSON) != "[]" || digest != wantDigest {
			t.Fatalf("backfilled grant %s = rev=%d catalog=%d legacy=%t options=%v bindings=%s digest=%s, %v", tc.id, revision, catalogRevision, legacy, options, bindingsJSON, digest, err)
		}
	}
	var oldRunRevision *int64
	if err := env.db.QueryRow(ctx, `SELECT product_service_revision FROM factory_production_runs WHERE id=$1`, oldRun.ID).Scan(&oldRunRevision); err != nil || oldRunRevision != nil {
		t.Fatalf("old run revision changed: %v, %v", oldRunRevision, err)
	}
	env.store.ConfigurePlatformServiceProductWrites("local-test", true)
	newRun, _, err := env.store.IssueProductionRunAsUser(ctx, authorizedProductionInput(owner.User.ID, owner.BrandCloud.ID, product.ID), issue)
	if err != nil || newRun.ProductServiceRevision == nil || *newRun.ProductServiceRevision != 1 {
		t.Fatalf("new legacy-Product run = %+v, %v", newRun, err)
	}
	postApply, err := env.store.BackfillLegacyProductServiceGrants(ctx, false, "")
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := env.store.BackfillLegacyProductServiceGrants(ctx, true, postApply.SnapshotSHA256)
	if err != nil || !repeat.Ready || repeat.Applied != 0 || repeat.AlreadyVersioned != 2 {
		t.Fatalf("idempotent backfill = %+v, %v", repeat, err)
	}
}

func TestLegacyProductGrantBackfillRejectsMismatchedSnapshotAtomically(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "legacy-grant-mismatch")
	first, err := env.store.CreateDeviceItemProfileAsUser(ctx, authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "first"))
	if err != nil {
		t.Fatal(err)
	}
	initial, err := env.store.BackfillLegacyProductServiceGrants(ctx, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.BackfillLegacyProductServiceGrants(ctx, true, initial.SnapshotSHA256); err != nil {
		t.Fatal(err)
	}
	second, err := env.store.CreateDeviceItemProfileAsUser(ctx, authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "second"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_item_profiles SET service_options='["iot_shadow"]'::jsonb WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	stale, err := env.store.BackfillLegacyProductServiceGrants(ctx, true, initial.SnapshotSHA256)
	if err != nil || stale.Ready || stale.Applied != 0 || stale.IssueCount < 1 {
		t.Fatalf("stale reviewed snapshot was accepted = %+v, %v", stale, err)
	}
	current, err := env.store.BackfillLegacyProductServiceGrants(ctx, false, "")
	if err != nil {
		t.Fatal(err)
	}
	report, err := env.store.BackfillLegacyProductServiceGrants(ctx, true, current.SnapshotSHA256)
	if err != nil || report.Ready || report.IssueCount != 1 || report.Applied != 0 || report.NeedsBackfill != 1 {
		t.Fatalf("mismatch was not atomic = %+v, %v", report, err)
	}
	var grants int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM product_service_grants WHERE product_id=$1`, second.ID).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("mismatched apply inserted another Product grant: %d, %v", grants, err)
	}
}

func TestLegacyProductGrantBackfillFlagsUnknownUnversionedOption(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "legacy-unknown-option")
	product, err := env.store.CreateDeviceItemProfileAsUser(ctx, authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "unknown-option"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE device_item_profiles SET service_options='["mystery_plugin"]'::jsonb WHERE id=$1`, product.ID); err != nil {
		t.Fatal(err)
	}
	report, err := env.store.BackfillLegacyProductServiceGrants(ctx, false, "")
	if err != nil || report.Ready || report.IssueCount != 1 || report.NeedsBackfill != 1 || report.OptionCounts["mystery_plugin"] != 1 {
		t.Fatalf("unknown unversioned option was silently backfilled: %+v, %v", report, err)
	}
	apply, err := env.store.BackfillLegacyProductServiceGrants(ctx, true, report.SnapshotSHA256)
	if err != nil || apply.Ready || apply.Applied != 0 {
		t.Fatalf("unknown option was applied: %+v, %v", apply, err)
	}
}

func TestLegacyProductGrantBackfillRejectsCorruptExistingDigest(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "legacy-corrupt-digest")
	product, err := env.store.CreateDeviceItemProfileAsUser(ctx, authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "digest-check"))
	if err != nil {
		t.Fatal(err)
	}
	dry, err := env.store.BackfillLegacyProductServiceGrants(ctx, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.BackfillLegacyProductServiceGrants(ctx, true, dry.SnapshotSHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE product_service_grants SET snapshot_sha256=repeat('f',64) WHERE product_id=$1`, product.ID); err != nil {
		t.Fatal(err)
	}
	report, err := env.store.BackfillLegacyProductServiceGrants(ctx, false, "")
	if err != nil || report.Ready || report.IssueCount != 1 || report.Applied != 0 {
		t.Fatalf("corrupt digest was not reported: %+v, %v", report, err)
	}
}
