CREATE TABLE IF NOT EXISTS device_claim_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID REFERENCES organizations(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    category TEXT NOT NULL,
    video_cloud_devid TEXT NOT NULL,
    activity_id TEXT NOT NULL,
    clip_public_key TEXT NOT NULL,
    service_options JSONB NOT NULL DEFAULT '[]'::jsonb,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    expires_at TIMESTAMPTZ NOT NULL,
    claimed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS device_claim_tokens_org_idx
    ON device_claim_tokens (organization_id, created_at DESC);

CREATE INDEX IF NOT EXISTS device_claim_tokens_active_idx
    ON device_claim_tokens (token_hash)
    WHERE claimed_at IS NULL;

CREATE TABLE IF NOT EXISTS device_claims (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    claim_token_id UUID NOT NULL REFERENCES device_claim_tokens(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    claimed_by UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK (status IN ('resolved')) DEFAULT 'resolved',
    provision_input JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (claim_token_id)
);

CREATE INDEX IF NOT EXISTS device_claims_org_created_idx
    ON device_claims (organization_id, created_at DESC);

CREATE INDEX IF NOT EXISTS device_claims_device_idx
    ON device_claims (device_id);

DROP TRIGGER IF EXISTS device_claim_tokens_set_updated_at ON device_claim_tokens;
CREATE TRIGGER device_claim_tokens_set_updated_at
    BEFORE UPDATE ON device_claim_tokens
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS device_claims_set_updated_at ON device_claims;
CREATE TRIGGER device_claims_set_updated_at
    BEFORE UPDATE ON device_claims
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();
