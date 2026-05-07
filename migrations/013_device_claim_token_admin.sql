ALTER TABLE device_claim_tokens
    ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS notes TEXT;

DROP INDEX IF EXISTS device_claim_tokens_active_idx;
CREATE INDEX IF NOT EXISTS device_claim_tokens_active_idx
    ON device_claim_tokens (token_hash)
    WHERE claimed_at IS NULL AND revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS device_claim_tokens_created_by_idx
    ON device_claim_tokens (created_by, created_at DESC);
