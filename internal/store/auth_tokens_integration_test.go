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

}
