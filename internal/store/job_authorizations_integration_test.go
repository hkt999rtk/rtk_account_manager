package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestJobAuthorizationLifecycle(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "job-authorization-owner")
	product, err := env.store.CreateDeviceItemProfileAsUser(ctx, authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "job-product"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	input := JobAuthorizationInput{
		JobID:        "11111111-1111-4111-8111-111111111111",
		BrandCloudID: owner.BrandCloud.ID,
		ActorUserID:  owner.User.ID,
		ScopeHash:    strings.Repeat("a", 64),
		Capability:   "provisioning.create",
		ProductIDs:   []string{product.ID},
		ExpiresAt:    now.Add(time.Hour),
	}
	grant, err := env.store.CreateJobAuthorization(ctx, input, now)
	if err != nil {
		t.Fatal(err)
	}
	if grant.JobID != input.JobID || grant.BrandCloudID != input.BrandCloudID || grant.Status != "active" {
		t.Fatalf("unexpected grant: %+v", grant)
	}
	validated, err := env.store.ValidateJobAuthorization(ctx, grant.ID, now.Add(time.Minute))
	if err != nil || validated.ID != grant.ID {
		t.Fatalf("validate: %+v %v", validated, err)
	}
	revoked, err := env.store.RevokeJobAuthorization(ctx, grant.ID, now.Add(2*time.Minute))
	if err != nil || revoked.Status != "revoked" || revoked.RevokedAt == nil {
		t.Fatalf("revoke: %+v %v", revoked, err)
	}
	if _, err = env.store.ValidateJobAuthorization(ctx, grant.ID, now.Add(3*time.Minute)); !errors.Is(err, ErrJobAuthorizationRevoked) {
		t.Fatalf("revoked grant validated: %v", err)
	}
}
