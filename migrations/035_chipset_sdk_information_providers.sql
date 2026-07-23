CREATE TABLE IF NOT EXISTS chipset_information_providers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    manifest_url TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL DEFAULT 'draft',
    manifest_version TEXT NOT NULL DEFAULT '',
    manifest_sha256 TEXT NOT NULL DEFAULT '',
    etag TEXT NOT NULL DEFAULT '',
    last_modified TEXT NOT NULL DEFAULT '',
    snapshot JSONB,
    chipset_count INTEGER NOT NULL DEFAULT 0,
    sdk_release_count INTEGER NOT NULL DEFAULT 0,
    last_refresh_attempt_at TIMESTAMPTZ,
    last_successful_refresh_at TIMESTAMPTZ,
    stale BOOLEAN NOT NULL DEFAULT false,
    validation_error TEXT NOT NULL DEFAULT '',
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chipset_information_providers_name_not_blank CHECK (btrim(name) <> ''),
    CONSTRAINT chipset_information_providers_url_not_blank CHECK (btrim(manifest_url) <> ''),
    CONSTRAINT chipset_information_providers_status_check CHECK (status IN ('draft', 'published', 'unpublished')),
    CONSTRAINT chipset_information_providers_counts_check CHECK (chipset_count >= 0 AND sdk_release_count >= 0)
);

CREATE INDEX IF NOT EXISTS chipset_information_providers_status_idx
    ON chipset_information_providers (status, updated_at DESC);

DROP TRIGGER IF EXISTS chipset_information_providers_set_updated_at ON chipset_information_providers;
CREATE TRIGGER chipset_information_providers_set_updated_at
    BEFORE UPDATE ON chipset_information_providers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

INSERT INTO permissions (name, domain, action, description)
VALUES
    ('platform.chipset_sdk.read', 'platform.chipset_sdk', 'read', 'Read ChipSet and SDK provider configuration and refresh state'),
    ('platform.chipset_sdk.edit', 'platform.chipset_sdk', 'edit', 'Create and edit draft ChipSet and SDK providers'),
    ('platform.chipset_sdk.publish', 'platform.chipset_sdk', 'publish', 'Publish, unpublish, and refresh ChipSet and SDK providers')
ON CONFLICT (name) DO UPDATE
SET domain = EXCLUDED.domain,
    action = EXCLUDED.action,
    description = EXCLUDED.description;

INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM roles r
CROSS JOIN permissions p
WHERE r.name = 'platform_admin'
  AND p.name IN (
      'platform.chipset_sdk.read',
      'platform.chipset_sdk.edit',
      'platform.chipset_sdk.publish'
  )
ON CONFLICT DO NOTHING;

