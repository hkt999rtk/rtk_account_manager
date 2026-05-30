ALTER TABLE device_claim_tokens
    ADD COLUMN IF NOT EXISTS service_options JSONB NOT NULL DEFAULT '[]'::jsonb;

WITH role_permission_names(role_name, permission_name) AS (
    VALUES
    ('member', 'claim.resolve'),
    ('member', 'lifecycle_operation.provision'),
    ('member', 'lifecycle_operation.deactivate'),
    ('member', 'lifecycle_operation.inspect')
)
INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM role_permission_names rpm
JOIN roles r ON r.name = rpm.role_name AND r.disabled_at IS NULL
JOIN permissions p ON p.name = rpm.permission_name
ON CONFLICT DO NOTHING;
