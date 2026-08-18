INSERT INTO permissions (name, domain, action, description)
VALUES
    ('billing_account.read', 'billing_account', 'read', 'Read the organization commercial account and projected balance'),
    ('billing_ledger.read', 'billing_ledger', 'read', 'Read immutable organization balance ledger entries'),
    ('payment_method.read', 'payment_method', 'read', 'Read redacted payment method metadata'),
    ('payment_method.manage', 'payment_method', 'manage', 'Create or revoke organization payment methods and consent'),
    ('payment_intent.read', 'payment_intent', 'read', 'Read customer-safe payment intent and attempt state'),
    ('payment_intent.create', 'payment_intent', 'create', 'Create an idempotent manual top-up intent'),
    ('auto_topup.read', 'auto_topup', 'read', 'Read automatic top-up policy and safety state'),
    ('auto_topup.manage', 'auto_topup', 'manage', 'Create, replace, or disable an automatic top-up policy'),
    ('payment_reconciliation.operate', 'payment_reconciliation', 'operate', 'Operate trusted payment reconciliation jobs')
ON CONFLICT (name) DO UPDATE
SET domain = EXCLUDED.domain,
    action = EXCLUDED.action,
    description = EXCLUDED.description;

WITH role_permission_names(role_name, permission_name) AS (
    VALUES
        ('owner', 'billing_account.read'),
        ('owner', 'billing_ledger.read'),
        ('owner', 'payment_method.read'),
        ('owner', 'payment_method.manage'),
        ('owner', 'payment_intent.read'),
        ('owner', 'payment_intent.create'),
        ('owner', 'auto_topup.read'),
        ('owner', 'auto_topup.manage'),
        ('service_integration', 'payment_reconciliation.operate')
)
INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM role_permission_names names
JOIN roles r ON r.name = names.role_name
JOIN permissions p ON p.name = names.permission_name
ON CONFLICT (role_id, permission_id) DO NOTHING;
