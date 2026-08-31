package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func deviceUserWrites(s *Store, ctx context.Context, actor, cloud, device string) map[string]func() error {
	return map[string]func() error{
		"create": func() error {
			_, err := s.CreateDeviceAsUser(ctx, actor, cloud, DeviceInput{Name: "Created", Category: model.DeviceCategoryIPCamera})
			return err
		},
		"update": func() error {
			_, err := s.UpdateDeviceAsUser(ctx, actor, cloud, device, DeviceInput{Name: "Updated", Category: model.DeviceCategoryIPCamera})
			return err
		},
		"status": func() error {
			_, err := s.UpdateDeviceStatusAsUser(ctx, actor, cloud, device, model.DeviceStatusOnline, nil)
			return err
		},
		"delete": func() error { return s.DeleteDeviceAsUser(ctx, actor, cloud, device) },
	}
}

func TestUserDeviceMutationsRecheckViewerHandoffAndActivation(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "device-write-owner")
	other := handoffDeveloper(t, env, "device-write-other")
	d, err := env.store.CreateDeviceAsUser(ctx, owner.User.ID, owner.BrandCloud.ID, DeviceInput{Name: "Original", Category: model.DeviceCategoryIPCamera})
	if err != nil {
		t.Fatal(err)
	}
	deny := func(actor, cloud string) {
		t.Helper()
		for name, write := range deviceUserWrites(env.store, ctx, actor, cloud, d.ID) {
			if err := write(); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s bypassed current authority: %v", name, err)
			}
		}
	}
	deny(other.User.ID, owner.BrandCloud.ID)
	// Platform privilege cannot bypass cloud admission or a viewer ceiling.
	if _, err = env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, other.User.ID); err != nil {
		t.Fatal(err)
	}
	deny(other.User.ID, owner.BrandCloud.ID)
	if _, err = env.db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role,access_scope) VALUES($1,$2,'viewer','{"kind":"all_products"}');`, owner.BrandCloud.ID, other.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = env.db.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type,scope_id,organization_id) SELECT id,'user',$1,'organization',$2::text,$2::text::uuid FROM roles WHERE name='admin'`, other.User.ID, owner.BrandCloud.ID); err != nil {
		t.Fatal(err)
	}
	deny(other.User.ID, owner.BrandCloud.ID)
	for _, write := range []func() error{deviceUserWrites(env.store, ctx, owner.User.ID, owner.BrandCloud.ID, d.ID)["update"], deviceUserWrites(env.store, ctx, owner.User.ID, owner.BrandCloud.ID, d.ID)["status"]} {
		if err := write(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = env.db.Exec(ctx, `UPDATE users SET signup_pending_verification=true WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	deny(owner.User.ID, owner.BrandCloud.ID)
	if _, err = env.db.Exec(ctx, `UPDATE users SET signup_pending_verification=false WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	configureTestHandoff(t, env)
	handoffRequest(t, env, owner, other, "device-handoff")
	if _, err = env.store.AcceptBrandCloudOwnerTransfer(ctx, other.User.ID, "device-handoff", time.Now()); err != nil {
		t.Fatal(err)
	}
	deny(owner.User.ID, owner.BrandCloud.ID)
	got, err := env.store.GetDevice(ctx, owner.BrandCloud.ID, d.ID)
	if err != nil || got.Name != "Updated" || got.Status != model.DeviceStatusOnline || got.DisabledAt != nil {
		t.Fatalf("denied writes mutated device: %+v, %v", got, err)
	}
	var audits int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE subject_id=$1 AND event_type IN ('device.created','device.updated','device.status.updated','device.deleted')`, d.ID).Scan(&audits); err != nil || audits != 3 {
		t.Fatalf("unexpected successful-write audit count %d: %v", audits, err)
	}
}

func TestUserDeviceMutationsWaitForOwnershipChangeThenDenyFormerOwner(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	source := handoffDeveloper(t, env, "device-race-source")
	target := handoffDeveloper(t, env, "device-race-target")
	d, err := env.store.CreateDeviceAsUser(ctx, source.User.ID, source.BrandCloud.ID, DeviceInput{Name: "Original", Category: model.DeviceCategoryIPCamera})
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err := env.store.HasUserDevicePermission(ctx, source.User.ID, source.BrandCloud.ID, "registry_device.manage", d.ID); err != nil || !allowed {
		t.Fatalf("initial middleware authority: %t %v", allowed, err)
	}
	tx, err := env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// Synthetic ownership boundary, not a Billing protocol acceptance test. Keep
	// the old ACL deliberately: removed membership alone must revoke old writes.
	if err := lockBrandCloudCollaborationTx(ctx, tx, source.BrandCloud.ID, source.User.ID, target.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM organization_members WHERE organization_id=$1 AND user_id=$2`, source.BrandCloud.ID, source.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'owner')`, source.BrandCloud.ID, target.User.ID); err != nil {
		t.Fatal(err)
	}
	blockerPID := transactionBackendPID(t, ctx, tx)
	results := make(chan error, 4)
	for _, write := range deviceUserWrites(env.store, ctx, source.User.ID, source.BrandCloud.ID, d.ID) {
		go func(write func() error) { results <- write() }(write)
	}
	awaitBlockedConnections(t, ctx, env.db, blockerPID, 1, results)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		select {
		case err := <-results:
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("former owner wrote after commit: %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	got, err := env.store.GetDevice(ctx, source.BrandCloud.ID, d.ID)
	if err != nil || got.Name != "Original" || got.DisabledAt != nil {
		t.Fatalf("blocked writes changed device: %+v %v", got, err)
	}
	var devices, audits int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM devices WHERE organization_id=$1),(SELECT count(*) FROM audit_events WHERE organization_id=$1 AND event_type LIKE 'device.%')`, source.BrandCloud.ID).Scan(&devices, &audits); err != nil || devices != 1 || audits != 1 {
		t.Fatalf("race created data/audits: %d/%d %v", devices, audits, err)
	}
}

func TestUserDeviceMutationAuditFailureRollsBackWrite(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "device-audit-owner")
	d, err := env.store.CreateDeviceAsUser(ctx, owner.User.ID, owner.BrandCloud.ID, DeviceInput{Name: "Original", Category: model.DeviceCategoryIPCamera})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.db.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT reject_device_write_test CHECK(event_type NOT IN ('device.created','device.updated','device.deleted','device.status.updated')) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	defer env.db.Exec(context.Background(), `ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS reject_device_write_test`)
	for name, write := range deviceUserWrites(env.store, ctx, owner.User.ID, owner.BrandCloud.ID, d.ID) {
		if err := write(); err == nil {
			t.Fatalf("%s survived audit failure", name)
		}
	}
	got, err := env.store.GetDevice(ctx, owner.BrandCloud.ID, d.ID)
	if err != nil || got.Name != "Original" || got.Status != d.Status || got.DisabledAt != nil {
		t.Fatalf("audit failure did not roll back device: %+v %v", got, err)
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM devices WHERE organization_id=$1`, owner.BrandCloud.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit failure left new device: %d %v", count, err)
	}
}

func TestUserDeviceMutationsPreserveLegacyPlatformAndFailureSemantics(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "device-legacy-owner")
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	legacy, err := env.store.CreateOrganization(ctx, owner.User.ID, "Legacy customer")
	if err != nil {
		t.Fatal(err)
	}
	d, err := env.store.CreateDeviceAsUser(ctx, owner.User.ID, legacy.ID, DeviceInput{Name: "Legacy", Category: model.DeviceCategoryIPCamera})
	if err != nil {
		t.Fatal(err)
	}
	writes := deviceUserWrites(env.store, ctx, owner.User.ID, legacy.ID, d.ID)
	for _, name := range []string{"update", "status", "delete", "delete"} {
		if err := writes[name](); err != nil {
			t.Fatalf("legacy %s failed: %v", name, err)
		}
	}
	for _, name := range []string{"update", "status"} {
		if err := writes[name](); !errors.Is(err, ErrDisabled) {
			t.Fatalf("disabled device %s: %v", name, err)
		}
	}
	for name, write := range deviceUserWrites(env.store, ctx, "", legacy.ID, d.ID) {
		if err := write(); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing actor %s: %v", name, err)
		}
	}
	if _, err := env.store.CreateDeviceAsUser(ctx, owner.User.ID, owner.User.ID, DeviceInput{Name: "Missing", Category: model.DeviceCategoryIPCamera}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing cloud: %v", err)
	}
	if err := env.store.DeleteDeviceAsUser(ctx, owner.User.ID, owner.BrandCloud.ID, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-cloud device: %v", err)
	}
	if _, err := env.store.CreateDeviceAsUser(ctx, owner.User.ID, legacy.ID, DeviceInput{Name: "Invalid", Category: model.DeviceCategoryIPCamera, Metadata: map[string]any{"invalid": make(chan int)}}); err == nil {
		t.Fatal("invalid metadata accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for name, write := range deviceUserWrites(env.store, canceled, owner.User.ID, legacy.ID, d.ID) {
		if err := write(); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled %s: %v", name, err)
		}
	}
}
