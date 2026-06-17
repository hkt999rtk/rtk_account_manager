CREATE TABLE IF NOT EXISTS factory_production_runs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    brand_cloud_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    device_item_profile_id UUID NOT NULL REFERENCES device_item_profiles(id) ON DELETE RESTRICT,
    factory_id TEXT NOT NULL DEFAULT '',
    batch_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active',
    allowed_quantity INTEGER NOT NULL,
    issued_quantity INTEGER NOT NULL DEFAULT 0,
    valid_from TIMESTAMPTZ NOT NULL,
    valid_until TIMESTAMPTZ NOT NULL,
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT factory_production_runs_status_check CHECK (status IN ('active', 'disabled')),
    CONSTRAINT factory_production_runs_allowed_quantity_positive CHECK (allowed_quantity > 0),
    CONSTRAINT factory_production_runs_issued_quantity_non_negative CHECK (issued_quantity >= 0),
    CONSTRAINT factory_production_runs_period_check CHECK (valid_until > valid_from)
);

CREATE INDEX IF NOT EXISTS factory_production_runs_brand_profile_idx
    ON factory_production_runs (brand_cloud_id, device_item_profile_id, created_at DESC);

CREATE INDEX IF NOT EXISTS factory_production_runs_status_validity_idx
    ON factory_production_runs (status, valid_from, valid_until);

DROP TRIGGER IF EXISTS factory_production_runs_set_updated_at ON factory_production_runs;
CREATE TRIGGER factory_production_runs_set_updated_at
    BEFORE UPDATE ON factory_production_runs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
