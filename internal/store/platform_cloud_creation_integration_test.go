package store

import (
	"context"
	"errors"
	"testing"
)

func TestMultiCloudPlatformCreationIsAtomicAndExplicitIntegration(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	actor, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "operator@test.invalid", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := env.store.SignupDeveloper(ctx, DeveloperSignupInput{Email: "designated-owner@test.invalid", PasswordHash: "hash", SignupPendingVerification: true})
	if err != nil {
		t.Fatal(err)
	}
	input := BrandCloudInput{Name: "Explicit Owner", TenantSlug: "explicit-owner", OwnerUserID: owner.User.ID}
	assertCloudAbsent := func() {
		t.Helper()
		var count int
		if err := env.db.QueryRow(ctx, `SELECT count(*) FROM organizations WHERE tenant_slug=$1`, input.TenantSlug).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed creation left cloud: count=%d err=%v", count, err)
		}
	}
	if _, err := env.store.CreateBrandCloud(ctx, actor.User.ID, BrandCloudInput{Name: input.Name}); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing explicit owner: %v", err)
	}
	if _, err := env.store.CreateBrandCloud(ctx, actor.User.ID, input); !errors.Is(err, ErrAccountNotActivated) {
		t.Fatalf("pending owner accepted: %v", err)
	}
	assertCloudAbsent()
	if _, err := env.db.Exec(ctx, `UPDATE users SET email_verified=true,signup_pending_verification=false,developer_cloud_limit=1 WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CreateBrandCloud(ctx, actor.User.ID, input); !errors.Is(err, ErrDeveloperCloudLimitExceeded) {
		t.Fatalf("platform bypassed owner quota: %v", err)
	}
	assertCloudAbsent()
	if err := env.store.SetDeveloperCloudLimit(ctx, owner.User.ID, 2); err != nil {
		t.Fatal(err)
	}
	// Failure to persist the operator's audit event must roll back owner and cloud.
	if _, err := env.store.CreateBrandCloud(ctx, "invalid-audit-actor", input); err == nil {
		t.Fatal("invalid audit actor accepted")
	}
	assertCloudAbsent()
	cloud, err := env.store.CreateBrandCloud(ctx, actor.User.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if cloud.Role != "" {
		t.Fatalf("operator response claims owner role: %s", cloud.Role)
	}
	var actualOwner string
	var members, audits int
	if err := env.db.QueryRow(ctx, `SELECT user_id::text FROM organization_members WHERE organization_id=$1 AND role='owner'`, cloud.ID).Scan(&actualOwner); err != nil || actualOwner != owner.User.ID {
		t.Fatalf("owner=%s err=%v", actualOwner, err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM organization_members WHERE organization_id=$1`, cloud.ID).Scan(&members); err != nil || members != 1 {
		t.Fatalf("operator received implicit access: members=%d err=%v", members, err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE organization_id=$1 AND event_type='brand_cloud_created' AND actor_user_id=$2 AND payload->>'owner_user_id'=$3`, cloud.ID, actor.User.ID, owner.User.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("owner attribution missing: audits=%d err=%v", audits, err)
	}
}
