ALTER TABLE users
    ADD COLUMN IF NOT EXISTS signup_pending_verification BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS platform_admin BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE organizations
    ADD COLUMN IF NOT EXISTS tier TEXT NOT NULL DEFAULT 'commercial',
    ADD COLUMN IF NOT EXISTS evaluation_device_quota INTEGER NOT NULL DEFAULT 5;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'organizations_tier_check'
    ) THEN
        ALTER TABLE organizations
            ADD CONSTRAINT organizations_tier_check
            CHECK (tier IN ('evaluation', 'commercial'));
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'organizations_evaluation_device_quota_check'
    ) THEN
        ALTER TABLE organizations
            ADD CONSTRAINT organizations_evaluation_device_quota_check
            CHECK (evaluation_device_quota BETWEEN 1 AND 200);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS quota_raise_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    requested_by UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    requested_quota INTEGER NOT NULL CHECK (requested_quota BETWEEN 1 AND 200),
    use_case TEXT NOT NULL,
    contact_info JSONB NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'approved', 'declined')) DEFAULT 'pending',
    decided_by UUID REFERENCES users(id) ON DELETE SET NULL,
    decision_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS quota_raise_requests_org_status_idx
    ON quota_raise_requests (organization_id, status, created_at DESC);
