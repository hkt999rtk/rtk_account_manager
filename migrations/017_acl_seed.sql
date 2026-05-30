INSERT INTO permissions (name, domain, action, description)
VALUES
    ('organization.read', 'organization', 'read', 'Read organization profile and settings'),
    ('organization.update', 'organization', 'update', 'Update organization profile and settings'),
    ('membership.read', 'membership', 'read', 'List organization members'),
    ('membership.manage', 'membership', 'manage', 'Add, remove, update, disable, or enable organization members'),
    ('registry_device.read', 'registry_device', 'read', 'Read registry device records'),
    ('registry_device.manage', 'registry_device', 'manage', 'Create, update, status update, or soft-disable registry devices'),
    ('device_group.read', 'device_group', 'read', 'Read device groups and group members'),
    ('device_group.manage', 'device_group', 'manage', 'Create, update, or delete device groups'),
    ('device_group.assign', 'device_group', 'assign', 'Attach or detach devices from groups'),
    ('device_tag.read', 'device_tag', 'read', 'Read device tags'),
    ('device_tag.assign', 'device_tag', 'assign', 'Attach or detach device tags'),
    ('claim.resolve', 'claim', 'resolve', 'Resolve claim material and bind registry facts'),
    ('lifecycle_operation.provision', 'lifecycle_operation', 'provision', 'Start or reuse provisioning'),
    ('lifecycle_operation.deactivate', 'lifecycle_operation', 'deactivate', 'Start or reuse deactivation'),
    ('lifecycle_operation.inspect', 'lifecycle_operation', 'inspect', 'Inspect provisioning and readiness state'),
    ('quota_request.create', 'quota_request', 'create', 'Create evaluation quota raise request'),
    ('quota_request.read', 'quota_request', 'read', 'Read quota raise requests'),
    ('quota_request.approve', 'quota_request', 'approve', 'Approve quota raise requests'),
    ('quota_request.decline', 'quota_request', 'decline', 'Decline quota raise requests'),
    ('platform_metrics.read', 'platform_metrics', 'read', 'Read platform operational metrics'),
    ('acl.read', 'acl', 'read', 'Read authorization catalog and ACL facts'),
    ('acl.manage', 'acl', 'manage', 'Manage roles, bindings, assignments, and mappings')
ON CONFLICT (name) DO UPDATE
SET domain = EXCLUDED.domain,
    action = EXCLUDED.action,
    description = EXCLUDED.description;

INSERT INTO roles (name, scope_type, description, system_role)
VALUES
    ('owner', 'organization', 'Legacy organization owner with full tenant authority', true),
    ('admin', 'organization', 'Legacy organization admin with operational tenant authority', true),
    ('member', 'organization', 'Legacy organization member with read-only tenant authority', true),
    ('platform_admin', 'platform', 'Platform operator for quota, metrics, and ACL administration', true),
    ('support_operator', 'platform', 'Support operator with read-only support visibility', true),
    ('tenant_admin', 'organization', 'Organization administrator managed by the product ACL catalog', true),
    ('fleet_manager', 'organization', 'Device fleet manager for registry and lifecycle operations', true),
    ('installer', 'organization', 'Installer allowed to claim and provision devices', true),
    ('firmware_operator', 'organization', 'Firmware and lifecycle operator', true),
    ('read_only_observer', 'organization', 'Read-only organization observer', true),
    ('end_user', 'organization', 'End-user read-only role', true),
    ('device_agent', 'organization', 'Device agent role for constrained device-side product facts', true),
    ('service_integration', 'platform', 'Trusted service integration role', true)
ON CONFLICT (name) DO UPDATE
SET scope_type = EXCLUDED.scope_type,
    description = EXCLUDED.description,
    system_role = true,
    disabled_at = NULL;

WITH role_permission_names(role_name, permission_name) AS (
    VALUES
    ('owner', 'organization.read'),
    ('owner', 'organization.update'),
    ('owner', 'membership.read'),
    ('owner', 'membership.manage'),
    ('owner', 'registry_device.read'),
    ('owner', 'registry_device.manage'),
    ('owner', 'device_group.read'),
    ('owner', 'device_group.manage'),
    ('owner', 'device_group.assign'),
    ('owner', 'device_tag.read'),
    ('owner', 'device_tag.assign'),
    ('owner', 'claim.resolve'),
    ('owner', 'lifecycle_operation.provision'),
    ('owner', 'lifecycle_operation.deactivate'),
    ('owner', 'lifecycle_operation.inspect'),
    ('owner', 'quota_request.create'),

    ('admin', 'organization.read'),
    ('admin', 'membership.read'),
    ('admin', 'registry_device.read'),
    ('admin', 'registry_device.manage'),
    ('admin', 'device_group.read'),
    ('admin', 'device_group.manage'),
    ('admin', 'device_group.assign'),
    ('admin', 'device_tag.read'),
    ('admin', 'device_tag.assign'),
    ('admin', 'claim.resolve'),
    ('admin', 'lifecycle_operation.provision'),
    ('admin', 'lifecycle_operation.deactivate'),
    ('admin', 'lifecycle_operation.inspect'),
    ('admin', 'quota_request.create'),

    ('member', 'organization.read'),
    ('member', 'membership.read'),
    ('member', 'registry_device.read'),
    ('member', 'device_group.read'),
    ('member', 'device_tag.read'),
    ('member', 'claim.resolve'),
    ('member', 'lifecycle_operation.provision'),
    ('member', 'lifecycle_operation.deactivate'),
    ('member', 'lifecycle_operation.inspect'),
    ('member', 'quota_request.create'),

    ('tenant_admin', 'organization.read'),
    ('tenant_admin', 'organization.update'),
    ('tenant_admin', 'membership.read'),
    ('tenant_admin', 'membership.manage'),
    ('tenant_admin', 'registry_device.read'),
    ('tenant_admin', 'registry_device.manage'),
    ('tenant_admin', 'device_group.read'),
    ('tenant_admin', 'device_group.manage'),
    ('tenant_admin', 'device_group.assign'),
    ('tenant_admin', 'device_tag.read'),
    ('tenant_admin', 'device_tag.assign'),
    ('tenant_admin', 'claim.resolve'),
    ('tenant_admin', 'lifecycle_operation.provision'),
    ('tenant_admin', 'lifecycle_operation.deactivate'),
    ('tenant_admin', 'lifecycle_operation.inspect'),
    ('tenant_admin', 'quota_request.create'),

    ('fleet_manager', 'organization.read'),
    ('fleet_manager', 'membership.read'),
    ('fleet_manager', 'registry_device.read'),
    ('fleet_manager', 'registry_device.manage'),
    ('fleet_manager', 'device_group.read'),
    ('fleet_manager', 'device_group.manage'),
    ('fleet_manager', 'device_group.assign'),
    ('fleet_manager', 'device_tag.read'),
    ('fleet_manager', 'device_tag.assign'),
    ('fleet_manager', 'claim.resolve'),
    ('fleet_manager', 'lifecycle_operation.provision'),
    ('fleet_manager', 'lifecycle_operation.deactivate'),
    ('fleet_manager', 'lifecycle_operation.inspect'),

    ('installer', 'organization.read'),
    ('installer', 'registry_device.read'),
    ('installer', 'claim.resolve'),
    ('installer', 'lifecycle_operation.provision'),
    ('installer', 'lifecycle_operation.inspect'),

    ('firmware_operator', 'organization.read'),
    ('firmware_operator', 'registry_device.read'),
    ('firmware_operator', 'lifecycle_operation.provision'),
    ('firmware_operator', 'lifecycle_operation.deactivate'),
    ('firmware_operator', 'lifecycle_operation.inspect'),

    ('read_only_observer', 'organization.read'),
    ('read_only_observer', 'membership.read'),
    ('read_only_observer', 'registry_device.read'),
    ('read_only_observer', 'device_group.read'),
    ('read_only_observer', 'device_tag.read'),
    ('read_only_observer', 'lifecycle_operation.inspect'),

    ('end_user', 'organization.read'),
    ('end_user', 'registry_device.read'),
    ('end_user', 'device_group.read'),
    ('end_user', 'device_tag.read'),
    ('end_user', 'lifecycle_operation.inspect'),

    ('device_agent', 'registry_device.read'),
    ('device_agent', 'lifecycle_operation.inspect'),

    ('platform_admin', 'quota_request.read'),
    ('platform_admin', 'quota_request.approve'),
    ('platform_admin', 'quota_request.decline'),
    ('platform_admin', 'platform_metrics.read'),
    ('platform_admin', 'acl.read'),
    ('platform_admin', 'acl.manage'),

    ('support_operator', 'quota_request.read'),
    ('support_operator', 'platform_metrics.read'),
    ('support_operator', 'acl.read'),

    ('service_integration', 'acl.read')
)
INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM role_permission_names rpm
JOIN roles r ON r.name = rpm.role_name
JOIN permissions p ON p.name = rpm.permission_name
ON CONFLICT DO NOTHING;

INSERT INTO role_assignments (role_id, actor_type, actor_id, scope_type, scope_id, organization_id)
SELECT r.id, 'user', m.user_id::text, 'organization', m.organization_id::text, m.organization_id
FROM organization_members m
JOIN users u ON u.id = m.user_id AND u.disabled_at IS NULL
JOIN roles r ON r.name = m.role
WHERE NOT EXISTS (
    SELECT 1
    FROM role_assignments ra
    WHERE ra.role_id = r.id
      AND ra.actor_type = 'user'
      AND ra.actor_id = m.user_id::text
      AND ra.scope_type = 'organization'
      AND ra.scope_id = m.organization_id::text
      AND ra.disabled_at IS NULL
);

INSERT INTO role_assignments (role_id, actor_type, actor_id, scope_type)
SELECT r.id, 'user', u.id::text, 'platform'
FROM users u
JOIN roles r ON r.name = 'platform_admin'
WHERE COALESCE(u.platform_admin, false)
  AND u.disabled_at IS NULL
  AND NOT EXISTS (
      SELECT 1
      FROM role_assignments ra
      WHERE ra.role_id = r.id
        AND ra.actor_type = 'user'
        AND ra.actor_id = u.id::text
        AND ra.scope_type = 'platform'
        AND ra.disabled_at IS NULL
  );

CREATE OR REPLACE FUNCTION sync_membership_role_assignment()
RETURNS TRIGGER AS $$
DECLARE
    target_role_id UUID;
    target_org_id UUID;
    target_user_id UUID;
BEGIN
    IF TG_OP = 'DELETE' THEN
        target_org_id := OLD.organization_id;
        target_user_id := OLD.user_id;
    ELSE
        target_org_id := NEW.organization_id;
        target_user_id := NEW.user_id;
    END IF;

    UPDATE role_assignments
    SET disabled_at = COALESCE(disabled_at, now())
    WHERE actor_type = 'user'
      AND actor_id = target_user_id::text
      AND scope_type = 'organization'
      AND scope_id = target_org_id::text
      AND role_id IN (SELECT id FROM roles WHERE name IN ('owner', 'admin', 'member'))
      AND disabled_at IS NULL;

    IF TG_OP <> 'DELETE' THEN
        SELECT id INTO target_role_id FROM roles WHERE name = NEW.role AND disabled_at IS NULL;
        IF target_role_id IS NOT NULL THEN
            INSERT INTO role_assignments (role_id, actor_type, actor_id, scope_type, scope_id, organization_id)
            VALUES (target_role_id, 'user', NEW.user_id::text, 'organization', NEW.organization_id::text, NEW.organization_id)
            ON CONFLICT DO NOTHING;
        END IF;
    END IF;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS organization_members_sync_acl_insert ON organization_members;
CREATE TRIGGER organization_members_sync_acl_insert
    AFTER INSERT ON organization_members
    FOR EACH ROW EXECUTE FUNCTION sync_membership_role_assignment();

DROP TRIGGER IF EXISTS organization_members_sync_acl_update ON organization_members;
CREATE TRIGGER organization_members_sync_acl_update
    AFTER UPDATE OF role ON organization_members
    FOR EACH ROW EXECUTE FUNCTION sync_membership_role_assignment();

DROP TRIGGER IF EXISTS organization_members_sync_acl_delete ON organization_members;
CREATE TRIGGER organization_members_sync_acl_delete
    AFTER DELETE ON organization_members
    FOR EACH ROW EXECUTE FUNCTION sync_membership_role_assignment();
