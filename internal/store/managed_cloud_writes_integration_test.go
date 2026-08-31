package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func sameManagedCloudJSON(a, b ManagedBrandCloud) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(x) == string(y) && a.Operational == b.Operational
}

func TestManagedCloudWriteConcurrentReplayAndQuota(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	user := handoffDeveloper(t, env, "crud-owner")
	if err := env.store.SetDeveloperCloudLimit(ctx, user.User.ID, 2); err != nil {
		t.Fatal(err)
	}
	in := ManagedCloudWrite{Name: stringPtr("  測試雲  "), Description: stringPtr("description")}
	var wg sync.WaitGroup
	results := make(chan ManagedBrandCloud, 10)
	failures := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := env.store.CreateManagedBrandCloud(ctx, user.User.ID, "same-key", in)
			if err != nil {
				failures <- err
			} else {
				results <- c
			}
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var first ManagedBrandCloud
	for c := range results {
		if first.ID == "" {
			first = c
		}
		if !sameManagedCloudJSON(first, c) {
			t.Fatalf("replay changed: %+v %+v", first, c)
		}
	}
	if first.ID == "" || first.Name != "測試雲" || first.Description != "description" || first.OwnerUserID != user.User.ID || first.MyRole != "owner" || first.OwnershipVersion != 1 {
		t.Fatalf("cloud: %+v", first)
	}
	var receipts, audits, owners int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM managed_cloud_write_receipts),(SELECT count(*) FROM audit_events WHERE event_type='developer_brand_cloud_created' AND organization_id=$1),(SELECT count(*) FROM organization_members WHERE organization_id=$1 AND role='owner')`, first.ID).Scan(&receipts, &audits, &owners); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || audits != 1 || owners != 1 {
		t.Fatalf("duplicate effects receipts/audits/owners=%d/%d/%d", receipts, audits, owners)
	}
	if _, err := env.store.CreateManagedBrandCloud(ctx, user.User.ID, "new-key", in); !errors.Is(err, ErrDeveloperCloudLimitExceeded) {
		t.Fatalf("quota bypass: %v", err)
	}
	if _, err := env.store.CreateManagedBrandCloud(ctx, user.User.ID, "same-key", ManagedCloudWrite{Name: stringPtr("different")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed payload: %v", err)
	}
	replay, err := New(env.db).CreateManagedBrandCloud(ctx, user.User.ID, "same-key", in)
	if err != nil || !sameManagedCloudJSON(first, replay) {
		t.Fatalf("restart/quota replay: %+v %v", replay, err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE managed_cloud_write_receipts SET response='{}'`); err == nil {
		t.Fatal("receipt mutable")
	}
	other := handoffDeveloper(t, env, "crud-other")
	independent, err := env.store.CreateManagedBrandCloud(ctx, other.User.ID, "same-key", in)
	if err != nil || independent.ID == first.ID {
		t.Fatalf("cross-actor key collision: %+v %v", independent, err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE organizations SET deleted_at=now() WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CreateManagedBrandCloud(ctx, user.User.ID, "same-key", in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tombstone replay resurrected cloud: %v", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE organizations SET deleted_at=now() WHERE id=$1;`, user.BrandCloud.ID); err != nil {
		t.Fatal(err)
	}
	empty, err := env.store.ListManagedBrandClouds(ctx, user.User.ID, "all", 25, 0)
	if err != nil || empty.Total != 0 || empty.OwnedCount != 0 {
		t.Fatalf("empty list: %+v %v", empty, err)
	}
	if _, err := env.store.CreateManagedBrandCloud(ctx, user.User.ID, "after-last-cloud", ManagedCloudWrite{Name: stringPtr(strings.Repeat("雲", 255)), Description: stringPtr(strings.Repeat("字", 2000))}); err != nil {
		t.Fatalf("eligible empty-list user cannot create: %v", err)
	}
}

func TestManagedCloudPatchScopeReplayAndHandoffFence(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "patch-owner")
	other := handoffDeveloper(t, env, "patch-other")
	id := owner.BrandCloud.ID
	original, err := env.store.GetManagedBrandCloud(ctx, owner.User.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	in := ManagedCloudWrite{Name: stringPtr("Renamed"), Description: stringPtr("new description")}
	changed, err := env.store.UpdateManagedBrandCloud(ctx, owner.User.ID, id, "patch", in)
	if err != nil || changed.ID != original.ID || !reflect.DeepEqual(changed.TenantSlug, original.TenantSlug) || changed.OwnerUserID != original.OwnerUserID || changed.OwnershipVersion != original.OwnershipVersion || changed.Name != "Renamed" {
		t.Fatalf("rename: %+v %v", changed, err)
	}
	cleared, err := env.store.UpdateManagedBrandCloud(ctx, owner.User.ID, id, "clear", ManagedCloudWrite{Description: stringPtr("")})
	if err != nil || cleared.Description != "" || cleared.Name != "Renamed" {
		t.Fatalf("clear: %+v %v", cleared, err)
	}
	replay, err := New(env.db).UpdateManagedBrandCloud(ctx, owner.User.ID, id, "patch", in)
	if err != nil || !sameManagedCloudJSON(changed, replay) {
		t.Fatalf("stable replay: %+v %v", replay, err)
	}
	current, err := env.store.GetManagedBrandCloud(ctx, owner.User.ID, id)
	if err != nil || current.Description != "" {
		t.Fatalf("replay reapplied stale change: %+v %v", current, err)
	}
	if _, err := env.store.UpdateManagedBrandCloud(ctx, owner.User.ID, id, "patch", ManagedCloudWrite{Name: stringPtr("conflict")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("key rebind: %v", err)
	}
	for _, role := range []string{"admin", "member", "viewer"} {
		if _, err := env.db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role,access_scope) VALUES($1,$2,$3,CASE WHEN $3='viewer' THEN '{"kind":"all_products"}'::jsonb ELSE NULL END) ON CONFLICT(organization_id,user_id) DO UPDATE SET role=EXCLUDED.role,access_scope=EXCLUDED.access_scope`, id, other.User.ID, role); err != nil {
			t.Fatal(err)
		}
		if _, err := env.store.UpdateManagedBrandCloud(ctx, other.User.ID, id, "patch", in); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s updated cloud: %v", role, err)
		}
	}
	if _, err := env.store.UpdateManagedBrandCloud(ctx, owner.User.ID, other.BrandCloud.ID, "patch", in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross cloud update: %v", err)
	}
	configureTestHandoff(t, env)
	handoffRequest(t, env, owner, other, "patch-transfer")
	if _, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, other.User.ID, "patch-transfer", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.UpdateManagedBrandCloud(ctx, owner.User.ID, id, "patch", in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replay bypassed handoff fence: %v", err)
	}
	if _, err := env.store.UpdateManagedBrandCloud(ctx, owner.User.ID, id, "new-patch", in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mutation bypassed handoff fence: %v", err)
	}
	view, err := env.store.GetManagedBrandCloud(ctx, owner.User.ID, id)
	if err != nil || view.Operational || len(view.Capabilities) != 0 {
		t.Fatalf("fenced detail: %+v %v", view, err)
	}
}

func TestManagedCloudWriteValidationActivationAndRollback(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	user := handoffDeveloper(t, env, "validation-owner")
	for _, in := range []ManagedCloudWrite{{}, {Name: stringPtr("")}, {Name: stringPtr(" \t ")}, {Name: stringPtr(strings.Repeat("雲", 256))}, {Name: stringPtr("a\x00b")}, {Name: stringPtr("valid"), Description: stringPtr(strings.Repeat("a", 2001))}} {
		if _, err := env.store.CreateManagedBrandCloud(ctx, user.User.ID, "validation", in); !errors.Is(err, ErrInvalidManagedCloudWrite) {
			t.Fatalf("invalid payload accepted: %v", err)
		}
	}
	for _, key := range []string{"", strings.Repeat("x", 201), "a b", "\n"} {
		if _, err := env.store.CreateManagedBrandCloud(ctx, user.User.ID, key, ManagedCloudWrite{Name: stringPtr("valid")}); !errors.Is(err, ErrInvalidManagedCloudWrite) {
			t.Fatalf("invalid key accepted: %v", err)
		}
	}
	// Force the final transactional audit step to fail; no cloud or receipt may survive.
	if _, err := env.db.Exec(ctx, `CREATE FUNCTION reject_crud_test_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type IN ('developer_brand_cloud_created','developer_brand_cloud_updated') THEN RAISE EXCEPTION 'synthetic audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_crud_test_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_crud_test_audit()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Exec(context.Background(), `DROP TRIGGER IF EXISTS reject_crud_test_audit ON audit_events; DROP FUNCTION IF EXISTS reject_crud_test_audit()`)
	})
	if _, err := env.store.CreateManagedBrandCloud(ctx, user.User.ID, "rollback", ManagedCloudWrite{Name: stringPtr("rollback")}); err == nil {
		t.Fatal("audit failure ignored")
	}
	if _, err := env.store.UpdateManagedBrandCloud(ctx, user.User.ID, user.BrandCloud.ID, "rollback", ManagedCloudWrite{Name: stringPtr("rollback")}); err == nil {
		t.Fatal("patch audit failure ignored")
	}
	page, err := env.store.ListManagedBrandClouds(ctx, user.User.ID, "owned", 25, 0)
	if err != nil || page.Total != 1 || page.BrandClouds[0].Name != user.BrandCloud.Name {
		t.Fatalf("partial mutation: %+v %v", page, err)
	}
	var receipts int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM managed_cloud_write_receipts`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("partial receipt %d %v", receipts, err)
	}
	if _, err := env.db.Exec(ctx, `DROP TRIGGER reject_crud_test_audit ON audit_events; DROP FUNCTION reject_crud_test_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CreateManagedBrandCloud(ctx, user.User.ID, "rollback", ManagedCloudWrite{Name: stringPtr("retry")}); err != nil {
		t.Fatalf("rolled back key poisoned: %v", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=false,signup_pending_verification=true WHERE id=$1`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CreateManagedBrandCloud(ctx, user.User.ID, "rollback", ManagedCloudWrite{Name: stringPtr("retry")}); !errors.Is(err, ErrAccountNotActivated) {
		t.Fatalf("unverified replay: %v", err)
	}
	if _, err := env.store.UpdateManagedBrandCloud(ctx, user.User.ID, user.BrandCloud.ID, "activation", ManagedCloudWrite{Name: stringPtr("blocked")}); !errors.Is(err, ErrAccountNotActivated) {
		t.Fatalf("unverified patch: %v", err)
	}
}
