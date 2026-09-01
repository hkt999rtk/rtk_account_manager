package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func TestValidProductCollaboratorRole(t *testing.T) {
	for _, role := range []string{ProductEditorRole, ProductViewerRole} {
		if !validProductCollaboratorRole(role) {
			t.Fatalf("validProductCollaboratorRole(%q) = false", role)
		}
	}
	for _, role := range []string{"", ProductOwnerRole, "admin", "member"} {
		if validProductCollaboratorRole(role) {
			t.Fatalf("validProductCollaboratorRole(%q) = true", role)
		}
	}
}

func TestProductCollaborationInvitationVisibilityAndOwnershipTransferIntegration(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "product-owner@example.com", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "product-editor@example.com", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true,email_verified_at=$3 WHERE id IN ($1,$2)`, target.User.ID, owner.User.ID, now); err != nil {
		t.Fatal(err)
	}
	admitProductCollaboratorFixture(t, env, owner, target, now)
	createProduct := func(key string) model.DeviceItemProfile {
		profile, err := env.store.CreateDeviceItemProfile(ctx, DeviceItemProfileCreateInput{
			ActorUserID: &owner.User.ID, BrandCloudID: owner.BrandCloud.ID,
			ProfileKey: key, DisplayName: key, Category: model.DeviceCategoryIPCamera,
			CAProfile: "brand-ca", IssuerProfile: "issuer", ServiceOptions: []string{"video_streaming"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return profile
	}
	assignedProduct := createProduct("assigned-product")
	_ = createProduct("hidden-product")
	brandUser, err := env.store.CreateBrandCloudUser(ctx, owner.User.ID, owner.BrandCloud.ID, BrandCloudUserInput{
		Email: "brand-member@example.com", PasswordHash: "hash", Role: model.RoleMember,
	})
	if err != nil {
		t.Fatal(err)
	}
	if role, err := env.store.GetProductCollaboratorRole(ctx, brandUser.BrandCloudUser.ID, owner.BrandCloud.ID, assignedProduct.ID); err != nil || role != string(model.RoleMember) {
		t.Fatalf("brand member Product role = %q, err=%v", role, err)
	}
	if _, err := env.store.GetProductCollaboratorRole(ctx, "00000000-0000-0000-0000-000000000000", owner.BrandCloud.ID, assignedProduct.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing brand member role error = %v", err)
	}
	if _, _, err := env.store.CreateProductCollaboratorInvitation(ctx, ProductCollaboratorInvitationInput{
		BrandCloudID: owner.BrandCloud.ID, ProductID: assignedProduct.ID, InvitedByUserID: owner.User.ID,
		TargetEmail: target.User.Email, Role: ProductOwnerRole, TokenHash: "invalid-role", ExpiresAt: now.Add(time.Hour),
	}, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("invalid collaborator role error = %v", err)
	}
	if _, _, err := env.store.CreateProductCollaboratorInvitation(ctx, ProductCollaboratorInvitationInput{
		BrandCloudID: owner.BrandCloud.ID, ProductID: assignedProduct.ID, InvitedByUserID: owner.User.ID,
		TargetEmail: "missing@example.com", Role: ProductEditorRole, TokenHash: "missing-user", ExpiresAt: now.Add(time.Hour),
	}, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing collaborator error = %v", err)
	}

	invitation, created, err := env.store.CreateProductCollaboratorInvitation(ctx, ProductCollaboratorInvitationInput{
		BrandCloudID: owner.BrandCloud.ID, ProductID: assignedProduct.ID, InvitedByUserID: owner.User.ID,
		TargetEmail: target.User.Email, Role: ProductEditorRole, TokenHash: "product-invite-token", ExpiresAt: now.Add(time.Hour),
	}, now)
	if err != nil || !created || invitation.Role != ProductEditorRole {
		t.Fatalf("create invitation: created=%v invitation=%+v err=%v", created, invitation, err)
	}
	if _, err := env.store.AcceptProductCollaboratorInvitation(ctx, target.User.ID, "product-invite-token", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.store.CreateProductCollaboratorInvitation(ctx, ProductCollaboratorInvitationInput{
		BrandCloudID: owner.BrandCloud.ID, ProductID: assignedProduct.ID, InvitedByUserID: owner.User.ID,
		TargetEmail: target.User.Email, Role: ProductEditorRole, TokenHash: "already-assigned", ExpiresAt: now.Add(time.Hour),
	}, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("already assigned invitation error = %v", err)
	}
	if role, err := env.store.GetUserProductCollaboratorRole(ctx, target.User.ID, owner.BrandCloud.ID, assignedProduct.ID); err != nil || role != ProductEditorRole {
		t.Fatalf("collaborator role = %q, err=%v", role, err)
	}
	if _, err := env.store.GetUserProductCollaboratorRole(ctx, "00000000-0000-0000-0000-000000000000", owner.BrandCloud.ID, assignedProduct.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing collaborator role error = %v", err)
	}
	if collaborators, err := env.store.ListProductCollaborators(ctx, owner.BrandCloud.ID, assignedProduct.ID); err != nil || len(collaborators) != 2 {
		t.Fatalf("collaborators = %+v, err=%v", collaborators, err)
	}
	if allowed, err := env.store.HasUserPermissionForResource(ctx, target.User.ID, owner.BrandCloud.ID, "registry_device.manage", ScopeTypeProduct, assignedProduct.ID); err != nil || !allowed {
		t.Fatalf("Product editor permission: allowed=%v err=%v", allowed, err)
	}
	if invitations, err := env.store.ListProductCollaboratorInvitations(ctx, owner.BrandCloud.ID, assignedProduct.ID, now); err != nil || len(invitations) != 1 {
		t.Fatalf("invitations = %+v, err=%v", invitations, err)
	}
	page, err := env.store.ListDeviceItemProfiles(ctx, DeviceItemProfileListFilter{BrandCloudID: owner.BrandCloud.ID, UserID: target.User.ID, Limit: 20})
	if err != nil || page.Page.Total != 1 || len(page.Profiles) != 1 || page.Profiles[0].ID != assignedProduct.ID {
		t.Fatalf("collaborator Product visibility = %+v, err=%v", page, err)
	}
	if _, err := env.store.UpdateProductCollaborator(ctx, owner.User.ID, owner.BrandCloud.ID, assignedProduct.ID, target.User.ID, ProductViewerRole); err != nil {
		t.Fatalf("change collaborator to viewer: %v", err)
	}
	if _, err := env.store.UpdateProductCollaborator(ctx, owner.User.ID, owner.BrandCloud.ID, assignedProduct.ID, target.User.ID, ProductEditorRole); err != nil {
		t.Fatalf("restore collaborator editor role: %v", err)
	}
	if _, err := env.store.UpdateProductCollaborator(ctx, owner.User.ID, owner.BrandCloud.ID, assignedProduct.ID, target.User.ID, ProductOwnerRole); !errors.Is(err, ErrConflict) {
		t.Fatalf("invalid update role error = %v", err)
	}
	if _, err := env.store.UpdateProductCollaborator(ctx, owner.User.ID, owner.BrandCloud.ID, assignedProduct.ID, "00000000-0000-0000-0000-000000000000", ProductEditorRole); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing collaborator update error = %v", err)
	}
	if err := env.store.TransferProductOwnership(ctx, owner.User.ID, owner.BrandCloud.ID, assignedProduct.ID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing ownership target error = %v", err)
	}
	if err := env.store.TransferProductOwnership(ctx, owner.User.ID, owner.BrandCloud.ID, assignedProduct.ID, target.User.ID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := env.store.CanManageProductCollaborators(ctx, target.User.ID, owner.BrandCloud.ID, assignedProduct.ID); err != nil || !allowed {
		t.Fatalf("new Product owner cannot manage collaborators: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := env.store.CanManageProductCollaborators(ctx, owner.User.ID, owner.BrandCloud.ID, assignedProduct.ID); err != nil || !allowed {
		t.Fatalf("cloud owner lost ultimate collaborator authority: allowed=%v err=%v", allowed, err)
	}
	if err := env.store.RemoveProductCollaborator(ctx, target.User.ID, owner.BrandCloud.ID, assignedProduct.ID, owner.User.ID); err != nil {
		t.Fatalf("remove previous owner: %v", err)
	}
	if err := env.store.RemoveProductCollaborator(ctx, target.User.ID, owner.BrandCloud.ID, assignedProduct.ID, owner.User.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("repeated removal error = %v", err)
	}
}

func TestProductCollaboratorPendingInvitationLifecycleIntegration(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "pending-owner@example.com", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "pending-viewer@example.com", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true,email_verified_at=$3 WHERE id IN ($1,$2)`, target.User.ID, owner.User.ID, now); err != nil {
		t.Fatal(err)
	}
	admitProductCollaboratorFixture(t, env, owner, target, now)
	profile, err := env.store.CreateDeviceItemProfile(ctx, DeviceItemProfileCreateInput{
		ActorUserID: &owner.User.ID, BrandCloudID: owner.BrandCloud.ID,
		ProfileKey: "pending-product", DisplayName: "pending-product", Category: model.DeviceCategoryIPCamera,
		CAProfile: "brand-ca", IssuerProfile: "issuer", ServiceOptions: []string{"video_streaming"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.store.CreateProductCollaboratorInvitation(ctx, ProductCollaboratorInvitationInput{
		BrandCloudID: owner.BrandCloud.ID, ProductID: profile.ID, InvitedByUserID: owner.User.ID,
		TargetEmail: target.User.Email, Role: ProductViewerRole, TokenHash: "email-error", ExpiresAt: now.Add(time.Hour),
		Email: &EmailOutboxInput{},
	}, now); err == nil {
		t.Fatal("invitation with an unconfigured email outbox unexpectedly succeeded")
	}
	invitation, _, err := env.store.CreateProductCollaboratorInvitation(ctx, ProductCollaboratorInvitationInput{
		BrandCloudID: owner.BrandCloud.ID, ProductID: profile.ID, InvitedByUserID: owner.User.ID,
		TargetEmail: target.User.Email, Role: ProductViewerRole, TokenHash: "pending-token", ExpiresAt: now.Add(time.Hour),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	mutation := ProductCollaboratorInvitationMutation{
		BrandCloudID: owner.BrandCloud.ID, ProductID: profile.ID, InvitationID: invitation.ID,
		ActorUserID: owner.User.ID, TokenHash: "resent-token", ExpiresAt: now.Add(2 * time.Hour),
	}
	unauthorized := mutation
	unauthorized.ActorUserID = target.User.ID
	if _, err := env.store.ResendProductCollaboratorInvitation(ctx, unauthorized, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unauthorized resend error = %v", err)
	}
	if _, err := env.store.CancelProductCollaboratorInvitation(ctx, unauthorized, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unauthorized cancel error = %v", err)
	}
	if _, err := env.store.UpdateProductCollaborator(ctx, target.User.ID, owner.BrandCloud.ID, profile.ID, target.User.ID, ProductViewerRole); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unauthorized update error = %v", err)
	}
	if err := env.store.RemoveProductCollaborator(ctx, target.User.ID, owner.BrandCloud.ID, profile.ID, target.User.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unauthorized remove error = %v", err)
	}
	if err := env.store.TransferProductOwnership(ctx, target.User.ID, owner.BrandCloud.ID, profile.ID, target.User.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unauthorized transfer error = %v", err)
	}
	if _, _, err := env.store.CreateProductCollaboratorInvitation(ctx, ProductCollaboratorInvitationInput{
		BrandCloudID: owner.BrandCloud.ID, ProductID: profile.ID, InvitedByUserID: target.User.ID,
		TargetEmail: target.User.Email, Role: ProductViewerRole, TokenHash: "unauthorized", ExpiresAt: now.Add(time.Hour),
	}, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unauthorized invite error = %v", err)
	}
	withEmail := mutation
	withEmail.Email = &EmailOutboxInput{}
	if _, err := env.store.ResendProductCollaboratorInvitation(ctx, withEmail, now); err == nil {
		t.Fatal("resend with an unconfigured email outbox unexpectedly succeeded")
	}
	resent, err := env.store.ResendProductCollaboratorInvitation(ctx, mutation, now.Add(time.Minute))
	if err != nil || resent.ID != invitation.ID {
		t.Fatalf("resend invitation = %+v, err=%v", resent, err)
	}
	canceled, err := env.store.CancelProductCollaboratorInvitation(ctx, mutation, now.Add(2*time.Minute))
	if err != nil || canceled.Status != "canceled" {
		t.Fatalf("cancel invitation = %+v, err=%v", canceled, err)
	}
	if _, err := env.store.ResendProductCollaboratorInvitation(ctx, mutation, now.Add(3*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resend canceled invitation error = %v", err)
	}
	if _, err := env.store.CancelProductCollaboratorInvitation(ctx, mutation, now.Add(3*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel canceled invitation error = %v", err)
	}
	if _, err := env.store.AcceptProductCollaboratorInvitation(ctx, target.User.ID, "resent-token", now.Add(3*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("accept canceled invitation error = %v", err)
	}
	expired, _, err := env.store.CreateProductCollaboratorInvitation(ctx, ProductCollaboratorInvitationInput{
		BrandCloudID: owner.BrandCloud.ID, ProductID: profile.ID, InvitedByUserID: owner.User.ID,
		TargetEmail: target.User.Email, Role: ProductViewerRole, TokenHash: "expired-token", ExpiresAt: now.Add(-time.Minute),
	}, now)
	if err != nil || expired.ID == "" {
		t.Fatalf("create expired invitation = %+v, err=%v", expired, err)
	}
	if _, err := env.store.AcceptProductCollaboratorInvitation(ctx, target.User.ID, "expired-token", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired invitation error = %v", err)
	}
}
