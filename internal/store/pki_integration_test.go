package store

import (
	"context"
	"testing"
)

func TestPKIRolesDoNotImplyCustodianFromPlatformAdmin(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	admin, err := env.store.EnsurePlatformAdmin(ctx, "pki-admin@example.test", "test-hash", nil)
	if err != nil {
		t.Fatal(err)
	}
	roles, err := env.store.PKIRoles(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 1 || roles[0] != "platform_admin" {
		t.Fatalf("unexpected implicit PKI roles: %v", roles)
	}
	if _, err = env.store.CreateRoleAssignment(ctx, RoleAssignmentCreateInput{RoleName: "security_custodian", ActorType: "user", ActorID: admin.ID, ScopeType: "platform"}); err != nil {
		t.Fatal(err)
	}
	roles, err = env.store.PKIRoles(ctx, admin.ID)
	if err != nil || len(roles) != 2 {
		t.Fatalf("explicit custodian role %v: %v", roles, err)
	}
}
