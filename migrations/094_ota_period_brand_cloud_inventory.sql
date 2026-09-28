-- Both OTA seal producers consume one immutable, database-fenced Cloud set.
-- A transaction begun before month end must not backdate a later Cloud insert.
ALTER TABLE organizations ALTER COLUMN created_at SET DEFAULT clock_timestamp();

CREATE TABLE ota_period_brand_cloud_inventory_freezes (
    period_end TIMESTAMPTZ PRIMARY KEY,
    frozen_at TIMESTAMPTZ NOT NULL,
    cloud_count BIGINT NOT NULL CHECK (cloud_count >= 0)
);

CREATE TABLE ota_period_brand_cloud_inventory (
    period_end TIMESTAMPTZ NOT NULL REFERENCES ota_period_brand_cloud_inventory_freezes(period_end),
    organization_id UUID NOT NULL,
    PRIMARY KEY (period_end, organization_id)
);
