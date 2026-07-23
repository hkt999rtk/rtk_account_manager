package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"rtk_account_manager/internal/model"
)

const (
	ActorTypeUser                    = "user"
	ActorTypeBrandCloudUser          = "brand_cloud_user"
	ScopeTypePlatform                = "platform"
	ScopeTypeOrganization            = "organization"
	ScopeTypeSKU                     = "sku"
	ScopeTypeRegion                  = "region"
	ScopeTypeGroup                   = "group"
	ScopeTypeDevice                  = "device"
	PermissionACLRead                = "acl.read"
	PermissionACLManage              = "acl.manage"
	PermissionPlatformRead           = "platform_metrics.read"
	PermissionChipsetProviderRead    = "platform.chipset_sdk.read"
	PermissionChipsetProviderEdit    = "platform.chipset_sdk.edit"
	PermissionChipsetProviderPublish = "platform.chipset_sdk.publish"
)

type PermissionPage struct {
	Permissions []model.Permission
	Page        Page
}

type ProductRolePage struct {
	Roles []model.ProductRole
	Page  Page
}

type RoleAssignmentPage struct {
	Assignments []model.RoleAssignment
	Page        Page
}

type ExternalGroupMappingPage struct {
	Mappings []model.ExternalGroupMapping
	Page     Page
}

type ACLAuditEventPage struct {
	Events []model.ACLAuditEvent
	Page   Page
}

type RoleCreateInput struct {
	Name        string
	ScopeType   string
	Description *string
	SystemRole  bool
}

type RoleUpdateInput struct {
	Name        string
	Description *string
	ActorUserID *string
}

type RoleAssignmentCreateInput struct {
	RoleName       string
	ActorType      string
	ActorID        string
	ScopeType      string
	ScopeID        *string
	OrganizationID *string
	CreatedBy      *string
	Now            time.Time
}

type ExternalGroupMappingCreateInput struct {
	ProviderID     string
	ExternalGroup  string
	RoleName       string
	ScopeType      string
	ScopeID        *string
	OrganizationID *string
	CreatedBy      *string
	Now            time.Time
}

type ACLAuditEventInput struct {
	EventType      string
	ActorUserID    *string
	OrganizationID *string
	SubjectType    string
	SubjectID      string
	Payload        map[string]any
}

type ACLAuditEventListFilter struct {
	EventType      string
	SubjectType    string
	OrganizationID string
	Limit          int
	Offset         int
}

func (s *Store) HasPermission(ctx context.Context, userID, orgID, permission string) (bool, error) {
	permission = strings.TrimSpace(permission)
	orgID = strings.TrimSpace(orgID)
	if permission == "" {
		return false, nil
	}
	if isPlatformPermission(permission) {
		isAdmin, err := s.IsPlatformAdmin(ctx, userID)
		if err == nil && isAdmin {
			return true, nil
		}
		if err != nil && err != ErrNotFound {
			return false, err
		}
	}

	var allowed bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM role_assignments ra
			JOIN roles r ON r.id = ra.role_id AND r.disabled_at IS NULL
			JOIN role_permissions rp ON rp.role_id = r.id
			JOIN permissions p ON p.id = rp.permission_id
			JOIN users u ON u.id::text = ra.actor_id AND u.disabled_at IS NULL
			WHERE ra.actor_type = 'user'
			  AND ra.actor_id = $1
			  AND ra.disabled_at IS NULL
			  AND p.name = $2
			  AND (
			      (ra.scope_type = 'platform' AND $3 = '')
			      OR
			      (ra.scope_type = 'organization' AND ra.scope_id = $3)
			  )
		)
	`, userID, permission, orgID).Scan(&allowed)
	return allowed, err
}

func (s *Store) ListUserPlatformPermissions(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT p.name
		FROM role_assignments ra
		JOIN roles r ON r.id = ra.role_id AND r.disabled_at IS NULL
		JOIN role_permissions rp ON rp.role_id = r.id
		JOIN permissions p ON p.id = rp.permission_id
		JOIN users u ON u.id::text = ra.actor_id AND u.disabled_at IS NULL
		WHERE ra.actor_type = 'user' AND ra.actor_id = $1
		  AND ra.scope_type = 'platform' AND ra.disabled_at IS NULL
		ORDER BY p.name
	`, strings.TrimSpace(userID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	permissions := []string{}
	for rows.Next() {
		var permission string
		if err := rows.Scan(&permission); err != nil {
			return nil, err
		}
		permissions = append(permissions, permission)
	}
	return permissions, rows.Err()
}

func (s *Store) HasBrandCloudPermission(ctx context.Context, brandCloudUserID, orgID, permission string) (bool, error) {
	return s.HasBrandCloudPermissionForResource(ctx, brandCloudUserID, orgID, permission, "", "")
}

func (s *Store) HasBrandCloudPermissionForResource(ctx context.Context, brandCloudUserID, orgID, permission, scopeType, scopeID string) (bool, error) {
	permission = strings.TrimSpace(permission)
	orgID = strings.TrimSpace(orgID)
	scopeType = strings.TrimSpace(scopeType)
	scopeID = strings.TrimSpace(scopeID)
	if permission == "" || orgID == "" {
		return false, nil
	}
	if isPlatformPermission(permission) && scopeType == "" {
		return false, nil
	}

	var allowed bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM role_assignments ra
			JOIN roles r ON r.id = ra.role_id AND r.disabled_at IS NULL
			JOIN role_permissions rp ON rp.role_id = r.id
			JOIN permissions p ON p.id = rp.permission_id
			JOIN brand_cloud_users bcu ON bcu.id::text = ra.actor_id AND bcu.disabled_at IS NULL
			WHERE ra.actor_type = 'brand_cloud_user'
			  AND ra.actor_id = $1
			  AND ra.disabled_at IS NULL
			  AND p.name = $2
			  AND ra.organization_id::text = $3
			  AND bcu.brand_cloud_id::text = $3
			  AND (
			      ra.scope_type = 'organization'
			      OR ($4 <> '' AND $5 <> '' AND ra.scope_type = $4 AND ra.scope_id = $5)
			  )
		)
	`, brandCloudUserID, permission, orgID, scopeType, scopeID).Scan(&allowed)
	return allowed, err
}

func (s *Store) HasBrandCloudPermissionAnyResource(ctx context.Context, brandCloudUserID, orgID, permission string) (bool, error) {
	var allowed bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM role_assignments ra
			JOIN roles r ON r.id = ra.role_id AND r.disabled_at IS NULL
			JOIN role_permissions rp ON rp.role_id = r.id
			JOIN permissions p ON p.id = rp.permission_id AND p.name = $3
			JOIN brand_cloud_users bcu ON bcu.id::text = ra.actor_id AND bcu.disabled_at IS NULL
			WHERE ra.actor_type = 'brand_cloud_user' AND ra.actor_id = $1
			  AND ra.organization_id::text = $2 AND bcu.brand_cloud_id::text = $2 AND ra.disabled_at IS NULL
		)
	`, strings.TrimSpace(brandCloudUserID), strings.TrimSpace(orgID), strings.TrimSpace(permission)).Scan(&allowed)
	return allowed, err
}

func (s *Store) HasBrandCloudDevicePermission(ctx context.Context, brandCloudUserID, orgID, permission, deviceID string) (bool, error) {
	permission = strings.TrimSpace(permission)
	orgID = strings.TrimSpace(orgID)
	brandCloudUserID = strings.TrimSpace(brandCloudUserID)
	deviceID = strings.TrimSpace(deviceID)
	if permission == "" || orgID == "" || brandCloudUserID == "" || deviceID == "" {
		return false, nil
	}
	var allowed bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM devices d
			JOIN brand_cloud_users bcu ON bcu.id::text = $1 AND bcu.brand_cloud_id = d.organization_id AND bcu.disabled_at IS NULL
			JOIN role_assignments ra ON ra.actor_type = 'brand_cloud_user' AND ra.actor_id = bcu.id::text AND ra.organization_id = d.organization_id AND ra.disabled_at IS NULL
			JOIN roles r ON r.id = ra.role_id AND r.disabled_at IS NULL
			JOIN role_permissions rp ON rp.role_id = r.id
			JOIN permissions p ON p.id = rp.permission_id AND p.name = $3
			WHERE d.id::text = $2 AND d.organization_id::text = $4
			  AND (
			    ra.scope_type = 'organization'
			    OR (ra.scope_type = 'sku' AND ra.scope_id = d.device_item_profile_id::text)
			    OR (ra.scope_type = 'region' AND ra.scope_id = COALESCE(NULLIF(d.metadata ->> 'region', ''), '未設定'))
			    OR (ra.scope_type = 'device' AND ra.scope_id = d.id::text)
			    OR (ra.scope_type = 'group' AND EXISTS (SELECT 1 FROM device_group_members dgm WHERE dgm.device_id = d.id AND dgm.group_id::text = ra.scope_id))
			  )
		)
	`, brandCloudUserID, deviceID, permission, orgID).Scan(&allowed)
	return allowed, err
}

func isPlatformPermission(permission string) bool {
	return permission == "quota_request.read" ||
		permission == "quota_request.approve" ||
		permission == "quota_request.decline" ||
		permission == "platform_metrics.read" ||
		permission == "device.unprovision_override" ||
		permission == "acl.read" ||
		permission == "acl.manage" ||
		permission == PermissionChipsetProviderRead ||
		permission == PermissionChipsetProviderEdit ||
		permission == PermissionChipsetProviderPublish
}

func (s *Store) ListPermissions(ctx context.Context, limit, offset int) (PermissionPage, error) {
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*)::int FROM permissions`).Scan(&total); err != nil {
		return PermissionPage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT id::text, name, domain, action, description, created_at, updated_at
		FROM permissions
		ORDER BY name ASC
		LIMIT $1 OFFSET $2
	`, limit, offset)
	if err != nil {
		return PermissionPage{}, err
	}
	defer rows.Close()

	permissions := []model.Permission{}
	for rows.Next() {
		var p model.Permission
		if err := rows.Scan(&p.ID, &p.Name, &p.Domain, &p.Action, &p.Description, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return PermissionPage{}, err
		}
		permissions = append(permissions, p)
	}
	if err := rows.Err(); err != nil {
		return PermissionPage{}, err
	}
	return PermissionPage{Permissions: permissions, Page: Page{Limit: limit, Offset: offset, Total: total}}, nil
}

func (s *Store) GetRoleByName(ctx context.Context, name string) (model.ProductRole, error) {
	var role model.ProductRole
	err := s.db.QueryRow(ctx, `
		SELECT id::text, name, scope_type, description, system_role, created_at, updated_at, disabled_at
		FROM roles
		WHERE name = $1 AND disabled_at IS NULL
	`, strings.TrimSpace(name)).Scan(&role.ID, &role.Name, &role.ScopeType, &role.Description, &role.SystemRole, &role.CreatedAt, &role.UpdatedAt, &role.DisabledAt)
	if err == pgx.ErrNoRows {
		return model.ProductRole{}, ErrNotFound
	}
	return role, err
}

func (s *Store) UpdateRole(ctx context.Context, in RoleUpdateInput) (model.ProductRole, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.ProductRole{}, err
	}
	defer tx.Rollback(ctx)
	var role model.ProductRole
	err = tx.QueryRow(ctx, `
		UPDATE roles
		SET description = $2
		WHERE name = $1 AND disabled_at IS NULL
		RETURNING id::text, name, scope_type, description, system_role, created_at, updated_at, disabled_at
	`, strings.TrimSpace(in.Name), in.Description).Scan(&role.ID, &role.Name, &role.ScopeType, &role.Description, &role.SystemRole, &role.CreatedAt, &role.UpdatedAt, &role.DisabledAt)
	if err == pgx.ErrNoRows {
		return model.ProductRole{}, ErrNotFound
	}
	if err != nil {
		return model.ProductRole{}, err
	}
	if err := createACLAuditEventTx(ctx, tx, ACLAuditEventInput{
		EventType:   "role_updated",
		ActorUserID: in.ActorUserID,
		SubjectType: "role",
		SubjectID:   role.ID,
		Payload:     map[string]any{"role": role.Name},
	}); err != nil {
		return model.ProductRole{}, err
	}
	return role, tx.Commit(ctx)
}

func (s *Store) DisableRole(ctx context.Context, name string, actorUserID *string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var roleID string
	err = tx.QueryRow(ctx, `
		UPDATE roles
		SET disabled_at = now()
		WHERE name = $1 AND disabled_at IS NULL
		RETURNING id::text
	`, strings.TrimSpace(name)).Scan(&roleID)
	if err == pgx.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := createACLAuditEventTx(ctx, tx, ACLAuditEventInput{
		EventType:   "role_disabled",
		ActorUserID: actorUserID,
		SubjectType: "role",
		SubjectID:   roleID,
		Payload:     map[string]any{"role": strings.TrimSpace(name)},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ListRoles(ctx context.Context, limit, offset int) (ProductRolePage, error) {
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*)::int FROM roles WHERE disabled_at IS NULL`).Scan(&total); err != nil {
		return ProductRolePage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT id::text, name, scope_type, description, system_role, created_at, updated_at, disabled_at
		FROM roles
		WHERE disabled_at IS NULL
		ORDER BY name ASC
		LIMIT $1 OFFSET $2
	`, limit, offset)
	if err != nil {
		return ProductRolePage{}, err
	}
	defer rows.Close()
	roles := []model.ProductRole{}
	for rows.Next() {
		var role model.ProductRole
		if err := rows.Scan(&role.ID, &role.Name, &role.ScopeType, &role.Description, &role.SystemRole, &role.CreatedAt, &role.UpdatedAt, &role.DisabledAt); err != nil {
			return ProductRolePage{}, err
		}
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		return ProductRolePage{}, err
	}
	return ProductRolePage{Roles: roles, Page: Page{Limit: limit, Offset: offset, Total: total}}, nil
}

func (s *Store) CreateRole(ctx context.Context, in RoleCreateInput) (model.ProductRole, error) {
	var role model.ProductRole
	err := s.db.QueryRow(ctx, `
		INSERT INTO roles (name, scope_type, description, system_role)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text, name, scope_type, description, system_role, created_at, updated_at, disabled_at
	`, strings.TrimSpace(in.Name), strings.TrimSpace(in.ScopeType), in.Description, in.SystemRole).
		Scan(&role.ID, &role.Name, &role.ScopeType, &role.Description, &role.SystemRole, &role.CreatedAt, &role.UpdatedAt, &role.DisabledAt)
	if err != nil {
		return model.ProductRole{}, err
	}
	return role, nil
}

func (s *Store) BindRolePermission(ctx context.Context, roleName, permissionName string, actorUserID *string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var roleID, permissionID string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM roles WHERE name = $1 AND disabled_at IS NULL`, strings.TrimSpace(roleName)).Scan(&roleID); err != nil {
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT id::text FROM permissions WHERE name = $1`, strings.TrimSpace(permissionName)).Scan(&permissionID); err != nil {
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO role_permissions (role_id, permission_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, roleID, permissionID); err != nil {
		return err
	}
	if err := createACLAuditEventTx(ctx, tx, ACLAuditEventInput{
		EventType:   "role_permission_bound",
		ActorUserID: actorUserID,
		SubjectType: "role",
		SubjectID:   roleID,
		Payload:     map[string]any{"permission_id": permissionID, "permission": permissionName},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) CreateRoleAssignment(ctx context.Context, in RoleAssignmentCreateInput) (model.RoleAssignment, error) {
	now := defaultTime(in.Now)
	role, err := s.GetRoleByName(ctx, in.RoleName)
	if err != nil {
		return model.RoleAssignment{}, err
	}
	scopeID, orgID := normalizeScope(in.ScopeType, in.ScopeID, in.OrganizationID)
	var assignment model.RoleAssignment
	err = s.db.QueryRow(ctx, `
		INSERT INTO role_assignments (role_id, actor_type, actor_id, scope_type, scope_id, organization_id, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
		RETURNING id::text, role_id::text, actor_type, actor_id, scope_type, scope_id, organization_id::text, created_by::text, created_at, updated_at, disabled_at
	`, role.ID, defaultString(in.ActorType, ActorTypeUser), strings.TrimSpace(in.ActorID), strings.TrimSpace(in.ScopeType), scopeID, orgID, in.CreatedBy, now).
		Scan(&assignment.ID, &assignment.RoleID, &assignment.ActorType, &assignment.ActorID, &assignment.ScopeType, &assignment.ScopeID, &assignment.OrganizationID, &assignment.CreatedBy, &assignment.CreatedAt, &assignment.UpdatedAt, &assignment.DisabledAt)
	if err != nil {
		return model.RoleAssignment{}, err
	}
	assignment.RoleName = role.Name
	if err := s.CreateACLAuditEvent(ctx, ACLAuditEventInput{
		EventType:      "role_assignment_created",
		ActorUserID:    in.CreatedBy,
		OrganizationID: orgID,
		SubjectType:    "role_assignment",
		SubjectID:      assignment.ID,
		Payload:        map[string]any{"role": role.Name, "actor_type": assignment.ActorType, "actor_id": assignment.ActorID, "scope_type": assignment.ScopeType, "scope_id": assignment.ScopeID},
	}); err != nil {
		return model.RoleAssignment{}, err
	}
	return assignment, nil
}

func (s *Store) ListRoleAssignments(ctx context.Context, limit, offset int) (RoleAssignmentPage, error) {
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*)::int FROM role_assignments WHERE disabled_at IS NULL`).Scan(&total); err != nil {
		return RoleAssignmentPage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT ra.id::text, ra.role_id::text, r.name, ra.actor_type, ra.actor_id, ra.scope_type, ra.scope_id, ra.organization_id::text, ra.created_by::text, ra.created_at, ra.updated_at, ra.disabled_at
		FROM role_assignments ra
		JOIN roles r ON r.id = ra.role_id
		WHERE ra.disabled_at IS NULL
		ORDER BY ra.created_at ASC, ra.id ASC
		LIMIT $1 OFFSET $2
	`, limit, offset)
	if err != nil {
		return RoleAssignmentPage{}, err
	}
	defer rows.Close()
	assignments := []model.RoleAssignment{}
	for rows.Next() {
		var assignment model.RoleAssignment
		if err := rows.Scan(&assignment.ID, &assignment.RoleID, &assignment.RoleName, &assignment.ActorType, &assignment.ActorID, &assignment.ScopeType, &assignment.ScopeID, &assignment.OrganizationID, &assignment.CreatedBy, &assignment.CreatedAt, &assignment.UpdatedAt, &assignment.DisabledAt); err != nil {
			return RoleAssignmentPage{}, err
		}
		assignments = append(assignments, assignment)
	}
	if err := rows.Err(); err != nil {
		return RoleAssignmentPage{}, err
	}
	return RoleAssignmentPage{Assignments: assignments, Page: Page{Limit: limit, Offset: offset, Total: total}}, nil
}

func (s *Store) ListRoleAssignmentsForOrganization(ctx context.Context, organizationID string, limit, offset int) (RoleAssignmentPage, error) {
	organizationID = strings.TrimSpace(organizationID)
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*)::int FROM role_assignments WHERE disabled_at IS NULL AND organization_id::text = $1`, organizationID).Scan(&total); err != nil {
		return RoleAssignmentPage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT ra.id::text, ra.role_id::text, r.name, ra.actor_type, ra.actor_id, ra.scope_type, ra.scope_id, ra.organization_id::text, ra.created_by::text, ra.created_at, ra.updated_at, ra.disabled_at
		FROM role_assignments ra
		JOIN roles r ON r.id = ra.role_id
		WHERE ra.disabled_at IS NULL AND ra.organization_id::text = $1
		ORDER BY ra.created_at ASC, ra.id ASC
		LIMIT $2 OFFSET $3
	`, organizationID, limit, offset)
	if err != nil {
		return RoleAssignmentPage{}, err
	}
	defer rows.Close()
	assignments := []model.RoleAssignment{}
	for rows.Next() {
		var assignment model.RoleAssignment
		if err := rows.Scan(&assignment.ID, &assignment.RoleID, &assignment.RoleName, &assignment.ActorType, &assignment.ActorID, &assignment.ScopeType, &assignment.ScopeID, &assignment.OrganizationID, &assignment.CreatedBy, &assignment.CreatedAt, &assignment.UpdatedAt, &assignment.DisabledAt); err != nil {
			return RoleAssignmentPage{}, err
		}
		assignments = append(assignments, assignment)
	}
	if err := rows.Err(); err != nil {
		return RoleAssignmentPage{}, err
	}
	return RoleAssignmentPage{Assignments: assignments, Page: Page{Limit: limit, Offset: offset, Total: total}}, nil
}

func (s *Store) DisableRoleAssignment(ctx context.Context, assignmentID string, actorUserID *string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var orgID *string
	err = tx.QueryRow(ctx, `
		UPDATE role_assignments
		SET disabled_at = now()
		WHERE id::text = $1 AND disabled_at IS NULL
		RETURNING organization_id::text
	`, strings.TrimSpace(assignmentID)).Scan(&orgID)
	if err == pgx.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := createACLAuditEventTx(ctx, tx, ACLAuditEventInput{
		EventType:      "role_assignment_disabled",
		ActorUserID:    actorUserID,
		OrganizationID: orgID,
		SubjectType:    "role_assignment",
		SubjectID:      strings.TrimSpace(assignmentID),
		Payload:        map[string]any{},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) DisableRoleAssignmentForOrganization(ctx context.Context, organizationID, assignmentID string, actorUserID *string) error {
	organizationID = strings.TrimSpace(organizationID)
	assignmentID = strings.TrimSpace(assignmentID)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var orgID *string
	err = tx.QueryRow(ctx, `
		UPDATE role_assignments
		SET disabled_at = now()
		WHERE id::text = $1 AND organization_id::text = $2 AND disabled_at IS NULL
		RETURNING organization_id::text
	`, assignmentID, organizationID).Scan(&orgID)
	if err == pgx.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := createACLAuditEventTx(ctx, tx, ACLAuditEventInput{
		EventType: "role_assignment_disabled", ActorUserID: actorUserID, OrganizationID: orgID,
		SubjectType: "role_assignment", SubjectID: assignmentID,
		Payload: map[string]any{"organization_id": organizationID},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) CreateExternalGroupMapping(ctx context.Context, in ExternalGroupMappingCreateInput) (model.ExternalGroupMapping, error) {
	now := defaultTime(in.Now)
	role, err := s.GetRoleByName(ctx, in.RoleName)
	if err != nil {
		return model.ExternalGroupMapping{}, err
	}
	scopeID, orgID := normalizeScope(in.ScopeType, in.ScopeID, in.OrganizationID)
	var mapping model.ExternalGroupMapping
	err = s.db.QueryRow(ctx, `
		INSERT INTO external_group_mappings (provider_id, external_group, role_id, scope_type, scope_id, organization_id, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
		RETURNING id::text, provider_id, external_group, role_id::text, scope_type, scope_id, organization_id::text, created_by::text, created_at, updated_at, disabled_at
	`, strings.TrimSpace(in.ProviderID), strings.TrimSpace(in.ExternalGroup), role.ID, strings.TrimSpace(in.ScopeType), scopeID, orgID, in.CreatedBy, now).
		Scan(&mapping.ID, &mapping.ProviderID, &mapping.ExternalGroup, &mapping.RoleID, &mapping.ScopeType, &mapping.ScopeID, &mapping.OrganizationID, &mapping.CreatedBy, &mapping.CreatedAt, &mapping.UpdatedAt, &mapping.DisabledAt)
	if err != nil {
		return model.ExternalGroupMapping{}, err
	}
	mapping.RoleName = role.Name
	if err := s.CreateACLAuditEvent(ctx, ACLAuditEventInput{
		EventType:      "external_group_mapping_created",
		ActorUserID:    in.CreatedBy,
		OrganizationID: orgID,
		SubjectType:    "external_group_mapping",
		SubjectID:      mapping.ID,
		Payload:        map[string]any{"provider_id": mapping.ProviderID, "external_group": mapping.ExternalGroup, "role": role.Name},
	}); err != nil {
		return model.ExternalGroupMapping{}, err
	}
	return mapping, nil
}

func (s *Store) ListExternalGroupMappings(ctx context.Context, limit, offset int) (ExternalGroupMappingPage, error) {
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*)::int FROM external_group_mappings WHERE disabled_at IS NULL`).Scan(&total); err != nil {
		return ExternalGroupMappingPage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT egm.id::text, egm.provider_id, egm.external_group, egm.role_id::text, r.name, egm.scope_type, egm.scope_id, egm.organization_id::text, egm.created_by::text, egm.created_at, egm.updated_at, egm.disabled_at
		FROM external_group_mappings egm
		JOIN roles r ON r.id = egm.role_id
		WHERE egm.disabled_at IS NULL
		ORDER BY egm.created_at ASC, egm.id ASC
		LIMIT $1 OFFSET $2
	`, limit, offset)
	if err != nil {
		return ExternalGroupMappingPage{}, err
	}
	defer rows.Close()
	mappings := []model.ExternalGroupMapping{}
	for rows.Next() {
		var mapping model.ExternalGroupMapping
		if err := rows.Scan(&mapping.ID, &mapping.ProviderID, &mapping.ExternalGroup, &mapping.RoleID, &mapping.RoleName, &mapping.ScopeType, &mapping.ScopeID, &mapping.OrganizationID, &mapping.CreatedBy, &mapping.CreatedAt, &mapping.UpdatedAt, &mapping.DisabledAt); err != nil {
			return ExternalGroupMappingPage{}, err
		}
		mappings = append(mappings, mapping)
	}
	if err := rows.Err(); err != nil {
		return ExternalGroupMappingPage{}, err
	}
	return ExternalGroupMappingPage{Mappings: mappings, Page: Page{Limit: limit, Offset: offset, Total: total}}, nil
}

func (s *Store) DisableExternalGroupMapping(ctx context.Context, mappingID string, actorUserID *string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var orgID *string
	err = tx.QueryRow(ctx, `
		UPDATE external_group_mappings
		SET disabled_at = now()
		WHERE id::text = $1 AND disabled_at IS NULL
		RETURNING organization_id::text
	`, strings.TrimSpace(mappingID)).Scan(&orgID)
	if err == pgx.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := createACLAuditEventTx(ctx, tx, ACLAuditEventInput{
		EventType:      "external_group_mapping_disabled",
		ActorUserID:    actorUserID,
		OrganizationID: orgID,
		SubjectType:    "external_group_mapping",
		SubjectID:      strings.TrimSpace(mappingID),
		Payload:        map[string]any{},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ApplyExternalGroupMappings(ctx context.Context, userID, providerID string, groups []string, now time.Time) error {
	if len(groups) == 0 {
		return nil
	}
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		rows, err := s.db.Query(ctx, `
			SELECT r.name, egm.scope_type, egm.scope_id, egm.organization_id::text
			FROM external_group_mappings egm
			JOIN roles r ON r.id = egm.role_id AND r.disabled_at IS NULL
			WHERE egm.provider_id = $1
			  AND egm.external_group = $2
			  AND egm.disabled_at IS NULL
		`, providerID, group)
		if err != nil {
			return err
		}
		for rows.Next() {
			var roleName, scopeType string
			var scopeID, orgID *string
			if err := rows.Scan(&roleName, &scopeType, &scopeID, &orgID); err != nil {
				rows.Close()
				return err
			}
			if _, err := s.CreateRoleAssignment(ctx, RoleAssignmentCreateInput{
				RoleName:       roleName,
				ActorType:      ActorTypeUser,
				ActorID:        userID,
				ScopeType:      scopeType,
				ScopeID:        scopeID,
				OrganizationID: orgID,
				Now:            now,
			}); err != nil {
				rows.Close()
				if !isUniqueViolation(err) {
					return err
				}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return nil
}

func (s *Store) CreateACLAuditEvent(ctx context.Context, in ACLAuditEventInput) error {
	payload := []byte(`{}`)
	if len(in.Payload) > 0 {
		raw, err := json.Marshal(in.Payload)
		if err != nil {
			return err
		}
		payload = raw
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO acl_audit_events (event_type, actor_user_id, organization_id, subject_type, subject_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, in.EventType, in.ActorUserID, in.OrganizationID, in.SubjectType, in.SubjectID, payload)
	return err
}

func createACLAuditEventTx(ctx context.Context, tx pgx.Tx, in ACLAuditEventInput) error {
	payload := []byte(`{}`)
	if len(in.Payload) > 0 {
		raw, err := json.Marshal(in.Payload)
		if err != nil {
			return err
		}
		payload = raw
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO acl_audit_events (event_type, actor_user_id, organization_id, subject_type, subject_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, in.EventType, in.ActorUserID, in.OrganizationID, in.SubjectType, in.SubjectID, payload)
	return err
}

func (s *Store) ListACLAuditEvents(ctx context.Context, in ACLAuditEventListFilter) (ACLAuditEventPage, error) {
	var total int
	if err := s.db.QueryRow(ctx, `
		SELECT count(*)::int
		FROM acl_audit_events
		WHERE ($1 = '' OR event_type = $1)
		  AND ($2 = '' OR subject_type = $2)
		  AND ($3 = '' OR organization_id::text = $3)
	`, in.EventType, in.SubjectType, in.OrganizationID).Scan(&total); err != nil {
		return ACLAuditEventPage{}, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT id::text, event_type, actor_user_id::text, organization_id::text, subject_type, subject_id, payload, created_at, updated_at
		FROM acl_audit_events
		WHERE ($1 = '' OR event_type = $1)
		  AND ($2 = '' OR subject_type = $2)
		  AND ($3 = '' OR organization_id::text = $3)
		ORDER BY created_at ASC
		LIMIT $4 OFFSET $5
	`, in.EventType, in.SubjectType, in.OrganizationID, in.Limit, in.Offset)
	if err != nil {
		return ACLAuditEventPage{}, err
	}
	defer rows.Close()
	events := []model.ACLAuditEvent{}
	for rows.Next() {
		var event model.ACLAuditEvent
		var raw []byte
		if err := rows.Scan(&event.ID, &event.EventType, &event.ActorUserID, &event.OrganizationID, &event.SubjectType, &event.SubjectID, &raw, &event.CreatedAt, &event.UpdatedAt); err != nil {
			return ACLAuditEventPage{}, err
		}
		if err := json.Unmarshal(raw, &event.Payload); err != nil {
			return ACLAuditEventPage{}, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return ACLAuditEventPage{}, err
	}
	return ACLAuditEventPage{Events: events, Page: Page{Limit: in.Limit, Offset: in.Offset, Total: total}}, nil
}

func normalizeScope(scopeType string, scopeID, orgID *string) (*string, *string) {
	if scopeType == ScopeTypePlatform {
		return nil, nil
	}
	if scopeType != ScopeTypeOrganization && scopeType != ScopeTypeSKU && scopeType != ScopeTypeRegion && scopeType != ScopeTypeGroup && scopeType != ScopeTypeDevice {
		return scopeID, orgID
	}
	if orgID == nil && scopeID != nil {
		if scopeType == ScopeTypeOrganization {
			orgID = scopeID
		}
	}
	if scopeType == ScopeTypeOrganization && scopeID == nil && orgID != nil {
		scopeID = orgID
	}
	return scopeID, orgID
}

func defaultTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
