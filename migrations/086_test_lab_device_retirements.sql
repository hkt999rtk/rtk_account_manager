CREATE TABLE test_lab_device_retirements (
    device_id UUID PRIMARY KEY REFERENCES devices(id),
    brand_cloud_id UUID NOT NULL REFERENCES organizations(id),
    product_id UUID NOT NULL REFERENCES device_item_profiles(id),
    operation_id UUID NOT NULL UNIQUE,
    requested_by UUID NOT NULL REFERENCES users(id),
    status TEXT NOT NULL CHECK (status IN ('pending', 'failed', 'completed')),
    failure_code TEXT,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX test_lab_device_retirements_scope ON test_lab_device_retirements(brand_cloud_id, product_id, requested_at DESC);
