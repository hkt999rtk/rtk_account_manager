ALTER TABLE role_assignments DROP CONSTRAINT IF EXISTS role_assignments_actor_type_check;
ALTER TABLE role_assignments ADD CONSTRAINT role_assignments_actor_type_check
    CHECK (actor_type IN ('user', 'brand_cloud_user', 'service_account'));

ALTER TABLE role_assignments DROP CONSTRAINT IF EXISTS role_assignments_scope_type_check;
ALTER TABLE role_assignments ADD CONSTRAINT role_assignments_scope_type_check
    CHECK (scope_type IN ('platform', 'organization', 'product', 'region', 'group', 'device'));

ALTER TABLE role_assignments DROP CONSTRAINT IF EXISTS role_assignments_scope_consistency;
ALTER TABLE role_assignments ADD CONSTRAINT role_assignments_scope_consistency CHECK (
    (scope_type = 'platform' AND scope_id IS NULL AND organization_id IS NULL)
    OR
    (scope_type = 'organization' AND scope_id IS NOT NULL AND organization_id IS NOT NULL AND scope_id = organization_id::text)
    OR
    (scope_type IN ('product', 'region', 'group', 'device') AND scope_id IS NOT NULL AND organization_id IS NOT NULL)
);

ALTER TABLE external_group_mappings DROP CONSTRAINT IF EXISTS external_group_mappings_scope_type_check;
ALTER TABLE external_group_mappings ADD CONSTRAINT external_group_mappings_scope_type_check
    CHECK (scope_type IN ('platform', 'organization', 'product', 'region', 'group', 'device'));

ALTER TABLE external_group_mappings DROP CONSTRAINT IF EXISTS external_group_mappings_scope_consistency;
ALTER TABLE external_group_mappings ADD CONSTRAINT external_group_mappings_scope_consistency CHECK (
    (scope_type = 'platform' AND scope_id IS NULL AND organization_id IS NULL)
    OR
    (scope_type = 'organization' AND scope_id IS NOT NULL AND organization_id IS NOT NULL AND scope_id = organization_id::text)
    OR
    (scope_type IN ('product', 'region', 'group', 'device') AND scope_id IS NOT NULL AND organization_id IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS role_assignments_resource_scope_idx
    ON role_assignments (organization_id, scope_type, scope_id, actor_type, actor_id)
    WHERE disabled_at IS NULL;

INSERT INTO permissions (name, domain, action, description)
VALUES
    ('role_assignment.read', 'role_assignment', 'read', 'Read roles and scoped access assignments'),
    ('role_assignment.manage', 'role_assignment', 'manage', 'Create and remove scoped access assignments')
ON CONFLICT (name) DO UPDATE
SET domain = EXCLUDED.domain, action = EXCLUDED.action, description = EXCLUDED.description;

WITH role_permission_names(role_name, permission_name) AS (
    VALUES
        ('owner', 'role_assignment.read'), ('owner', 'role_assignment.manage'),
        ('admin', 'role_assignment.read'), ('admin', 'role_assignment.manage'),
        ('tenant_admin', 'role_assignment.read'), ('tenant_admin', 'role_assignment.manage')
)
INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM role_permission_names rpm
JOIN roles r ON r.name = rpm.role_name
JOIN permissions p ON p.name = rpm.permission_name
ON CONFLICT DO NOTHING;
