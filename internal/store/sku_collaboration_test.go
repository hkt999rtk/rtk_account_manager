package store

import (
	"context"
	"errors"
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
	brandUser, err := env.store.CreateBrandCloudUser(ctx, owner.User.ID, owner.BrandCloud.ID, BrandCloudUserInput{
		Email: "brand-member@example.com", PasswordHash: "hash", Role: model.RoleMember,
	})
	if err != nil {
		t.Fatal(err)
	}
	if role, err := env.store.GetSKUCollaboratorRole(ctx, brandUser.BrandCloudUser.ID, owner.BrandCloud.ID, assignedSKU.ID); err != nil || role != string(model.RoleMember) {
		t.Fatalf("brand member SKU role = %q, err=%v", role, err)
	}
	if _, err := env.store.GetSKUCollaboratorRole(ctx, "00000000-0000-0000-0000-000000000000", owner.BrandCloud.ID, assignedSKU.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing brand member role error = %v", err)
	}
	if _, _, err := env.store.CreateSKUCollaboratorInvitation(ctx, SKUCollaboratorInvitationInput{
		BrandCloudID: owner.BrandCloud.ID, SKUID: assignedSKU.ID, InvitedByUserID: owner.User.ID,
		TargetEmail: target.User.Email, Role: SKUOwnerRole, TokenHash: "invalid-role", ExpiresAt: now.Add(time.Hour),
	}, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("invalid collaborator role error = %v", err)
	}
	if _, _, err := env.store.CreateSKUCollaboratorInvitation(ctx, SKUCollaboratorInvitationInput{
		BrandCloudID: owner.BrandCloud.ID, SKUID: assignedSKU.ID, InvitedByUserID: owner.User.ID,
		TargetEmail: "missing@example.com", Role: SKUEditorRole, TokenHash: "missing-user", ExpiresAt: now.Add(time.Hour),
	}, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing collaborator error = %v", err)
	}

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
	if _, _, err := env.store.CreateSKUCollaboratorInvitation(ctx, SKUCollaboratorInvitationInput{
		BrandCloudID: owner.BrandCloud.ID, SKUID: assignedSKU.ID, InvitedByUserID: owner.User.ID,
		TargetEmail: target.User.Email, Role: SKUEditorRole, TokenHash: "already-assigned", ExpiresAt: now.Add(time.Hour),
	}, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("already assigned invitation error = %v", err)
	}
	if role, err := env.store.GetUserSKUCollaboratorRole(ctx, target.User.ID, owner.BrandCloud.ID, assignedSKU.ID); err != nil || role != SKUEditorRole {
		t.Fatalf("collaborator role = %q, err=%v", role, err)
	}
	if _, err := env.store.GetUserSKUCollaboratorRole(ctx, "00000000-0000-0000-0000-000000000000", owner.BrandCloud.ID, assignedSKU.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing collaborator role error = %v", err)
	}
	if collaborators, err := env.store.ListSKUCollaborators(ctx, owner.BrandCloud.ID, assignedSKU.ID); err != nil || len(collaborators) != 2 {
		t.Fatalf("collaborators = %+v, err=%v", collaborators, err)
	}
	if allowed, err := env.store.HasUserPermissionForResource(ctx, target.User.ID, owner.BrandCloud.ID, "registry_device.manage", ScopeTypeSKU, assignedSKU.ID); err != nil || !allowed {
		t.Fatalf("SKU editor permission: allowed=%v err=%v", allowed, err)
	}
	if invitations, err := env.store.ListSKUCollaboratorInvitations(ctx, owner.BrandCloud.ID, assignedSKU.ID, now); err != nil || len(invitations) != 1 {
		t.Fatalf("invitations = %+v, err=%v", invitations, err)
	}
	page, err := env.store.ListDeviceItemProfiles(ctx, DeviceItemProfileListFilter{BrandCloudID: owner.BrandCloud.ID, UserID: target.User.ID, Limit: 20})
	if err != nil || page.Page.Total != 1 || len(page.Profiles) != 1 || page.Profiles[0].ID != assignedSKU.ID {
		t.Fatalf("collaborator SKU visibility = %+v, err=%v", page, err)
	}
	if _, err := env.store.UpdateSKUCollaborator(ctx, owner.User.ID, owner.BrandCloud.ID, assignedSKU.ID, target.User.ID, SKUViewerRole); err != nil {
		t.Fatalf("change collaborator to viewer: %v", err)
	}
	if _, err := env.store.UpdateSKUCollaborator(ctx, owner.User.ID, owner.BrandCloud.ID, assignedSKU.ID, target.User.ID, SKUEditorRole); err != nil {
		t.Fatalf("restore collaborator editor role: %v", err)
	}
	if _, err := env.store.UpdateSKUCollaborator(ctx, owner.User.ID, owner.BrandCloud.ID, assignedSKU.ID, target.User.ID, SKUOwnerRole); !errors.Is(err, ErrConflict) {
		t.Fatalf("invalid update role error = %v", err)
	}
	if _, err := env.store.UpdateSKUCollaborator(ctx, owner.User.ID, owner.BrandCloud.ID, assignedSKU.ID, "00000000-0000-0000-0000-000000000000", SKUEditorRole); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing collaborator update error = %v", err)
	}
	if err := env.store.TransferSKUOwnership(ctx, owner.User.ID, owner.BrandCloud.ID, assignedSKU.ID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing ownership target error = %v", err)
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
	if err := env.store.RemoveSKUCollaborator(ctx, target.User.ID, owner.BrandCloud.ID, assignedSKU.ID, owner.User.ID); err != nil {
		t.Fatalf("remove previous owner: %v", err)
	}
	if err := env.store.RemoveSKUCollaborator(ctx, target.User.ID, owner.BrandCloud.ID, assignedSKU.ID, owner.User.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("repeated removal error = %v", err)
	}
}

func TestSKUCollaboratorPendingInvitationLifecycleIntegration(t *testing.T) {
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
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true,email_verified_at=$2 WHERE id=$1`, target.User.ID, now); err != nil {
		t.Fatal(err)
	}
	profile, err := env.store.CreateDeviceItemProfile(ctx, DeviceItemProfileCreateInput{
		ActorUserID: &owner.User.ID, BrandCloudID: owner.BrandCloud.ID,
		ProfileKey: "pending-sku", DisplayName: "pending-sku", Category: model.DeviceCategoryIPCamera,
		CAProfile: "brand-ca", IssuerProfile: "issuer", ServiceOptions: []string{"video_streaming"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.store.CreateSKUCollaboratorInvitation(ctx, SKUCollaboratorInvitationInput{
		BrandCloudID: owner.BrandCloud.ID, SKUID: profile.ID, InvitedByUserID: owner.User.ID,
		TargetEmail: target.User.Email, Role: SKUViewerRole, TokenHash: "email-error", ExpiresAt: now.Add(time.Hour),
		Email: &EmailOutboxInput{},
	}, now); err == nil {
		t.Fatal("invitation with an unconfigured email outbox unexpectedly succeeded")
	}
	invitation, _, err := env.store.CreateSKUCollaboratorInvitation(ctx, SKUCollaboratorInvitationInput{
		BrandCloudID: owner.BrandCloud.ID, SKUID: profile.ID, InvitedByUserID: owner.User.ID,
		TargetEmail: target.User.Email, Role: SKUViewerRole, TokenHash: "pending-token", ExpiresAt: now.Add(time.Hour),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	mutation := SKUCollaboratorInvitationMutation{
		BrandCloudID: owner.BrandCloud.ID, SKUID: profile.ID, InvitationID: invitation.ID,
		ActorUserID: owner.User.ID, TokenHash: "resent-token", ExpiresAt: now.Add(2 * time.Hour),
	}
	unauthorized := mutation
	unauthorized.ActorUserID = target.User.ID
	if _, err := env.store.ResendSKUCollaboratorInvitation(ctx, unauthorized, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unauthorized resend error = %v", err)
	}
	if _, err := env.store.CancelSKUCollaboratorInvitation(ctx, unauthorized, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unauthorized cancel error = %v", err)
	}
	if _, err := env.store.UpdateSKUCollaborator(ctx, target.User.ID, owner.BrandCloud.ID, profile.ID, target.User.ID, SKUViewerRole); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unauthorized update error = %v", err)
	}
	if err := env.store.RemoveSKUCollaborator(ctx, target.User.ID, owner.BrandCloud.ID, profile.ID, target.User.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unauthorized remove error = %v", err)
	}
	if err := env.store.TransferSKUOwnership(ctx, target.User.ID, owner.BrandCloud.ID, profile.ID, target.User.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unauthorized transfer error = %v", err)
	}
	if _, _, err := env.store.CreateSKUCollaboratorInvitation(ctx, SKUCollaboratorInvitationInput{
		BrandCloudID: owner.BrandCloud.ID, SKUID: profile.ID, InvitedByUserID: target.User.ID,
		TargetEmail: target.User.Email, Role: SKUViewerRole, TokenHash: "unauthorized", ExpiresAt: now.Add(time.Hour),
	}, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unauthorized invite error = %v", err)
	}
	withEmail := mutation
	withEmail.Email = &EmailOutboxInput{}
	if _, err := env.store.ResendSKUCollaboratorInvitation(ctx, withEmail, now); err == nil {
		t.Fatal("resend with an unconfigured email outbox unexpectedly succeeded")
	}
	resent, err := env.store.ResendSKUCollaboratorInvitation(ctx, mutation, now.Add(time.Minute))
	if err != nil || resent.ID != invitation.ID {
		t.Fatalf("resend invitation = %+v, err=%v", resent, err)
	}
	canceled, err := env.store.CancelSKUCollaboratorInvitation(ctx, mutation, now.Add(2*time.Minute))
	if err != nil || canceled.Status != "canceled" {
		t.Fatalf("cancel invitation = %+v, err=%v", canceled, err)
	}
	if _, err := env.store.ResendSKUCollaboratorInvitation(ctx, mutation, now.Add(3*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resend canceled invitation error = %v", err)
	}
	if _, err := env.store.CancelSKUCollaboratorInvitation(ctx, mutation, now.Add(3*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel canceled invitation error = %v", err)
	}
	if _, err := env.store.AcceptSKUCollaboratorInvitation(ctx, target.User.ID, "resent-token", now.Add(3*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("accept canceled invitation error = %v", err)
	}
	expired, _, err := env.store.CreateSKUCollaboratorInvitation(ctx, SKUCollaboratorInvitationInput{
		BrandCloudID: owner.BrandCloud.ID, SKUID: profile.ID, InvitedByUserID: owner.User.ID,
		TargetEmail: target.User.Email, Role: SKUViewerRole, TokenHash: "expired-token", ExpiresAt: now.Add(-time.Minute),
	}, now)
	if err != nil || expired.ID == "" {
		t.Fatalf("create expired invitation = %+v, err=%v", expired, err)
	}
	if _, err := env.store.AcceptSKUCollaboratorInvitation(ctx, target.User.ID, "expired-token", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired invitation error = %v", err)
	}
}
