package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func TestLoginActivationTokenLifecycleAndScope(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	registered, err := env.store.Register(ctx, RegisterInput{
		Email:            "activation-user@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Activation Org",
	})
	if err != nil {
		t.Fatal(err)
	}

	created, err := env.store.CreateLoginActivationTokenForEmail(ctx, " activation-user@example.com ", "console-token-hash", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected console login activation token to be created")
	}

	activated, err := env.store.ActivateLoginToken(ctx, "console-token-hash")
	if err != nil {
		t.Fatal(err)
	}
	if activated.ID != registered.User.ID || activated.Email != registered.User.Email {
		t.Fatalf("unexpected activated user: %+v", activated)
	}
	if _, err := env.store.ActivateLoginToken(ctx, "console-token-hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected consumed console activation token to reject replay, got %v", err)
	}

	if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, registered.User.ID); err != nil {
		t.Fatal(err)
	}
	disabledCreated, err := env.store.CreateLoginActivationTokenForEmail(ctx, registered.User.Email, "disabled-token-hash", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if disabledCreated {
		t.Fatal("disabled users must not get login activation tokens")
	}
}

func TestBrandCloudLoginActivationTokenIsTenantScoped(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	admin, err := env.store.Register(ctx, RegisterInput{
		Email:            "brand-activation-admin@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Brand Activation Admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	acme, err := createVerifiedOwnedCloudForTest(t, env, admin.User.ID, BrandCloudInput{Name: "Acme", TenantSlug: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	contoso, err := createVerifiedOwnedCloudForTest(t, env, admin.User.ID, BrandCloudInput{Name: "Contoso", TenantSlug: "contoso"})
	if err != nil {
		t.Fatal(err)
	}
	createdUser, err := env.store.CreateBrandCloudUser(ctx, admin.User.ID, acme.ID, BrandCloudUserInput{
		Email:        "shared-brand-user@example.com",
		PasswordHash: "hash",
		DisplayName:  stringPtr("Shared Brand User"),
		Role:         "member",
	})
	if err != nil {
		t.Fatal(err)
	}

	created, err := env.store.CreateBrandCloudLoginActivationTokenForEmail(ctx, "acme", "shared-brand-user@example.com", "brand-token-hash", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected tenant-scoped login activation token")
	}
	if _, err := env.store.ActivateBrandCloudLoginToken(ctx, "contoso", "brand-token-hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected tenant mismatch to reject activation token, got %v", err)
	}

	activated, err := env.store.ActivateBrandCloudLoginToken(ctx, "acme", "brand-token-hash")
	if err != nil {
		t.Fatal(err)
	}
	if activated.BrandCloud.ID != acme.ID || activated.BrandCloud.ID == contoso.ID || activated.BrandCloudUser.ID != createdUser.BrandCloudUser.ID {
		t.Fatalf("unexpected activated brand-cloud user: %+v", activated)
	}
	if _, err := env.store.ActivateBrandCloudLoginToken(ctx, "acme", "brand-token-hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected consumed brand activation token to reject replay, got %v", err)
	}

	pending, err := env.store.CreateBrandCloudUser(ctx, admin.User.ID, acme.ID, BrandCloudUserInput{
		Email:        "pending-brand-user@example.com",
		PasswordHash: "hash",
		DisplayName:  stringPtr("Pending Brand User"),
		Role:         "member",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `
		UPDATE brand_cloud_users
		SET signup_pending_verification = true
		WHERE id = $1
	`, pending.BrandCloudUser.ID); err != nil {
		t.Fatal(err)
	}
	pendingCreated, err := env.store.CreateBrandCloudLoginActivationTokenForEmail(ctx, "acme", pending.BrandCloudUser.Email, "pending-token-hash", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if pendingCreated {
		t.Fatal("pending brand-cloud users must not get login activation tokens")
	}
}

func TestBrandCloudStoreCRUDAndErrorPaths(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	admin, err := env.store.Register(ctx, RegisterInput{
		Email:            "brand-crud-admin@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Brand CRUD Admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	member, err := env.store.Register(ctx, RegisterInput{
		Email:            "brand-crud-member@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Brand CRUD Member",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := createVerifiedOwnedCloudForTest(t, env, admin.User.ID, BrandCloudInput{Name: "Invalid Slug", TenantSlug: "!!!"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected invalid tenant slug conflict, got %v", err)
	}
	acme, err := createVerifiedOwnedCloudForTest(t, env, admin.User.ID, BrandCloudInput{
		Name:       "Acme Cameras",
		TenantSlug: "acme-crud",
		Metadata:   map[string]any{"region": "tw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createVerifiedOwnedCloudForTest(t, env, admin.User.ID, BrandCloudInput{Name: "Duplicate", TenantSlug: "acme-crud"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected duplicate tenant slug conflict, got %v", err)
	}
	contoso, err := createVerifiedOwnedCloudForTest(t, env, admin.User.ID, BrandCloudInput{Name: "Contoso Cameras", TenantSlug: "contoso-crud"})
	if err != nil {
		t.Fatal(err)
	}

	firstPage, err := env.store.ListBrandClouds(ctx, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if firstPage.Page.Total != 2 || len(firstPage.Organizations) != 1 || firstPage.Organizations[0].ID != acme.ID {
		t.Fatalf("unexpected first brand-cloud page: %+v", firstPage)
	}
	secondPage, err := env.store.ListBrandClouds(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if secondPage.Page.Total != 2 || len(secondPage.Organizations) != 1 || secondPage.Organizations[0].ID != contoso.ID {
		t.Fatalf("unexpected second brand-cloud page: %+v", secondPage)
	}

	updated, err := env.store.UpdateBrandCloud(ctx, admin.User.ID, acme.ID, BrandCloudInput{
		Name:     "Acme Cameras Pro",
		Status:   model.OrganizationStatusDisabled,
		Metadata: map[string]any{"region": "us"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Acme Cameras Pro" || updated.Status != model.OrganizationStatusDisabled || updated.Metadata["region"] != "us" {
		t.Fatalf("unexpected updated brand cloud: %+v", updated)
	}
	if _, err := env.store.GetBrandCloud(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing brand cloud not found, got %v", err)
	}
	if _, err := env.store.UpdateBrandCloud(ctx, admin.User.ID, "00000000-0000-0000-0000-000000000000", BrandCloudInput{Name: "Missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing brand cloud update not found, got %v", err)
	}

	brandMember, err := env.store.CreateBrandCloudUser(ctx, admin.User.ID, acme.ID, BrandCloudUserInput{
		Email:        member.User.Email,
		PasswordHash: "hash",
		Role:         model.RoleMember,
	})
	if err != nil {
		t.Fatal(err)
	}
	assigned, err := env.store.AssignBrandCloudMember(ctx, admin.User.ID, acme.ID, brandMember.BrandCloudUser.ID, model.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if assigned.BrandCloudUserID != brandMember.BrandCloudUser.ID || assigned.Role != model.RoleAdmin {
		t.Fatalf("unexpected assigned brand-cloud member: %+v", assigned)
	}
	if _, err := env.store.AssignBrandCloudMember(ctx, admin.User.ID, "00000000-0000-0000-0000-000000000000", brandMember.BrandCloudUser.ID, model.RoleMember); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing brand cloud member assignment not found, got %v", err)
	}
}

func TestBrandCloudUserProvisioningUsesBrandScopedIdentityOnly(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	admin, err := env.store.Register(ctx, RegisterInput{
		Email:            "brand-target-admin@example.com",
		PasswordHash:     "hash",
		OrganizationName: "Brand Target Admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	acme, err := createVerifiedOwnedCloudForTest(t, env, admin.User.ID, BrandCloudInput{Name: "Target Acme", TenantSlug: "target-acme"})
	if err != nil {
		t.Fatal(err)
	}
	contoso, err := createVerifiedOwnedCloudForTest(t, env, admin.User.ID, BrandCloudInput{Name: "Target Contoso", TenantSlug: "target-contoso"})
	if err != nil {
		t.Fatal(err)
	}

	acmeUser, err := env.store.CreateBrandCloudUser(ctx, admin.User.ID, acme.ID, BrandCloudUserInput{
		Email:        "shared-target@example.com",
		PasswordHash: "hash-acme",
		Role:         model.RoleOwner,
	})
	if err != nil {
		t.Fatal(err)
	}
	contosoUser, err := env.store.CreateBrandCloudUser(ctx, admin.User.ID, contoso.ID, BrandCloudUserInput{
		Email:        "shared-target@example.com",
		PasswordHash: "hash-contoso",
		Role:         model.RoleAdmin,
	})
	if err != nil {
		t.Fatal(err)
	}
	if acmeUser.BrandCloudUser.ID == contosoUser.BrandCloudUser.ID {
		t.Fatalf("same email in different brand clouds must create distinct brand-cloud users: acme=%+v contoso=%+v", acmeUser, contosoUser)
	}

	var globalUserCount int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM users WHERE email = 'shared-target@example.com'`).Scan(&globalUserCount); err != nil {
		t.Fatal(err)
	}
	if globalUserCount != 0 {
		t.Fatalf("brand-cloud user provisioning must not create global users, got %d", globalUserCount)
	}
	var legacyMembershipCount int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM organization_members WHERE organization_id IN ($1, $2) AND NOT (user_id=$3 AND role='owner')`, acme.ID, contoso.ID, admin.User.ID).Scan(&legacyMembershipCount); err != nil {
		t.Fatal(err)
	}
	if legacyMembershipCount != 0 {
		t.Fatalf("brand-cloud user provisioning must not create legacy organization_members, got %d", legacyMembershipCount)
	}

	assigned, err := env.store.AssignBrandCloudMember(ctx, admin.User.ID, acme.ID, acmeUser.BrandCloudUser.ID, model.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if assigned.BrandCloudUserID != acmeUser.BrandCloudUser.ID || assigned.Role != model.RoleAdmin {
		t.Fatalf("brand-cloud member assignment must use brand_cloud_user_id only, got %+v", assigned)
	}
	if _, err := env.store.AssignBrandCloudMember(ctx, admin.User.ID, acme.ID, admin.User.ID, model.RoleMember); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy global user_id assignment must be rejected, got %v", err)
	}
}
