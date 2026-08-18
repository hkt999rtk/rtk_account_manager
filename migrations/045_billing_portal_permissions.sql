INSERT INTO permissions (name, domain, action, description)
VALUES
    ('billing_summary.read', 'billing_summary', 'read', 'Read balance, current usage, runway, and latest invoice summary'),
    ('billing_usage.read', 'billing_usage', 'read', 'Read priced organization usage estimates'),
    ('invoice.read', 'invoice', 'read', 'Read organization invoice metadata and line items'),
    ('invoice_document.read', 'invoice_document', 'read', 'Download immutable organization invoice documents'),
    ('billing_activity.read', 'billing_activity', 'read', 'Read normalized billing and payment activity'),
    ('billing_profile.read', 'billing_profile', 'read', 'Read organization billing profile'),
    ('billing_profile.manage', 'billing_profile', 'manage', 'Update organization billing profile'),
    ('billing_statement.export', 'billing_statement', 'export', 'Export organization billing statements')
ON CONFLICT (name) DO UPDATE
SET domain = EXCLUDED.domain,
    action = EXCLUDED.action,
    description = EXCLUDED.description;

WITH role_permission_names(role_name, permission_name) AS (
    VALUES
        ('owner', 'billing_summary.read'),
        ('owner', 'billing_usage.read'),
        ('owner', 'invoice.read'),
        ('owner', 'invoice_document.read'),
        ('owner', 'billing_activity.read'),
        ('owner', 'billing_profile.read'),
        ('owner', 'billing_profile.manage'),
        ('owner', 'billing_statement.export')
)
INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM role_permission_names names
JOIN roles r ON r.name = names.role_name
JOIN permissions p ON p.name = names.permission_name
ON CONFLICT (role_id, permission_id) DO NOTHING;
