package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func admitProductCollaboratorFixture(t *testing.T, env storeIntegrationEnv, owner, target DeveloperSignupResult, now time.Time) {
	t.Helper()
	ctx := context.Background()
	token := "admit-" + owner.BrandCloud.ID + "-" + target.User.ID
	if _, _, err := env.store.CreateBrandCloudMemberInvitation(ctx, BrandCloudMemberInvitationInput{BrandCloudID: owner.BrandCloud.ID, InvitedByUserID: owner.User.ID, TargetEmail: target.User.Email, Role: model.RoleMember, TokenHash: token, ExpiresAt: now.Add(time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.store.AcceptBrandCloudMemberInvitation(ctx, target.User.ID, token, now); err != nil {
		t.Fatal(err)
	}
}

func TestMultiCloudProductMutationAuditFailureRollsBackIntegration(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "audit-owner@example.com", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "audit-target@example.com", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true,signup_pending_verification=false`); err != nil {
		t.Fatal(err)
	}
	admitProductCollaboratorFixture(t, env, owner, target, now)
	product, err := env.store.CreateDeviceItemProfile(ctx, DeviceItemProfileCreateInput{ActorUserID: &owner.User.ID, BrandCloudID: owner.BrandCloud.ID, ProfileKey: "audit", DisplayName: "Audit", Category: model.DeviceCategoryIPCamera, CAProfile: "ca", IssuerProfile: "issuer", ServiceOptions: []string{"video_streaming"}})
	if err != nil {
		t.Fatal(err)
	}
	in := ProductCollaboratorInvitationInput{BrandCloudID: owner.BrandCloud.ID, ProductID: product.ID, InvitedByUserID: owner.User.ID, TargetEmail: target.User.Email, Role: ProductEditorRole, TokenHash: "audit-invite", ExpiresAt: now.Add(time.Hour)}
	if _, _, err := env.store.CreateProductCollaboratorInvitation(ctx, in, now); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.AcceptProductCollaboratorInvitation(ctx, target.User.ID, in.TokenHash, now); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `CREATE FUNCTION test_reject_product_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected audit failure'; END $$;
	    CREATE TRIGGER test_reject_product_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION test_reject_product_audit()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := env.db.Exec(context.Background(), `DROP TRIGGER test_reject_product_audit ON audit_events; DROP FUNCTION test_reject_product_audit()`); err != nil {
			t.Error(err)
		}
	})
	var version int64
	if err := env.db.QueryRow(ctx, `SELECT authorization_version FROM organizations WHERE id=$1`, owner.BrandCloud.ID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		mutate func() error
	}{
		{"role", func() error {
			_, err := env.store.UpdateProductCollaborator(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, target.User.ID, ProductViewerRole)
			return err
		}},
		{"remove", func() error {
			return env.store.RemoveProductCollaborator(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, target.User.ID)
		}},
		{"transfer", func() error {
			return env.store.TransferProductOwnership(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, target.User.ID)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.mutate(); err == nil {
				t.Fatal("mutation committed despite failed audit")
			}
			role, err := env.store.GetUserProductCollaboratorRole(ctx, target.User.ID, owner.BrandCloud.ID, product.ID)
			if err != nil || role != ProductEditorRole {
				t.Fatalf("role/admission changed after rollback: %s %v", role, err)
			}
			var current int64
			if err := env.db.QueryRow(ctx, `SELECT authorization_version FROM organizations WHERE id=$1`, owner.BrandCloud.ID).Scan(&current); err != nil || current != version {
				t.Fatalf("version changed after rollback: %d want %d %v", current, version, err)
			}
		})
	}
}

func TestMultiCloudProductInvitationRequiresPriorCloudAdmissionIntegration(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "admission-owner@example.com", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "admission-target@example.com", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true,signup_pending_verification=false`); err != nil {
		t.Fatal(err)
	}
	product := func(key string) model.DeviceItemProfile {
		p, err := env.store.CreateDeviceItemProfile(ctx, DeviceItemProfileCreateInput{ActorUserID: &owner.User.ID, BrandCloudID: owner.BrandCloud.ID, ProfileKey: key, DisplayName: key, Category: model.DeviceCategoryIPCamera, CAProfile: "ca", IssuerProfile: "issuer", ServiceOptions: []string{"video_streaming"}})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	allowedProduct, hiddenProduct := product("admitted-product"), product("hidden-product")
	input := ProductCollaboratorInvitationInput{BrandCloudID: owner.BrandCloud.ID, ProductID: allowedProduct.ID, InvitedByUserID: owner.User.ID, TargetEmail: target.User.Email, Role: ProductEditorRole, TokenHash: "product-approved-token", ExpiresAt: now.Add(time.Hour)}
	if _, _, err := env.store.CreateProductCollaboratorInvitation(ctx, input, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Product invite admitted external cloud user: %v", err)
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM organization_members WHERE organization_id=$1 AND user_id=$2`, owner.BrandCloud.ID, target.User.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("Product invite created membership: %d %v", count, err)
	}
	// The owner first invites the existing global developer to the cloud. Only
	// that accepted membership permits a subsequent Product scope invitation.
	_, _, err = env.store.CreateBrandCloudMemberInvitation(ctx, BrandCloudMemberInvitationInput{BrandCloudID: owner.BrandCloud.ID, InvitedByUserID: owner.User.ID, TargetEmail: target.User.Email, Role: model.RoleMember, TokenHash: "cloud-admission", ExpiresAt: now.Add(time.Hour)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.store.AcceptBrandCloudMemberInvitation(ctx, target.User.ID, "cloud-admission", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.store.CreateProductCollaboratorInvitation(ctx, input, now); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.AcceptProductCollaboratorInvitation(ctx, target.User.ID, input.TokenHash, now); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		product string
		want    bool
	}{{allowedProduct.ID, true}, {hiddenProduct.ID, false}} {
		got, err := env.store.HasUserPermissionForResource(ctx, target.User.ID, owner.BrandCloud.ID, "registry_device.manage", ScopeTypeProduct, c.product)
		if err != nil || got != c.want {
			t.Fatalf("Product %s permission=%v want=%v err=%v", c.product, got, c.want, err)
		}
	}
	page, err := env.store.ListDeviceItemProfiles(ctx, DeviceItemProfileListFilter{BrandCloudID: owner.BrandCloud.ID, UserID: target.User.ID, Limit: 20})
	if err != nil || page.Page.Total != 1 || len(page.Profiles) != 1 || page.Profiles[0].ID != allowedProduct.ID {
		t.Fatalf("Product list scope=%+v err=%v", page, err)
	}
	device := func(p model.DeviceItemProfile) model.Device {
		d, err := env.store.CreateDevice(ctx, owner.BrandCloud.ID, DeviceInput{Name: p.ProfileKey, Category: model.DeviceCategoryIPCamera})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(ctx, `UPDATE devices SET device_item_profile_id=$2 WHERE id=$1`, d.ID, p.ID); err != nil {
			t.Fatal(err)
		}
		return d
	}
	visible, hidden := device(allowedProduct), device(hiddenProduct)
	for _, c := range []struct {
		id   string
		want bool
	}{{visible.ID, true}, {hidden.ID, false}} {
		got, err := env.store.HasUserDevicePermission(ctx, target.User.ID, owner.BrandCloud.ID, "registry_device.read", c.id)
		if err != nil || got != c.want {
			t.Fatalf("device permission=%v want=%v err=%v", got, c.want, err)
		}
	}
	devices, err := env.store.ListDevicesFiltered(ctx, DeviceListFilter{OrganizationID: owner.BrandCloud.ID, UserID: target.User.ID, Limit: 20})
	if err != nil || devices.Page.Total != 1 || len(devices.Devices) != 1 || devices.Devices[0].ID != visible.ID {
		t.Fatalf("device list=%+v err=%v", devices, err)
	}
	summary, err := env.store.FleetSummaryForUser(ctx, owner.BrandCloud.ID, target.User.ID)
	if err != nil || summary.Total != 1 {
		t.Fatalf("fleet totals leaked hidden Product: %+v %v", summary, err)
	}
	// Revocation cancels pending Product invitations and invalidates every old
	// Product role, even if a new cloud membership is subsequently inserted.
	input.ProductID = hiddenProduct.ID
	input.TokenHash = "pending-before-revocation"
	if _, _, err := env.store.CreateProductCollaboratorInvitation(ctx, input, now); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RemoveMember(ctx, owner.BrandCloud.ID, target.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.AddMember(ctx, owner.BrandCloud.ID, target.User.Email, model.RoleMember); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.AcceptProductCollaboratorInvitation(ctx, target.User.ID, input.TokenHash, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked pending grant revived: %v", err)
	}
	if got, err := env.store.HasUserDevicePermission(ctx, target.User.ID, owner.BrandCloud.ID, "registry_device.read", visible.ID); err != nil || got {
		t.Fatalf("rejoining revived Product permission: %v %v", got, err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM brand_cloud_product_admissions WHERE organization_id=$1 AND user_id=$2`, owner.BrandCloud.ID, target.User.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejoining revived scope: %d %v", count, err)
	}
}

func TestMultiCloudDelegatedProductOwnerCannotExpandAdmissionIntegration(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	user := func(email string) DeveloperSignupResult {
		u, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: email, PasswordHash: "hash"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true WHERE id=$1`, u.User.ID); err != nil {
			t.Fatal(err)
		}
		return u
	}
	owner, delegate, target := user("scope-owner@example.com"), user("scope-delegate@example.com"), user("scope-target@example.com")
	admitProductCollaboratorFixture(t, env, owner, delegate, now)
	admitProductCollaboratorFixture(t, env, owner, target, now)
	product, err := env.store.CreateDeviceItemProfile(ctx, DeviceItemProfileCreateInput{ActorUserID: &owner.User.ID, BrandCloudID: owner.BrandCloud.ID, ProfileKey: "delegated", DisplayName: "delegated", Category: model.DeviceCategoryIPCamera, CAProfile: "ca", IssuerProfile: "issuer", ServiceOptions: []string{"video_streaming"}})
	if err != nil {
		t.Fatal(err)
	}
	invite := func(actor, target DeveloperSignupResult, token string) error {
		_, _, err := env.store.CreateProductCollaboratorInvitation(ctx, ProductCollaboratorInvitationInput{BrandCloudID: owner.BrandCloud.ID, ProductID: product.ID, InvitedByUserID: actor.User.ID, TargetEmail: target.User.Email, Role: ProductEditorRole, TokenHash: token, ExpiresAt: now.Add(time.Hour)}, now)
		return err
	}
	if err := invite(owner, delegate, "delegate-invite"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.AcceptProductCollaboratorInvitation(ctx, delegate.User.ID, "delegate-invite", now); err != nil {
		t.Fatal(err)
	}
	if err := env.store.TransferProductOwnership(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, delegate.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := invite(delegate, target, "unapproved-invite"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delegate expanded owner scope: %v", err)
	}
	if err := invite(owner, target, "owner-approved-invite"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.AcceptProductCollaboratorInvitation(ctx, target.User.ID, "owner-approved-invite", now); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RemoveProductCollaborator(ctx, delegate.User.ID, owner.BrandCloud.ID, product.ID, target.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := invite(delegate, target, "within-approved-scope"); err != nil {
		t.Fatalf("delegate cannot use approved scope: %v", err)
	}
	// The cloud owner can revoke scope even between a role removal and a new
	// invitation's acceptance. That pending invitation must no longer be usable.
	if err := env.store.RemoveProductCollaborator(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, target.User.ID); err != nil {
		t.Fatalf("owner cannot withdraw pending admission: %v", err)
	}
	if _, err := env.store.AcceptProductCollaboratorInvitation(ctx, target.User.ID, "within-approved-scope", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked Product invitation accepted: %v", err)
	}
	if err := invite(delegate, target, "after-withdrawal"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delegate restored withdrawn scope: %v", err)
	}
	if err := env.store.RemoveProductCollaborator(ctx, owner.User.ID, owner.BrandCloud.ID, product.ID, delegate.User.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("removed Product owner without transfer: %v", err)
	}
}

func TestMultiCloudProductAcceptSerializesWithMembershipRevocationIntegration(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := time.Now().UTC()
	owner, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "race-owner@example.com", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "race-target@example.com", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true`); err != nil {
		t.Fatal(err)
	}
	admitProductCollaboratorFixture(t, env, owner, target, now)
	product, err := env.store.CreateDeviceItemProfile(ctx, DeviceItemProfileCreateInput{ActorUserID: &owner.User.ID, BrandCloudID: owner.BrandCloud.ID, ProfileKey: "race", DisplayName: "race", Category: model.DeviceCategoryIPCamera, CAProfile: "ca", IssuerProfile: "issuer", ServiceOptions: []string{"video_streaming"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.store.CreateProductCollaboratorInvitation(ctx, ProductCollaboratorInvitationInput{BrandCloudID: owner.BrandCloud.ID, ProductID: product.ID, InvitedByUserID: owner.User.ID, TargetEmail: target.User.Email, Role: ProductEditorRole, TokenHash: "revocation-race", ExpiresAt: now.Add(time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	tx, err := env.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `DELETE FROM organization_members WHERE organization_id=$1 AND user_id=$2`, owner.BrandCloud.ID, target.User.ID); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan error, 1)
	go func() {
		_, err := env.store.AcceptProductCollaboratorInvitation(ctx, target.User.ID, "revocation-race", now)
		accepted <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var blocked bool
		if err := env.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT id::text FROM organizations WHERE id::text=%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-accepted:
			t.Fatalf("accept did not serialize: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("accept never waited on cloud revocation lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-accepted; !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked invitation won race: %v", err)
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM brand_cloud_product_admissions WHERE organization_id=$1 AND user_id=$2`, owner.BrandCloud.ID, target.User.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("race created admission: %d %v", count, err)
	}
}
