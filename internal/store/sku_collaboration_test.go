package store

import (
	"context"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func TestValidSKUCollaboratorRole(t *testing.T) {
	for _, role := range []string{SKUEditorRole, SKUViewerRole} {
		if !validSKUCollaboratorRole(role) {
			t.Fatalf("validSKUCollaboratorRole(%q) = false", role)
		}
	}
	for _, role := range []string{"", SKUOwnerRole, "admin", "member"} {
		if validSKUCollaboratorRole(role) {
			t.Fatalf("validSKUCollaboratorRole(%q) = true", role)
		}
	}
}

func TestSKUCollaborationInvitationVisibilityAndOwnershipTransferIntegration(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "sku-owner@example.com", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "sku-editor@example.com", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true,email_verified_at=$2 WHERE id=$1`, target.User.ID, now); err != nil {
		t.Fatal(err)
	}
	createSKU := func(key string) model.DeviceItemProfile {
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
	assignedSKU := createSKU("assigned-sku")
	_ = createSKU("hidden-sku")

	invitation, created, err := env.store.CreateSKUCollaboratorInvitation(ctx, SKUCollaboratorInvitationInput{
		BrandCloudID: owner.BrandCloud.ID, SKUID: assignedSKU.ID, InvitedByUserID: owner.User.ID,
		TargetEmail: target.User.Email, Role: SKUEditorRole, TokenHash: "sku-invite-token", ExpiresAt: now.Add(time.Hour),
	}, now)
	if err != nil || !created || invitation.Role != SKUEditorRole {
		t.Fatalf("create invitation: created=%v invitation=%+v err=%v", created, invitation, err)
	}
	if _, err := env.store.AcceptSKUCollaboratorInvitation(ctx, target.User.ID, "sku-invite-token", now); err != nil {
		t.Fatal(err)
	}
	page, err := env.store.ListDeviceItemProfiles(ctx, DeviceItemProfileListFilter{BrandCloudID: owner.BrandCloud.ID, UserID: target.User.ID, Limit: 20})
	if err != nil || page.Page.Total != 1 || len(page.Profiles) != 1 || page.Profiles[0].ID != assignedSKU.ID {
		t.Fatalf("collaborator SKU visibility = %+v, err=%v", page, err)
	}
	if err := env.store.TransferSKUOwnership(ctx, owner.User.ID, owner.BrandCloud.ID, assignedSKU.ID, target.User.ID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := env.store.CanManageSKUCollaborators(ctx, target.User.ID, owner.BrandCloud.ID, assignedSKU.ID); err != nil || !allowed {
		t.Fatalf("new SKU owner cannot manage collaborators: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := env.store.CanManageSKUCollaborators(ctx, owner.User.ID, owner.BrandCloud.ID, assignedSKU.ID); err != nil || allowed {
		t.Fatalf("previous SKU owner retained collaborator authority: allowed=%v err=%v", allowed, err)
	}
}
