WITH role_permission_names(role_name, permission_name) AS (
    VALUES
    ('member', 'lifecycle_operation.provision'),
    ('member', 'lifecycle_operation.deactivate')
)
INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM role_permission_names rpm
JOIN roles r ON r.name = rpm.role_name AND r.disabled_at IS NULL
JOIN permissions p ON p.name = rpm.permission_name
ON CONFLICT DO NOTHING;
