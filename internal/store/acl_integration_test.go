package store

import (
	"context"
	"testing"
	"time"

	"rtk_account_manager/internal/auth"
)

func TestACLSeedPermissionCatalogAndSystemRoles(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	requiredRoles := []string{
		"platform_admin",
		"support_operator",
		"tenant_admin",
		"fleet_manager",
		"installer",
		"firmware_operator",
		"read_only_observer",
		"end_user",
		"device_agent",
		"service_integration",
	}
	for _, roleName := range requiredRoles {
		if _, err := env.store.GetRoleByName(ctx, roleName); err != nil {
			t.Fatalf("expected seeded role %s: %v", roleName, err)
		}
	}

	assertRolePermission(t, ctx, env, "tenant_admin", "membership.manage", true)
	assertRolePermission(t, ctx, env, "tenant_admin", "registry_device.manage", true)
	assertRolePermission(t, ctx, env, "fleet_manager", "registry_device.manage", true)
	assertRolePermission(t, ctx, env, "fleet_manager", "membership.manage", false)
	assertRolePermission(t, ctx, env, "installer", "claim.resolve", true)
	assertRolePermission(t, ctx, env, "installer", "registry_device.manage", false)
	assertRolePermission(t, ctx, env, "firmware_operator", "lifecycle_operation.deactivate", true)
	assertRolePermission(t, ctx, env, "read_only_observer", "registry_device.read", true)
	assertRolePermission(t, ctx, env, "read_only_observer", "registry_device.manage", false)
}

func TestEnsurePlatformAdminCreatesAndReenablesUser(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	displayName := "Platform Root"
	created, err := env.store.EnsurePlatformAdmin(ctx, " Root@Example.COM ", "hash-one", &displayName)
	if err != nil {
		t.Fatal(err)
	}
	if created.Email != "root@example.com" || created.DisplayName == nil || *created.DisplayName != displayName {
		t.Fatalf("expected normalized created platform admin, got %+v", created)
	}
	if !created.EmailVerified || created.SignupPendingVerification || created.DisabledAt != nil {
		t.Fatalf("expected enabled verified platform admin, got %+v", created)
	}
	isAdmin, err := env.store.IsPlatformAdmin(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !isAdmin {
		t.Fatal("expected ensured user to be platform admin")
	}

	if _, err := env.db.Exec(ctx, `UPDATE users SET disabled_at = now(), platform_admin = false WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := env.store.EnsurePlatformAdmin(ctx, "root@example.com", "hash-two", nil)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != created.ID || updated.DisplayName == nil || *updated.DisplayName != displayName {
		t.Fatalf("expected existing display name and user id to be preserved, got %+v", updated)
	}
	if updated.DisabledAt != nil {
		t.Fatalf("expected disabled platform admin to be reenabled, got %+v", updated)
	}
	isAdmin, err = env.store.IsPlatformAdmin(ctx, updated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !isAdmin {
		t.Fatal("expected reenabled user to be platform admin")
	}
}

func TestACLRoleAssignmentsAuthorizeInsideScopeOnly(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first, err := env.store.Register(ctx, RegisterInput{
		Email:                 "acl-owner@example.com",
		PasswordHash:          mustHashACLPassword(t),
		OrganizationName:      "ACL Owner Org",
		OrganizationTier:      "commercial",
		EvaluationDeviceQuota: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := env.store.Register(ctx, RegisterInput{
		Email:                 "acl-other@example.com",
		PasswordHash:          mustHashACLPassword(t),
		OrganizationName:      "ACL Other Org",
		OrganizationTier:      "commercial",
		EvaluationDeviceQuota: 5,
	})
	if err != nil {
		t.Fatal(err)
	}

	canManage, err := env.store.HasPermission(ctx, first.User.ID, first.Organization.ID, "registry_device.manage")
	if err != nil {
		t.Fatal(err)
	}
	if !canManage {
		t.Fatal("expected owner membership trigger to create role assignment with registry_device.manage")
	}
	crossOrg, err := env.store.HasPermission(ctx, first.User.ID, second.Organization.ID, "registry_device.manage")
	if err != nil {
		t.Fatal(err)
	}
	if crossOrg {
		t.Fatal("expected role assignment to be scoped to only the user's organization")
	}

	orgID := second.Organization.ID
	scopeID := second.Organization.ID
	if _, err := env.store.CreateRoleAssignment(ctx, RoleAssignmentCreateInput{
		RoleName:       "read_only_observer",
		ActorID:        first.User.ID,
		ScopeType:      ScopeTypeOrganization,
		ScopeID:        &scopeID,
		OrganizationID: &orgID,
		Now:            now,
	}); err != nil {
		t.Fatal(err)
	}
	canReadOther, err := env.store.HasPermission(ctx, first.User.ID, second.Organization.ID, "registry_device.read")
	if err != nil {
		t.Fatal(err)
	}
	if !canReadOther {
		t.Fatal("expected explicit read_only_observer assignment to grant read permission")
	}
	canWriteOther, err := env.store.HasPermission(ctx, first.User.ID, second.Organization.ID, "registry_device.manage")
	if err != nil {
		t.Fatal(err)
	}
	if canWriteOther {
		t.Fatal("read_only_observer must not grant write permission")
	}
}

func TestListUserOrganizationPermissionsIncludesOwnerBillingOnlyInsideOrganization(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	owner, err := env.store.Register(ctx, RegisterInput{
		Email:            "billing-capabilities-owner@example.com",
		PasswordHash:     mustHashACLPassword(t),
		OrganizationName: "Billing Capabilities Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	other, err := env.store.Register(ctx, RegisterInput{
		Email:            "billing-capabilities-other@example.com",
		PasswordHash:     mustHashACLPassword(t),
		OrganizationName: "Other Billing Capabilities Org",
	})
	if err != nil {
		t.Fatal(err)
	}

	permissions, err := env.store.ListUserOrganizationPermissions(ctx, owner.User.ID, owner.Organization.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsPermission(permissions, "billing_account.read") {
		t.Fatalf("owner organization permissions missing billing_account.read: %v", permissions)
	}
	permissions, err = env.store.ListUserOrganizationPermissions(ctx, owner.User.ID, other.Organization.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(permissions) != 0 {
		t.Fatalf("permissions leaked into another organization: %v", permissions)
	}
}

func containsPermission(permissions []string, target string) bool {
	for _, permission := range permissions {
		if permission == target {
			return true
		}
	}
	return false
}

func TestACLExternalGroupMappingCreatesScopedAssignment(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	registered, err := env.store.Register(ctx, RegisterInput{
		Email:                 "acl-groups@example.com",
		PasswordHash:          mustHashACLPassword(t),
		OrganizationName:      "ACL Groups Org",
		OrganizationTier:      "commercial",
		EvaluationDeviceQuota: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	orgID := registered.Organization.ID
	scopeID := registered.Organization.ID
	if _, err := env.store.CreateExternalGroupMapping(ctx, ExternalGroupMappingCreateInput{
		ProviderID:     "keycloak",
		ExternalGroup:  "/installers",
		RoleName:       "installer",
		ScopeType:      ScopeTypeOrganization,
		ScopeID:        &scopeID,
		OrganizationID: &orgID,
		Now:            now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.ApplyExternalGroupMappings(ctx, registered.User.ID, "keycloak", []string{"/unmapped"}, now); err != nil {
		t.Fatal(err)
	}
	canClaimBefore, err := env.store.HasPermission(ctx, registered.User.ID, registered.Organization.ID, "claim.resolve")
	if err != nil {
		t.Fatal(err)
	}
	if !canClaimBefore {
		t.Fatal("owner compatibility assignment should already claim; test setup sanity failed")
	}

	observer, err := env.store.Register(ctx, RegisterInput{
		Email:                 "acl-observer@example.com",
		PasswordHash:          mustHashACLPassword(t),
		OrganizationName:      "ACL Observer Org",
		OrganizationTier:      "commercial",
		EvaluationDeviceQuota: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.store.ApplyExternalGroupMappings(ctx, observer.User.ID, "keycloak", []string{"/installers"}, now); err != nil {
		t.Fatal(err)
	}
	canClaimMapped, err := env.store.HasPermission(ctx, observer.User.ID, registered.Organization.ID, "claim.resolve")
	if err != nil {
		t.Fatal(err)
	}
	if !canClaimMapped {
		t.Fatal("expected mapped external group to grant installer claim permission in mapped org scope")
	}
	canManageMapped, err := env.store.HasPermission(ctx, observer.User.ID, registered.Organization.ID, "registry_device.manage")
	if err != nil {
		t.Fatal(err)
	}
	if canManageMapped {
		t.Fatal("installer mapping must not grant registry_device.manage")
	}
}

func TestACLPlatformAssignmentsAuditAndErrorPaths(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	registered, err := env.store.Register(ctx, RegisterInput{
		Email:                 "acl-platform@example.com",
		PasswordHash:          mustHashACLPassword(t),
		OrganizationName:      "ACL Platform Org",
		OrganizationTier:      "commercial",
		EvaluationDeviceQuota: 5,
	})
	if err != nil {
		t.Fatal(err)
	}

	if allowed, err := env.store.HasPermission(ctx, registered.User.ID, "", "platform_metrics.read"); err != nil {
		t.Fatal(err)
	} else if allowed {
		t.Fatal("ordinary user must not have platform permission")
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin = true WHERE id = $1`, registered.User.ID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := env.store.HasPermission(ctx, registered.User.ID, "", "platform_metrics.read"); err != nil {
		t.Fatal(err)
	} else if !allowed {
		t.Fatal("platform_admin compatibility flag should grant platform permission")
	}
	if allowed, err := env.store.HasPermission(ctx, registered.User.ID, "", "device.unprovision_override"); err != nil {
		t.Fatal(err)
	} else if !allowed {
		t.Fatal("platform_admin compatibility flag should grant unprovision override permission")
	}
	if allowed, err := env.store.HasPermission(ctx, registered.User.ID, registered.Organization.ID, ""); err != nil {
		t.Fatal(err)
	} else if allowed {
		t.Fatal("blank permissions must not authorize")
	}

	if _, err := env.store.ListPermissions(ctx, 5, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ListRoles(ctx, 5, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.GetRoleByName(ctx, "missing-role"); err != ErrNotFound {
		t.Fatalf("expected missing role ErrNotFound, got %v", err)
	}
	if err := env.store.BindRolePermission(ctx, "missing-role", "acl.read", &registered.User.ID); err != ErrNotFound {
		t.Fatalf("expected bind missing role ErrNotFound, got %v", err)
	}
	if err := env.store.BindRolePermission(ctx, "support_operator", "missing.permission", &registered.User.ID); err != ErrNotFound {
		t.Fatalf("expected bind missing permission ErrNotFound, got %v", err)
	}

	description := "temporary ACL role"
	role, err := env.store.CreateRole(ctx, RoleCreateInput{Name: "temporary_acl_role_" + registered.User.ID[:8], ScopeType: ScopeTypeOrganization, Description: &description})
	if err != nil {
		t.Fatal(err)
	}
	updatedDescription := "updated temporary ACL role"
	if _, err := env.store.UpdateRole(ctx, RoleUpdateInput{Name: role.Name, Description: &updatedDescription, ActorUserID: &registered.User.ID}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.DisableRole(ctx, role.Name, &registered.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.UpdateRole(ctx, RoleUpdateInput{Name: role.Name, Description: &updatedDescription, ActorUserID: &registered.User.ID}); err != ErrNotFound {
		t.Fatalf("expected disabled role update ErrNotFound, got %v", err)
	}
	if err := env.store.DisableRole(ctx, role.Name, &registered.User.ID); err != ErrNotFound {
		t.Fatalf("expected disabled role delete ErrNotFound, got %v", err)
	}

	assignment, err := env.store.CreateRoleAssignment(ctx, RoleAssignmentCreateInput{
		RoleName:  "support_operator",
		ActorID:   registered.User.ID,
		ScopeType: ScopeTypePlatform,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ListRoleAssignments(ctx, 10, 0); err != nil {
		t.Fatal(err)
	}
	platformPermissions, err := env.store.ListUserPlatformPermissions(ctx, registered.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(platformPermissions) == 0 {
		t.Fatal("expected support_operator platform permissions")
	}
	if err := env.store.DisableRoleAssignment(ctx, assignment.ID, &registered.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.store.DisableRoleAssignment(ctx, assignment.ID, &registered.User.ID); err != ErrNotFound {
		t.Fatalf("expected disabled assignment ErrNotFound, got %v", err)
	}

	mapping, err := env.store.CreateExternalGroupMapping(ctx, ExternalGroupMappingCreateInput{
		ProviderID:    "keycloak",
		ExternalGroup: "/support",
		RoleName:      "support_operator",
		ScopeType:     ScopeTypePlatform,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ListExternalGroupMappings(ctx, 10, 0); err != nil {
		t.Fatal(err)
	}
	if err := env.store.DisableExternalGroupMapping(ctx, mapping.ID, &registered.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.store.DisableExternalGroupMapping(ctx, mapping.ID, &registered.User.ID); err != ErrNotFound {
		t.Fatalf("expected disabled mapping ErrNotFound, got %v", err)
	}
	if _, err := env.store.CreateExternalGroupMapping(ctx, ExternalGroupMappingCreateInput{
		ProviderID:    "keycloak",
		ExternalGroup: "/support",
		RoleName:      "support_operator",
		ScopeType:     ScopeTypePlatform,
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.ApplyExternalGroupMappings(ctx, registered.User.ID, "keycloak", nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.ApplyExternalGroupMappings(ctx, registered.User.ID, "keycloak", []string{"", "/support", "/support"}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	events, err := env.store.ListACLAuditEvents(ctx, ACLAuditEventListFilter{EventType: "role_assignment_created", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if events.Page.Total == 0 {
		t.Fatal("expected ACL audit events for role assignments")
	}
}

func assertRolePermission(t *testing.T, ctx context.Context, env storeIntegrationEnv, roleName, permissionName string, want bool) {
	t.Helper()
	var got bool
	if err := env.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM roles r
			JOIN role_permissions rp ON rp.role_id = r.id
			JOIN permissions p ON p.id = rp.permission_id
			WHERE r.name = $1 AND p.name = $2
		)
	`, roleName, permissionName).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("role %s permission %s: got %v want %v", roleName, permissionName, got, want)
	}
}

func mustHashACLPassword(t *testing.T) string {
	t.Helper()
	hash, err := auth.HashPassword("password123")
	if err != nil {
		t.Fatal(err)
	}
	return hash
}
