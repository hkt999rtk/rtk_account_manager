package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"rtk_account_manager/internal/model"
)

func TestProductDeviceDisplayPreservesIdentityAndRequiresCurrentScope(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "display-owner")
	other := handoffDeveloper(t, env, "display-other")
	product, err := env.store.CreateDeviceItemProfile(ctx, DeviceItemProfileCreateInput{ActorUserID: &owner.User.ID, BrandCloudID: owner.BrandCloud.ID, ProfileKey: "display", DisplayName: "Display", Category: model.DeviceCategoryIPCamera, CAProfile: "ca", IssuerProfile: "issuer", ServiceOptions: []string{"mqtt"}})
	if err != nil {
		t.Fatal(err)
	}
	metadata := map[string]any{"video_cloud_devid": "preserved-video", "clip_public_key": "preserved-key", "region": "lab"}
	d, err := env.store.CreateDevice(ctx, owner.BrandCloud.ID, DeviceInput{Name: "Device", Category: model.DeviceCategoryIPCamera, SerialNumber: stringPtr("serial-1"), MACAddress: stringPtr("00:11:22:33:44:55"), Manufacturer: stringPtr("maker"), Model: stringPtr("model-1"), Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.db.Exec(ctx, `UPDATE devices SET device_item_profile_id=$2 WHERE id=$1`, d.ID, product.ID); err != nil {
		t.Fatal(err)
	}
	patch := func(actor, cloud, p string, in DeviceDisplayPatch) (model.Device, error) {
		return env.store.PatchProductDeviceDisplay(ctx, actor, cloud, p, d.ID, in)
	}
	for _, tc := range []struct{ actor, cloud, product string }{{other.User.ID, owner.BrandCloud.ID, product.ID}, {owner.User.ID, other.BrandCloud.ID, product.ID}, {owner.User.ID, owner.BrandCloud.ID, other.BrandCloud.ID}} {
		if _, err = patch(tc.actor, tc.cloud, tc.product, DeviceDisplayPatch{Name: stringPtr("forbidden")}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("scope bypass: %v", err)
		}
	}
	// Independent partial edits serialize without either replacing the other's
	// column, immutable hardware identity or operational metadata.
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for _, in := range []DeviceDisplayPatch{{Name: stringPtr("New name")}, {Model: stringPtr("New model")}} {
		wg.Add(1)
		go func(in DeviceDisplayPatch) {
			defer wg.Done()
			_, e := patch(owner.User.ID, owner.BrandCloud.ID, product.ID, in)
			failures <- e
		}(in)
	}
	wg.Wait()
	close(failures)
	for e := range failures {
		if e != nil {
			t.Fatal(e)
		}
	}
	got, err := env.store.GetDevice(ctx, owner.BrandCloud.ID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "New name" || got.Model == nil || *got.Model != "New model" || !reflect.DeepEqual(got.SerialNumber, d.SerialNumber) || !reflect.DeepEqual(got.MACAddress, d.MACAddress) || !reflect.DeepEqual(got.Manufacturer, d.Manufacturer) || !reflect.DeepEqual(got.Metadata, metadata) || got.DeviceItemProfileID == nil || *got.DeviceItemProfileID != product.ID {
		t.Fatalf("display edit altered identity or lost concurrent update: %+v", got)
	}
	if got, err = patch(owner.User.ID, owner.BrandCloud.ID, product.ID, DeviceDisplayPatch{Model: stringPtr("")}); err != nil || got.Model == nil || *got.Model != "" {
		t.Fatalf("explicit model clear: %+v %v", got, err)
	}
	var count int
	if err = env.db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE event_type='device.display.updated' AND subject_id=$1 AND actor_user_id=$2`, d.ID, owner.User.ID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("audit count %d: %v", count, err)
	}
	// Even a stale organization-wide admin ACL cannot override a viewer ceiling.
	if _, err = env.db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role,access_scope) VALUES($1,$2,'viewer','{"kind":"all_products"}');`, owner.BrandCloud.ID, other.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = env.db.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type,scope_id,organization_id) SELECT id,'user',$1,'organization',$2::text,$2::text::uuid FROM roles WHERE name='admin'`, other.User.ID, owner.BrandCloud.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = patch(other.User.ID, owner.BrandCloud.ID, product.ID, DeviceDisplayPatch{Name: stringPtr("viewer")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("viewer bypass: %v", err)
	}
	if _, err = env.db.Exec(ctx, `UPDATE users SET signup_pending_verification=true WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = patch(owner.User.ID, owner.BrandCloud.ID, product.ID, DeviceDisplayPatch{Name: stringPtr("held")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("activation hold bypass: %v", err)
	}
}
