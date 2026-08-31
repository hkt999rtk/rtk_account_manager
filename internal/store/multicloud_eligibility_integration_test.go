package store

import (
	"context"
	"errors"
	"testing"

	"rtk_account_manager/internal/model"
)

func TestMultiCloudEligibilityFencesStaleACLIntegration(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "cloud-owner@test.invalid", PasswordHash: "hash", SignupPendingVerification: true})
	if err != nil {
		t.Fatal(err)
	}
	member, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "cloud-member@test.invalid", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	cloud := owner.BrandCloud.ID
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true,signup_pending_verification=false WHERE id=$1`, member.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'admin')`, cloud, member.User.ID); err != nil {
		t.Fatal(err)
	}
	// Explicit legacy ACL: membership itself does not grant admin Product access.
	if _, err := env.db.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type,scope_id,organization_id)
		SELECT id,'user',$1,'organization',$2::text,$2::text::uuid FROM roles WHERE name='admin'`, member.User.ID, cloud); err != nil {
		t.Fatal(err)
	}
	device, err := env.store.CreateDevice(ctx, cloud, DeviceInput{Name: "test", Category: model.DeviceCategoryIPCamera, Metadata: map[string]any{model.DeviceMetadataVideoCloudDevid: "isolated-video-device"}})
	if err != nil {
		t.Fatal(err)
	}
	// This fixture has an existing explicit Product approval. Operational
	// eligibility must still deny it while its cloud owner is pending/disabled.
	var product string
	if err := env.db.QueryRow(ctx, `INSERT INTO device_item_profiles(brand_cloud_id,profile_key,display_name,category,ca_profile,issuer_profile)
	    VALUES ($1,'eligibility','Eligibility','ip_camera','ca','issuer') RETURNING id::text`, cloud).Scan(&product); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE devices SET device_item_profile_id=$2 WHERE id=$1`, device.ID, product); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO brand_cloud_product_admissions(organization_id,user_id,product_id,provenance,approved_by) VALUES ($1,$2,$3,'owner_invitation',$4)`, cloud, member.User.ID, product, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, want bool) {
		t.Helper()
		checks := []struct {
			name string
			call func() (bool, error)
		}{
			{"organization", func() (bool, error) {
				return env.store.HasPermission(ctx, member.User.ID, cloud, "registry_device.read")
			}},
			{"resource", func() (bool, error) {
				return env.store.HasUserPermissionForResource(ctx, member.User.ID, cloud, "registry_device.read", "device", device.ID)
			}},
			{"any resource", func() (bool, error) {
				return env.store.HasUserPermissionAnyResource(ctx, member.User.ID, cloud, "registry_device.read")
			}},
			{"device", func() (bool, error) {
				return env.store.HasUserDevicePermission(ctx, member.User.ID, cloud, "registry_device.read", device.ID)
			}},
		}
		for _, c := range checks {
			got, err := c.call()
			if err != nil || got != want {
				t.Fatalf("%s allowed=%v want=%v err=%v", c.name, got, want, err)
			}
		}
		permissions, err := env.store.ListUserOrganizationPermissions(ctx, member.User.ID, cloud)
		if err != nil || (len(permissions) > 0) != want {
			t.Fatalf("permission projection=%v want=%v err=%v", permissions, want, err)
		}
		_, err = env.store.GetDeveloperBrandCloudMember(ctx, cloud, member.User.ID)
		if want && err != nil || !want && !errors.Is(err, ErrNotFound) {
			t.Fatalf("membership eligibility err=%v want=%v", err, want)
		}
		_, err = env.store.GetOrganization(ctx, cloud, member.User.ID)
		if want && err != nil || !want && !errors.Is(err, ErrNotFound) {
			t.Fatalf("cloud read err=%v want=%v", err, want)
		}
		err = env.store.AuthorizeUserForVideoDevice(ctx, member.User.ID, "isolated-video-device")
		if want && err != nil || !want && !errors.Is(err, ErrNotFound) {
			t.Fatalf("video authorization err=%v want=%v", err, want)
		}
	}
	check(t, false) // Acting member verified, but designated owner still pending.
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true,signup_pending_verification=false WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	check(t, true)
	for _, test := range []struct {
		name, disable, restore string
		id                     string
	}{
		{"owner disabled", `UPDATE users SET disabled_at=now() WHERE id=$1`, `UPDATE users SET disabled_at=NULL WHERE id=$1`, owner.User.ID},
		{"owner unverified", `UPDATE users SET email_verified=false WHERE id=$1`, `UPDATE users SET email_verified=true WHERE id=$1`, owner.User.ID},
		{"owner pending", `UPDATE users SET signup_pending_verification=true WHERE id=$1`, `UPDATE users SET signup_pending_verification=false WHERE id=$1`, owner.User.ID},
		{"member disabled", `UPDATE organization_members SET disabled_at=now() WHERE user_id=$1`, `UPDATE organization_members SET disabled_at=NULL WHERE user_id=$1`, member.User.ID},
		{"owner membership disabled", `UPDATE organization_members SET disabled_at=now() WHERE user_id=$1`, `UPDATE organization_members SET disabled_at=NULL WHERE user_id=$1`, owner.User.ID},
		{"cloud disabled", `UPDATE organizations SET status='disabled' WHERE id=$1`, `UPDATE organizations SET status='active' WHERE id=$1`, cloud},
		{"cloud deleted", `UPDATE organizations SET deleted_at=now() WHERE id=$1`, `UPDATE organizations SET deleted_at=NULL WHERE id=$1`, cloud},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := env.db.Exec(ctx, test.disable, test.id); err != nil {
				t.Fatal(err)
			}
			if _, err := env.db.Exec(ctx, `UPDATE role_assignments SET disabled_at=NULL WHERE actor_id=$1 AND organization_id=$2`, member.User.ID, cloud); err != nil {
				t.Fatal(err)
			}
			check(t, false)
			if _, err := env.db.Exec(ctx, test.restore, test.id); err != nil {
				t.Fatal(err)
			}
			if _, err := env.db.Exec(ctx, `UPDATE role_assignments SET disabled_at=NULL WHERE actor_id=$1 AND organization_id=$2`, member.User.ID, cloud); err != nil {
				t.Fatal(err)
			}
			check(t, true)
		})
	}
	// Simulate a stale resource assignment surviving membership revocation.
	if _, err := env.db.Exec(ctx, `DELETE FROM organization_members WHERE organization_id=$1 AND user_id=$2`, cloud, member.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE role_assignments SET disabled_at=NULL WHERE actor_id=$1 AND organization_id=$2`, member.User.ID, cloud); err != nil {
		t.Fatal(err)
	}
	check(t, false)
}
