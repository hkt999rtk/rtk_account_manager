ALTER TABLE organization_members
    ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS organization_members_active_brand_cloud_idx
    ON organization_members (organization_id, disabled_at, created_at);
