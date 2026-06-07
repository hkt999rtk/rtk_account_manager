ALTER TABLE organizations
    ADD COLUMN IF NOT EXISTS tenant_slug TEXT;

UPDATE organizations
SET tenant_slug = 'brand-' || substr(replace(id::text, '-', ''), 1, 8)
WHERE organization_kind = 'brand_cloud'
  AND (tenant_slug IS NULL OR btrim(tenant_slug) = '');

ALTER TABLE organizations
    DROP CONSTRAINT IF EXISTS organizations_brand_tenant_slug_not_blank;

ALTER TABLE organizations
    ADD CONSTRAINT organizations_brand_tenant_slug_not_blank CHECK (
        organization_kind <> 'brand_cloud'
        OR (tenant_slug IS NOT NULL AND btrim(tenant_slug) <> '' AND tenant_slug = lower(btrim(tenant_slug)))
    );

CREATE UNIQUE INDEX IF NOT EXISTS organizations_brand_tenant_slug_key
    ON organizations (tenant_slug)
    WHERE organization_kind = 'brand_cloud';

CREATE TABLE IF NOT EXISTS brand_cloud_users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    brand_cloud_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    display_name TEXT,
    email_verified BOOLEAN NOT NULL DEFAULT false,
    email_verified_at TIMESTAMPTZ,
    signup_pending_verification BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at TIMESTAMPTZ,
    CONSTRAINT brand_cloud_users_email_not_blank CHECK (btrim(email) <> ''),
    CONSTRAINT brand_cloud_users_email_normalized CHECK (email = lower(btrim(email))),
    CONSTRAINT brand_cloud_users_brand_email_key UNIQUE (brand_cloud_id, email)
);

CREATE INDEX IF NOT EXISTS brand_cloud_users_email_idx
    ON brand_cloud_users (email);

CREATE TABLE IF NOT EXISTS brand_cloud_memberships (
    brand_cloud_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    brand_cloud_user_id UUID NOT NULL REFERENCES brand_cloud_users(id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT brand_cloud_memberships_role_check CHECK (role IN ('owner', 'admin', 'member')),
    CONSTRAINT brand_cloud_memberships_brand_user_key UNIQUE (brand_cloud_id, brand_cloud_user_id)
);

CREATE INDEX IF NOT EXISTS brand_cloud_memberships_user_idx
    ON brand_cloud_memberships (brand_cloud_user_id);

CREATE TABLE IF NOT EXISTS brand_cloud_refresh_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    brand_cloud_user_id UUID NOT NULL REFERENCES brand_cloud_users(id) ON DELETE CASCADE,
    brand_cloud_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS brand_cloud_refresh_tokens_user_idx
    ON brand_cloud_refresh_tokens (brand_cloud_user_id, brand_cloud_id)
    WHERE revoked_at IS NULL;

ALTER TABLE role_assignments
    DROP CONSTRAINT IF EXISTS role_assignments_actor_type_check;

ALTER TABLE role_assignments
    ADD CONSTRAINT role_assignments_actor_type_check CHECK (actor_type IN ('user', 'service_account', 'brand_cloud_user'));

INSERT INTO brand_cloud_users (
    brand_cloud_id,
    email,
    password_hash,
    display_name,
    email_verified,
    email_verified_at,
    signup_pending_verification,
    created_at,
    updated_at,
    disabled_at
)
SELECT
    o.id,
    lower(btrim(u.email)),
    u.password_hash,
    u.display_name,
    u.email_verified,
    u.email_verified_at,
    u.signup_pending_verification,
    LEAST(u.created_at, m.created_at),
    GREATEST(u.updated_at, m.updated_at),
    u.disabled_at
FROM organization_members m
JOIN organizations o ON o.id = m.organization_id AND o.organization_kind = 'brand_cloud'
JOIN users u ON u.id = m.user_id
ON CONFLICT ON CONSTRAINT brand_cloud_users_brand_email_key
DO UPDATE SET
    display_name = COALESCE(EXCLUDED.display_name, brand_cloud_users.display_name),
    email_verified = EXCLUDED.email_verified,
    email_verified_at = COALESCE(brand_cloud_users.email_verified_at, EXCLUDED.email_verified_at),
    signup_pending_verification = EXCLUDED.signup_pending_verification,
    disabled_at = EXCLUDED.disabled_at,
    updated_at = now();

INSERT INTO brand_cloud_memberships (brand_cloud_id, brand_cloud_user_id, role, created_at, updated_at)
SELECT
    o.id,
    bcu.id,
    m.role,
    m.created_at,
    m.updated_at
FROM organization_members m
JOIN organizations o ON o.id = m.organization_id AND o.organization_kind = 'brand_cloud'
JOIN users u ON u.id = m.user_id
JOIN brand_cloud_users bcu ON bcu.brand_cloud_id = o.id AND bcu.email = lower(btrim(u.email))
ON CONFLICT ON CONSTRAINT brand_cloud_memberships_brand_user_key
DO UPDATE SET role = EXCLUDED.role, updated_at = now();

INSERT INTO role_assignments (role_id, actor_type, actor_id, scope_type, scope_id, organization_id)
SELECT
    r.id,
    'brand_cloud_user',
    bcm.brand_cloud_user_id::text,
    'organization',
    bcm.brand_cloud_id::text,
    bcm.brand_cloud_id
FROM brand_cloud_memberships bcm
JOIN roles r ON r.name = bcm.role
ON CONFLICT DO NOTHING;

CREATE OR REPLACE FUNCTION sync_brand_cloud_membership_role_assignment()
RETURNS trigger AS $$
DECLARE
    target_role_id UUID;
BEGIN
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
    VALUES (target_role_id, 'brand_cloud_user', NEW.brand_cloud_user_id::text, 'organization', NEW.brand_cloud_id::text, NEW.brand_cloud_id)
    ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS brand_cloud_memberships_sync_role_assignment ON brand_cloud_memberships;
CREATE TRIGGER brand_cloud_memberships_sync_role_assignment
    AFTER INSERT OR UPDATE OF role ON brand_cloud_memberships
    FOR EACH ROW EXECUTE FUNCTION sync_brand_cloud_membership_role_assignment();

DROP TRIGGER IF EXISTS brand_cloud_users_set_updated_at ON brand_cloud_users;
CREATE TRIGGER brand_cloud_users_set_updated_at
    BEFORE UPDATE ON brand_cloud_users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS brand_cloud_memberships_set_updated_at ON brand_cloud_memberships;
CREATE TRIGGER brand_cloud_memberships_set_updated_at
    BEFORE UPDATE ON brand_cloud_memberships
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
