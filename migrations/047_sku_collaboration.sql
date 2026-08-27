ALTER TABLE roles DROP CONSTRAINT IF EXISTS roles_scope_type_check;
ALTER TABLE roles ADD CONSTRAINT roles_scope_type_check
    CHECK (scope_type IN ('platform', 'organization', 'sku'));

INSERT INTO permissions (name, domain, action, description)
VALUES
    ('sku.create', 'sku', 'create', 'Create a SKU project in a brand cloud'),
    ('sku_collaborator.manage', 'sku_collaborator', 'manage', 'Invite, update, and remove SKU collaborators'),
    ('sku_owner.transfer', 'sku_owner', 'transfer', 'Transfer explicit SKU project ownership')
ON CONFLICT (name) DO UPDATE
SET domain = EXCLUDED.domain, action = EXCLUDED.action, description = EXCLUDED.description;

INSERT INTO roles (name, scope_type, description, system_role)
VALUES
    ('sku_owner', 'sku', 'SKU project owner with full project and collaborator authority', true),
    ('sku_editor', 'sku', 'SKU project editor without collaborator or ownership authority', true),
    ('sku_viewer', 'sku', 'Read-only SKU project collaborator', true)
ON CONFLICT (name) DO UPDATE
SET scope_type = EXCLUDED.scope_type,
    description = EXCLUDED.description,
    system_role = true,
    disabled_at = NULL;

WITH role_permission_names(role_name, permission_name) AS (
    VALUES
        ('owner', 'sku.create'),
        ('owner', 'sku_collaborator.manage'),
        ('owner', 'sku_owner.transfer'),
        ('sku_owner', 'registry_device.read'),
        ('sku_owner', 'registry_device.manage'),
        ('sku_owner', 'device_group.read'),
        ('sku_owner', 'device_group.manage'),
        ('sku_owner', 'device_group.assign'),
        ('sku_owner', 'device_tag.read'),
        ('sku_owner', 'device_tag.assign'),
        ('sku_owner', 'lifecycle_operation.provision'),
        ('sku_owner', 'lifecycle_operation.deactivate'),
        ('sku_owner', 'lifecycle_operation.inspect'),
        ('sku_owner', 'sku_collaborator.manage'),
        ('sku_owner', 'sku_owner.transfer'),
        ('sku_editor', 'registry_device.read'),
        ('sku_editor', 'registry_device.manage'),
        ('sku_editor', 'device_group.read'),
        ('sku_editor', 'device_group.manage'),
        ('sku_editor', 'device_group.assign'),
        ('sku_editor', 'device_tag.read'),
        ('sku_editor', 'device_tag.assign'),
        ('sku_editor', 'lifecycle_operation.provision'),
        ('sku_editor', 'lifecycle_operation.deactivate'),
        ('sku_editor', 'lifecycle_operation.inspect'),
        ('sku_viewer', 'registry_device.read'),
        ('sku_viewer', 'device_group.read'),
        ('sku_viewer', 'device_tag.read'),
        ('sku_viewer', 'lifecycle_operation.inspect')
)
INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM role_permission_names rpm
JOIN roles r ON r.name = rpm.role_name
JOIN permissions p ON p.name = rpm.permission_name
ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS sku_collaborator_invitations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    brand_cloud_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    sku_id UUID NOT NULL REFERENCES device_item_profiles(id) ON DELETE CASCADE,
    invited_by_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    target_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    target_email TEXT NOT NULL,
    role TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL DEFAULT 'pending',
    expires_at TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    canceled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT sku_collaborator_invitations_role_check CHECK (role IN ('sku_editor', 'sku_viewer')),
    CONSTRAINT sku_collaborator_invitations_status_check CHECK (status IN ('pending', 'accepted', 'canceled', 'expired')),
    CONSTRAINT sku_collaborator_invitations_email_normalized CHECK (target_email = lower(btrim(target_email)))
);

CREATE UNIQUE INDEX IF NOT EXISTS sku_collaborator_invitations_pending_target_idx
    ON sku_collaborator_invitations (sku_id, target_user_id)
    WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS sku_collaborator_invitations_sku_status_idx
    ON sku_collaborator_invitations (sku_id, status, created_at DESC);

DROP TRIGGER IF EXISTS sku_collaborator_invitations_set_updated_at ON sku_collaborator_invitations;
CREATE TRIGGER sku_collaborator_invitations_set_updated_at
    BEFORE UPDATE ON sku_collaborator_invitations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE email_outbox DROP CONSTRAINT IF EXISTS email_outbox_message_type_check;
ALTER TABLE email_outbox ADD CONSTRAINT email_outbox_message_type_check
    CHECK (message_type IN (
        'email_verification', 'login_activation', 'password_reset',
        'brand_cloud_owner_transfer', 'brand_cloud_user_activation',
        'brand_cloud_membership_invitation', 'sku_collaborator_invitation',
        'quota_approved', 'quota_declined'
    ));

-- Preserve current access explicitly. Future SKUs receive only their creator's
-- owner assignment; ordinary Brand Cloud membership no longer implies access.
INSERT INTO role_assignments (role_id, actor_type, actor_id, scope_type, scope_id, organization_id)
SELECT target_role.id, 'user', om.user_id::text, 'sku', dip.id::text, om.organization_id
FROM organization_members om
JOIN organizations o ON o.id=om.organization_id AND o.organization_kind='brand_cloud'
JOIN device_item_profiles dip ON dip.brand_cloud_id=om.organization_id
JOIN roles target_role ON target_role.name=CASE om.role
    WHEN 'owner' THEN 'sku_owner' WHEN 'admin' THEN 'sku_editor' ELSE 'sku_viewer' END
WHERE om.disabled_at IS NULL
ON CONFLICT DO NOTHING;

-- Remove broad resource access from non-owner memberships after the backfill.
UPDATE role_assignments ra
SET disabled_at=now(), updated_at=now()
FROM organization_members om, organizations o
WHERE ra.actor_type='user' AND ra.actor_id=om.user_id::text
  AND ra.organization_id=om.organization_id AND o.id=om.organization_id
  AND o.organization_kind='brand_cloud' AND ra.scope_type='organization'
  AND om.role IN ('admin','member') AND ra.disabled_at IS NULL;

CREATE OR REPLACE FUNCTION sync_brand_cloud_membership_role_assignment()
RETURNS trigger AS $$
DECLARE
    target_role_id UUID;
BEGIN
    -- Tenant-local Brand Cloud users retain their operational role. Developer
    -- accounts use the user actor and are scoped explicitly per SKU.
    SELECT id INTO target_role_id FROM roles WHERE name = NEW.role AND disabled_at IS NULL;
    IF target_role_id IS NULL THEN
        RETURN NEW;
    END IF;
    UPDATE role_assignments
    SET disabled_at = now(), updated_at = now()
    WHERE actor_type = 'brand_cloud_user'
      AND actor_id = NEW.brand_cloud_user_id::text
      AND scope_type = 'organization'
      AND scope_id = NEW.brand_cloud_id::text
      AND disabled_at IS NULL
      AND role_id <> target_role_id;
    INSERT INTO role_assignments (role_id, actor_type, actor_id, scope_type, scope_id, organization_id)
    VALUES (target_role_id, 'brand_cloud_user', NEW.brand_cloud_user_id::text,
            'organization', NEW.brand_cloud_id::text, NEW.brand_cloud_id)
    ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION sync_membership_role_assignment()
RETURNS TRIGGER AS $$
DECLARE
    target_role_id UUID;
    target_org_id UUID;
    target_user_id UUID;
    target_org_kind TEXT;
BEGIN
    IF TG_OP='DELETE' THEN target_org_id:=OLD.organization_id; target_user_id:=OLD.user_id;
    ELSE target_org_id:=NEW.organization_id; target_user_id:=NEW.user_id; END IF;

    UPDATE role_assignments SET disabled_at=COALESCE(disabled_at,now()),updated_at=now()
    WHERE actor_type='user' AND actor_id=target_user_id::text AND scope_type='organization'
      AND scope_id=target_org_id::text AND role_id IN (SELECT id FROM roles WHERE name IN ('owner','admin','member'))
      AND disabled_at IS NULL;

    IF TG_OP<>'DELETE' THEN
        SELECT organization_kind INTO target_org_kind FROM organizations WHERE id=NEW.organization_id;
        -- Brand Cloud admin/member membership is identity only. The owner keeps
        -- the audited organization governance override; other access is per SKU.
        IF target_org_kind<>'brand_cloud' OR NEW.role='owner' THEN
            SELECT id INTO target_role_id FROM roles WHERE name=NEW.role AND disabled_at IS NULL;
            IF target_role_id IS NOT NULL THEN
                INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type,scope_id,organization_id)
                VALUES(target_role_id,'user',NEW.user_id::text,'organization',NEW.organization_id::text,NEW.organization_id)
                ON CONFLICT DO NOTHING;
            END IF;
        END IF;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
