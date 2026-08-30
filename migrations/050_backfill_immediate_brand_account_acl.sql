INSERT INTO role_assignments (
    role_id,
    actor_type,
    actor_id,
    scope_type,
    scope_id,
    organization_id,
    created_by
)
SELECT
    r.id,
    'user',
    om.user_id::text,
    'organization',
    om.organization_id::text,
    om.organization_id,
    ae.actor_user_id
FROM audit_events ae
JOIN organization_members om
  ON om.organization_id = ae.organization_id
 AND om.user_id::text = ae.subject_id
JOIN users u ON u.id = om.user_id
JOIN organizations o ON o.id = om.organization_id
JOIN roles r ON r.name = om.role AND r.disabled_at IS NULL
WHERE ae.event_type IN ('brand_cloud_account_created', 'brand_cloud_account_assigned')
  AND ae.subject_type = 'user'
  AND ae.payload->>'activation_mode' = 'immediate'
  AND om.disabled_at IS NULL
  AND u.disabled_at IS NULL
  AND u.email_verified
  AND NOT u.signup_pending_verification
  AND o.organization_kind = 'brand_cloud'
  AND o.status = 'active'
ON CONFLICT DO NOTHING;
