ALTER TABLE users
    ADD COLUMN IF NOT EXISTS developer_cloud_limit INTEGER NOT NULL DEFAULT 8;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'users_developer_cloud_limit_check'
    ) THEN
        ALTER TABLE users
            ADD CONSTRAINT users_developer_cloud_limit_check
            CHECK (developer_cloud_limit BETWEEN 0 AND 200);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS brand_cloud_owner_transfers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    brand_cloud_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    requested_by_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    target_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL DEFAULT 'pending',
    expires_at TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    canceled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT brand_cloud_owner_transfers_status_check CHECK (status IN ('pending', 'accepted', 'canceled', 'expired')),
    CONSTRAINT brand_cloud_owner_transfers_different_users CHECK (requested_by_user_id <> target_user_id)
);

CREATE INDEX IF NOT EXISTS brand_cloud_owner_transfers_brand_status_idx
    ON brand_cloud_owner_transfers (brand_cloud_id, status, created_at DESC);

CREATE INDEX IF NOT EXISTS brand_cloud_owner_transfers_target_status_idx
    ON brand_cloud_owner_transfers (target_user_id, status, created_at DESC);

DROP TRIGGER IF EXISTS brand_cloud_owner_transfers_set_updated_at ON brand_cloud_owner_transfers;
CREATE TRIGGER brand_cloud_owner_transfers_set_updated_at
    BEFORE UPDATE ON brand_cloud_owner_transfers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
