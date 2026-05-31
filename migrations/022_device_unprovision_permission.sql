INSERT INTO permissions (name, domain, action, description)
VALUES
    ('device.unprovision', 'device', 'unprovision', 'Release a normal device from its organization binding for resale or re-onboarding'),
    ('device.unprovision_override', 'device', 'unprovision_override', 'Platform-admin override to release a normal device binding with reason, evidence, and audit')
ON CONFLICT (name) DO UPDATE
SET domain = EXCLUDED.domain,
    action = EXCLUDED.action,
    description = EXCLUDED.description;

WITH role_permission_names(role_name, permission_name) AS (
    VALUES
    ('owner', 'device.unprovision'),
    ('admin', 'device.unprovision'),
    ('member', 'device.unprovision'),
    ('tenant_admin', 'device.unprovision'),
    ('fleet_manager', 'device.unprovision'),
    ('platform_admin', 'device.unprovision_override')
)
INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM role_permission_names rpm
JOIN roles r ON r.name = rpm.role_name AND r.disabled_at IS NULL
JOIN permissions p ON p.name = rpm.permission_name
ON CONFLICT DO NOTHING;

ALTER TABLE device_operations
    DROP CONSTRAINT IF EXISTS device_operations_device_id_fkey;

ALTER TABLE device_operations
    DROP CONSTRAINT IF EXISTS device_operations_org_device_fkey;

ALTER TABLE device_operations
    DROP CONSTRAINT IF EXISTS device_operations_operation_type_check;

ALTER TABLE device_operations
    ADD CONSTRAINT device_operations_operation_type_check
    CHECK (operation_type IN ('provision', 'deactivate', 'unprovision'));

ALTER TABLE device_message_outbox
    DROP CONSTRAINT IF EXISTS device_message_outbox_message_type_check;

ALTER TABLE device_message_outbox
    ADD CONSTRAINT device_message_outbox_message_type_check
    CHECK (message_type IN ('DeviceProvisionRequested', 'DeviceDeactivateRequested', 'DeviceUnprovisionRequested'));

ALTER TABLE device_message_inbox
    DROP CONSTRAINT IF EXISTS device_message_inbox_message_type_check;

ALTER TABLE device_message_inbox
    ADD CONSTRAINT device_message_inbox_message_type_check
    CHECK (message_type IN ('DeviceProvisionSucceeded', 'DeviceProvisionFailed', 'DeviceDeactivateSucceeded', 'DeviceDeactivateFailed', 'DeviceUnprovisionSucceeded', 'DeviceUnprovisionFailed', 'DeviceOnlineChanged', 'DeviceMetadataChanged'));
