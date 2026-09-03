CREATE TABLE IF NOT EXISTS job_authorizations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id TEXT NOT NULL CHECK (btrim(job_id) <> '' AND length(job_id) <= 128),
    brand_cloud_id UUID NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    actor_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    scope_hash TEXT NOT NULL CHECK (scope_hash ~ '^[a-f0-9]{64}$'),
    capability TEXT NOT NULL CHECK (capability IN ('provisioning.create')),
    product_ids UUID[] NOT NULL DEFAULT '{}',
    authorization_version BIGINT NOT NULL CHECK (authorization_version > 0),
    ownership_version BIGINT NOT NULL CHECK (ownership_version > 0),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','revoked','expired')),
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT job_authorizations_job_actor_unique UNIQUE (job_id, brand_cloud_id, actor_user_id),
    CONSTRAINT job_authorizations_expiry_after_creation CHECK (expires_at > created_at),
    CONSTRAINT job_authorizations_revocation_consistent CHECK ((status='revoked')=(revoked_at IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS job_authorizations_active_idx
    ON job_authorizations (brand_cloud_id, status, expires_at);
