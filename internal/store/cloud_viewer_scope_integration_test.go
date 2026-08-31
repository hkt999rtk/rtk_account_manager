package store

import (
	"context"
	"errors"
	"rtk_account_manager/internal/model"
	"testing"
	"time"
)

func TestMultiCloudViewerScopeLifecycleIntegration(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	user := func(email string) DeveloperSignupResult {
		u, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: email, PasswordHash: "hash"})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	owner, viewer := user("viewer-owner@test.invalid"), user("scope-viewer@test.invalid")
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
	p1, p2 := product("viewer-one"), product("viewer-two")
	in := BrandCloudMemberInvitationInput{BrandCloudID: owner.BrandCloud.ID, InvitedByUserID: owner.User.ID, TargetEmail: viewer.User.Email, Role: model.RoleViewer, AccessScope: &model.CloudViewerScope{Kind: "selected_products", ProductIDs: []string{p2.ID, p1.ID}}, TokenHash: "viewer-first", ExpiresAt: now.Add(time.Hour)}
	invite, created, err := env.store.CreateBrandCloudMemberInvitation(ctx, in, now)
	if err != nil || !created {
		t.Fatalf("invite %v %v", created, err)
	}
	in.AccessScope = &model.CloudViewerScope{Kind: "selected_products", ProductIDs: []string{p1.ID, p2.ID}}
	duplicate, created, err := env.store.CreateBrandCloudMemberInvitation(ctx, in, now)
	if err != nil || created || duplicate.ID != invite.ID {
		t.Fatalf("normalized replay %v %v", created, err)
	}
	in.AccessScope = &model.CloudViewerScope{Kind: "all_products"}
	if _, _, err := env.store.CreateBrandCloudMemberInvitation(ctx, in, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed pending scope: %v", err)
	}
	resent, err := env.store.ResendBrandCloudMemberInvitation(ctx, BrandCloudMemberInvitationMutation{BrandCloudID: owner.BrandCloud.ID, ActorUserID: owner.User.ID, InvitationID: invite.ID, TokenHash: "viewer-rotated", ExpiresAt: now.Add(time.Hour)}, now)
	if err != nil || !sameCloudScope(resent.AccessScope, invite.AccessScope) {
		t.Fatalf("resend changed scope: %+v %v", resent, err)
	}
	if _, _, err := env.store.AcceptBrandCloudMemberInvitation(ctx, viewer.User.ID, "viewer-first", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old token accepted: %v", err)
	}
	_, member, err := env.store.AcceptBrandCloudMemberInvitation(ctx, viewer.User.ID, "viewer-rotated", now)
	if err != nil || member.Role != model.RoleViewer || !sameCloudScope(member.AccessScope, invite.AccessScope) {
		t.Fatalf("accept scope: %+v %v", member, err)
	}
	list := func(want int) {
		t.Helper()
		page, err := env.store.ListDeviceItemProfiles(ctx, DeviceItemProfileListFilter{BrandCloudID: owner.BrandCloud.ID, UserID: viewer.User.ID, Limit: 20})
		if err != nil || page.Page.Total != want || len(page.Profiles) != want {
			t.Fatalf("Product list %+v want=%d %v", page, want, err)
		}
	}
	list(2)
	p3 := product("future-product")
	list(2)
	patch := func(role model.Role, scope *model.CloudViewerScope) {
		t.Helper()
		_, err := env.store.UpdateDeveloperBrandCloudMember(ctx, CloudMemberUpdateInput{BrandCloudID: owner.BrandCloud.ID, ActorUserID: owner.User.ID, TargetUserID: viewer.User.ID, Role: role, AccessScope: scope})
		if err != nil {
			t.Fatal(err)
		}
	}
	patch("", &model.CloudViewerScope{Kind: "all_products"})
	list(3)
	_ = product("next-future-product")
	list(4)
	// Even a stale custom/high-privilege organization ACL cannot lift viewer's ceiling.
	if _, err := env.db.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type,scope_id,organization_id) SELECT id,'user',$1,'organization',$2::text,$2::text::uuid FROM roles WHERE name IN('admin','owner')`, viewer.User.ID, owner.BrandCloud.ID); err != nil {
		t.Fatal(err)
	}
	for _, permission := range []string{"registry_device.manage", "billing_account.read", "billing_summary.read", "payment_method.manage", "product.create", "product_owner.transfer"} {
		allowed, err := env.store.HasUserPermissionForResource(ctx, viewer.User.ID, owner.BrandCloud.ID, permission, ScopeTypeProduct, p1.ID)
		if err != nil || allowed {
			t.Fatalf("viewer acquired %s: %v %v", permission, allowed, err)
		}
	}
	if role, err := env.store.GetUserProductCollaboratorRole(ctx, viewer.User.ID, owner.BrandCloud.ID, p1.ID); err != nil || role != ProductViewerRole {
		t.Fatalf("stale owner role leaked into Product projection: %s %v", role, err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, viewer.User.ID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := env.store.HasPermission(ctx, viewer.User.ID, owner.BrandCloud.ID, "acl.manage"); err != nil || allowed {
		t.Fatalf("platform shortcut lifted viewer ceiling: %v %v", allowed, err)
	}
	device, err := env.store.CreateDevice(ctx, owner.BrandCloud.ID, DeviceInput{Name: "viewer-device", Category: model.DeviceCategoryIPCamera, Metadata: map[string]any{model.DeviceMetadataVideoCloudDevid: "viewer-no-playback"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE devices SET device_item_profile_id=$2 WHERE id=$1`, device.ID, p3.ID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := env.store.HasUserDevicePermission(ctx, viewer.User.ID, owner.BrandCloud.ID, "registry_device.read", device.ID); err != nil || !allowed {
		t.Fatalf("viewer read: %v %v", allowed, err)
	}
	if err := env.store.AuthorizeUserForVideoDevice(ctx, viewer.User.ID, "viewer-no-playback"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("viewer playback: %v", err)
	}
	patch("", &model.CloudViewerScope{Kind: "selected_products", ProductIDs: []string{p1.ID}})
	list(1)
	if allowed, err := env.store.HasUserDevicePermission(ctx, viewer.User.ID, owner.BrandCloud.ID, "registry_device.read", device.ID); err != nil || allowed {
		t.Fatalf("removed Product device still visible: %v %v", allowed, err)
	}
	patch(model.RoleAdmin, nil)
	list(0)
	patch(model.RoleViewer, &model.CloudViewerScope{Kind: "all_products"})
	list(4)
	if err := env.store.RemoveMember(ctx, owner.BrandCloud.ID, viewer.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.AddMember(ctx, owner.BrandCloud.ID, viewer.User.Email, model.RoleMember); err != nil {
		t.Fatal(err)
	}
	list(0)
}
