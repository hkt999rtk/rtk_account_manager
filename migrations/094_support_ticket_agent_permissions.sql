-- Ticket permissions for the Realtek support queue. Brand Cloud customer
-- access is derived from current membership by Cloud Admin's BFF.
INSERT INTO permissions (name, domain, action, description) VALUES
    ('ticket.support.read', 'ticket.support', 'read', 'Read the Realtek support ticket queue'),
    ('ticket.support.reply', 'ticket.support', 'reply', 'Publicly reply and change support ticket state'),
    ('ticket.support.note', 'ticket.support', 'note', 'Add internal support ticket notes'),
    ('ticket.support.assign', 'ticket.support', 'assign', 'Claim an unassigned support ticket'),
    ('ticket.support.reassign', 'ticket.support', 'reassign', 'Assign a support ticket to another agent')
ON CONFLICT (name) DO UPDATE
SET domain=EXCLUDED.domain, action=EXCLUDED.action, description=EXCLUDED.description;

WITH grants(role_name, permission_name) AS (VALUES
    ('support_operator','ticket.support.read'),
    ('support_operator','ticket.support.reply'),
    ('support_operator','ticket.support.note'),
    ('support_operator','ticket.support.assign'),
    ('platform_admin','ticket.support.read'),
    ('platform_admin','ticket.support.reply'),
    ('platform_admin','ticket.support.note'),
    ('platform_admin','ticket.support.assign'),
    ('platform_admin','ticket.support.reassign')
)
INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id FROM grants g
JOIN roles r ON r.name=g.role_name
JOIN permissions p ON p.name=g.permission_name
ON CONFLICT DO NOTHING;
