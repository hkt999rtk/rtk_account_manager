package store

import (
	"context"
	"errors"
	"testing"
)

func TestMultiCloudListScopeAndQuotaIntegration(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "list-owner@test.invalid", PasswordHash: "hash", SignupPendingVerification: true})
	if err != nil {
		t.Fatal(err)
	}
	other, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "list-other@test.invalid", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CreateDeveloperBrandCloud(ctx, owner.User.ID, BrandCloudInput{Name: "pending must fail"}); !errors.Is(err, ErrAccountNotActivated) {
		t.Fatalf("pending creation err=%v", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true,signup_pending_verification=false WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	second, err := env.store.CreateDeveloperBrandCloud(ctx, owner.User.ID, BrandCloudInput{Name: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=false,signup_pending_verification=true WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES ($1,$2,'member')`, other.BrandCloud.ID, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		view  string
		total int
	}{{"all", 3}, {"owned", 2}, {"shared", 1}} {
		page, err := env.store.ListManagedBrandClouds(ctx, owner.User.ID, test.view, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != test.total || len(page.BrandClouds) != 1 || page.OwnedCount != 2 || page.OwnedLimit != 8 {
			t.Fatalf("%s page=%+v", test.view, page)
		}
		cloud := page.BrandClouds[0]
		if cloud.OwnerUserID == "" || cloud.OwnershipVersion != 1 || cloud.MyRole != cloud.Role || cloud.Operational || cloud.Capabilities == nil {
			t.Fatalf("cloud projection=%+v", cloud)
		}
	}
	empty, err := env.store.ListManagedBrandClouds(ctx, owner.User.ID, "all", 1, 99)
	if err != nil || empty.Total != 3 || len(empty.BrandClouds) != 0 || empty.OwnedCount != 2 {
		t.Fatalf("empty page=%+v err=%v", empty, err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE organizations SET deleted_at=now() WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE organization_members SET disabled_at=now() WHERE organization_id=$1 AND user_id=$2`, other.BrandCloud.ID, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	page, err := env.store.ListManagedBrandClouds(ctx, owner.User.ID, "all", 25, 0)
	if err != nil || page.Total != 1 || page.OwnedCount != 1 || len(page.BrandClouds) != 1 {
		t.Fatalf("filtered page=%+v err=%v", page, err)
	}
	if page.BrandClouds[0].Status != "pending_activation" {
		t.Fatalf("pending projection=%+v", page.BrandClouds[0])
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true,signup_pending_verification=false WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	page, err = env.store.ListManagedBrandClouds(ctx, owner.User.ID, "owned", 25, 0)
	if err != nil || !page.BrandClouds[0].Operational || page.BrandClouds[0].Status != "active" {
		t.Fatalf("activated page=%+v err=%v", page, err)
	}
	for _, view := range []string{"unknown", "OWNED"} {
		if _, err := env.store.ListManagedBrandClouds(ctx, owner.User.ID, view, 25, 0); !errors.Is(err, ErrConflict) {
			t.Fatalf("view %s err=%v", view, err)
		}
	}
	for _, limits := range [][2]int{{0, 0}, {101, 0}, {1, -1}} {
		if _, err := env.store.ListManagedBrandClouds(ctx, owner.User.ID, "all", limits[0], limits[1]); !errors.Is(err, ErrConflict) {
			t.Fatalf("limits=%v err=%v", limits, err)
		}
	}
	if _, err := env.store.ListManagedBrandClouds(ctx, "missing", "all", 25, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user err=%v", err)
	}
}

func TestMultiCloudConcurrentCreationQuotaIntegration(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "quota-owner@test.invalid", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "quota-other@test.invalid", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true,signup_pending_verification=false WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'admin')`, other.BrandCloud.ID, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.store.SetDeveloperCloudLimit(ctx, owner.User.ID, 2); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, name := range []string{"parallel-a", "parallel-b"} {
		go func(name string) {
			<-start
			_, err := env.store.CreateDeveloperBrandCloud(ctx, owner.User.ID, BrandCloudInput{Name: name})
			results <- err
		}(name)
	}
	close(start)
	success, limited := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrDeveloperCloudLimitExceeded) {
			limited++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || limited != 1 {
		t.Fatalf("success=%d limited=%d", success, limited)
	}
	page, err := env.store.ListManagedBrandClouds(ctx, owner.User.ID, "all", 25, 0)
	if err != nil || page.Total != 3 || page.OwnedCount != 2 || page.OwnedLimit != 2 {
		t.Fatalf("quota page=%+v err=%v", page, err)
	}
}
