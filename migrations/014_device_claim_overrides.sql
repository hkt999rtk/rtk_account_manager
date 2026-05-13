ALTER TABLE device_claims
    ADD COLUMN IF NOT EXISTS overridden_by UUID REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS override_reason TEXT,
    ADD COLUMN IF NOT EXISTS override_evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS overridden_at TIMESTAMPTZ;

ALTER TABLE device_claims
    DROP CONSTRAINT IF EXISTS device_claims_status_check;

ALTER TABLE device_claims
    ADD CONSTRAINT device_claims_status_check
    CHECK (status IN ('resolved', 'transferred', 'reclaimed'));

CREATE INDEX IF NOT EXISTS device_claims_override_idx
    ON device_claims (status, overridden_at DESC)
    WHERE overridden_at IS NOT NULL;
