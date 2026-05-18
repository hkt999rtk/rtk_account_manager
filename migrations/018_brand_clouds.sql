ALTER TABLE organizations
    ADD COLUMN IF NOT EXISTS organization_kind TEXT NOT NULL DEFAULT 'customer_org',
    ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'active',
    ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}'::jsonb;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'organizations_kind_check'
    ) THEN
        ALTER TABLE organizations
            ADD CONSTRAINT organizations_kind_check
            CHECK (organization_kind IN ('customer_org', 'brand_cloud'));
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'organizations_status_check'
    ) THEN
        ALTER TABLE organizations
            ADD CONSTRAINT organizations_status_check
            CHECK (status IN ('active', 'disabled'));
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS organizations_kind_status_idx
    ON organizations (organization_kind, status, created_at DESC);
