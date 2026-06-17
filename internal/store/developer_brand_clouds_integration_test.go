package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func TestDeveloperSignupCreatesDefaultBrandCloudAndEnforcesCloudLimit(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	result, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{
		Email:                     " Dev.Owner@Example.COM ",
		PasswordHash:              "hash",
		DisplayName:               stringPtr("Dev Owner"),
		SignupPendingVerification: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.User.Email != "dev.owner@example.com" || result.User.DeveloperCloudLimit != 8 {
		t.Fatalf("unexpected developer user: %+v", result.User)
	}
	if result.BrandCloud.Name != "dev.owner@example.com" ||
		result.BrandCloud.OrganizationKind != model.OrganizationKindBrandCloud ||
		result.BrandCloud.Role != model.RoleOwner {
		t.Fatalf("unexpected default brand cloud: %+v", result.BrandCloud)
	}

	page, err := env.store.ListDeveloperBrandClouds(ctx, result.User.ID, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Page.Total != 1 || len(page.Organizations) != 1 || page.Organizations[0].ID != result.BrandCloud.ID {
		t.Fatalf("expected default brand cloud in developer list, got %+v", page)
	}

	if err := env.store.SetDeveloperCloudLimit(ctx, result.User.ID, 2); err != nil {
		t.Fatal(err)
	}
	second, err := env.store.CreateDeveloperBrandCloud(ctx, result.User.ID, BrandCloudInput{Name: "Second Cloud", TenantSlug: "second-cloud"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Role != model.RoleOwner {
		t.Fatalf("developer-created brand cloud should return owner role, got %+v", second)
	}
	if _, err := env.store.CreateDeveloperBrandCloud(ctx, result.User.ID, BrandCloudInput{Name: "Third Cloud", TenantSlug: "third-cloud"}); !errors.Is(err, ErrDeveloperCloudLimitExceeded) {
		t.Fatalf("expected developer cloud limit error, got %v", err)
	}
	if err := env.store.SetDeveloperCloudLimit(ctx, result.User.ID, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CreateDeveloperBrandCloud(ctx, result.User.ID, BrandCloudInput{Name: "Third Cloud", TenantSlug: "third-cloud"}); err != nil {
		t.Fatalf("expected increased developer cloud limit to allow third cloud, got %v", err)
	}
}

func TestDeveloperBrandCloudErrorPaths(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	result, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{
		Email:        "cloud-errors@example.com",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{
		Email:        "cloud-errors@example.com",
		PasswordHash: "hash",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected duplicate developer signup conflict, got %v", err)
	}
	if _, err := env.store.CreateDeveloperBrandCloud(ctx, result.User.ID, BrandCloudInput{
		Name:       "Invalid Slug",
		TenantSlug: "!!!",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected invalid tenant slug conflict, got %v", err)
	}
	if _, err := env.store.CreateDeveloperBrandCloud(ctx, "00000000-0000-0000-0000-000000000000", BrandCloudInput{
		Name: "Missing User",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing developer not found, got %v", err)
	}
	if err := env.store.SetDeveloperCloudLimit(ctx, "00000000-0000-0000-0000-000000000000", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing developer limit update not found, got %v", err)
	}
	if _, err := env.store.GetDeveloperBrandCloudMember(ctx, result.BrandCloud.ID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing developer brand cloud member not found, got %v", err)
	}
}

func TestEnsurePlatformAdminCreatesRealtekConnectBrandCloud(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	root, err := env.store.EnsurePlatformAdmin(ctx, " Root.Admin@Example.COM ", "hash-one", stringPtr("Root Admin"))
	if err != nil {
		t.Fatal(err)
	}
	page, err := env.store.ListDeveloperBrandClouds(ctx, root.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Page.Total != 1 || len(page.Organizations) != 1 ||
		page.Organizations[0].Name != "Realtek Connect+" ||
		page.Organizations[0].Role != model.RoleOwner {
		t.Fatalf("expected root Realtek Connect+ cloud, got %+v", page)
	}

	rootAgain, err := env.store.EnsurePlatformAdmin(ctx, "root.admin@example.com", "hash-two", nil)
	if err != nil {
		t.Fatal(err)
	}
	pageAgain, err := env.store.ListDeveloperBrandClouds(ctx, rootAgain.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pageAgain.Page.Total != 1 || pageAgain.Organizations[0].ID != page.Organizations[0].ID {
		t.Fatalf("platform admin bootstrap must be idempotent, got before=%+v after=%+v", page, pageAgain)
	}
}

func TestBrandCloudOwnerTransferRequiresExistingTargetAndAcceptsWithLoggedInDeveloper(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	source, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{
		Email:        "source-owner@example.com",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{
		Email:        "target-owner@example.com",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := env.store.CreateBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferInput{
		BrandCloudID:      source.BrandCloud.ID,
		RequestedByUserID: source.User.ID,
		TargetEmail:       "missing-target@example.com",
		TokenHash:         "missing-token-hash",
		ExpiresAt:         time.Now().Add(time.Hour),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing developer target to reject transfer, got %v", err)
	}
	if _, err := env.store.CreateBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferInput{
		BrandCloudID:      source.BrandCloud.ID,
		RequestedByUserID: source.User.ID,
		TargetEmail:       "source-owner@example.com",
		TokenHash:         "self-token-hash",
		ExpiresAt:         time.Now().Add(time.Hour),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected self-transfer conflict, got %v", err)
	}
	if _, err := env.store.CreateBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferInput{
		BrandCloudID:      source.BrandCloud.ID,
		RequestedByUserID: target.User.ID,
		TargetEmail:       "source-owner@example.com",
		TokenHash:         "non-owner-token-hash",
		ExpiresAt:         time.Now().Add(time.Hour),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected non-owner transfer request not found, got %v", err)
	}

	transfer, err := env.store.CreateBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferInput{
		BrandCloudID:      source.BrandCloud.ID,
		RequestedByUserID: source.User.ID,
		TargetEmail:       " target-owner@example.com ",
		TokenHash:         "transfer-token-hash",
		ExpiresAt:         time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if transfer.Status != "pending" || transfer.TargetUserID != target.User.ID {
		t.Fatalf("unexpected owner transfer: %+v", transfer)
	}
	if _, err := env.store.CreateBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferInput{
		BrandCloudID:      source.BrandCloud.ID,
		RequestedByUserID: source.User.ID,
		TargetEmail:       "target-owner@example.com",
		TokenHash:         "expired-token-hash",
		ExpiresAt:         time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, "expired-token-hash", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired transfer token must reject, got %v", err)
	}
	if _, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, source.User.ID, "transfer-token-hash", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-target developer must not accept transfer, got %v", err)
	}
	accepted, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, "transfer-token-hash", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Status != "accepted" || accepted.AcceptedAt == nil {
		t.Fatalf("expected accepted transfer, got %+v", accepted)
	}

	sourceMember, err := env.store.GetDeveloperBrandCloudMember(ctx, source.BrandCloud.ID, source.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	targetMember, err := env.store.GetDeveloperBrandCloudMember(ctx, source.BrandCloud.ID, target.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceMember.Role != model.RoleAdmin || targetMember.Role != model.RoleOwner {
		t.Fatalf("expected source admin and target owner after transfer, source=%+v target=%+v", sourceMember, targetMember)
	}
	if _, err := env.store.AcceptBrandCloudOwnerTransfer(ctx, target.User.ID, "transfer-token-hash", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("accepted transfer token must reject replay, got %v", err)
	}
}
