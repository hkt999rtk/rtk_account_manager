ALTER TABLE factory_production_runs
    ADD COLUMN IF NOT EXISTS idempotency_key TEXT,
    ADD COLUMN IF NOT EXISTS issued_profile_key TEXT,
    ADD COLUMN IF NOT EXISTS issued_service_options JSONB,
    ADD COLUMN IF NOT EXISTS issued_service_grant_sha256 TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS factory_production_run_actor_intent_unique
    ON factory_production_runs(created_by,idempotency_key)
    WHERE idempotency_key IS NOT NULL;
