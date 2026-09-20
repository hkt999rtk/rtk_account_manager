CREATE TABLE product_service_grants (
    product_id uuid NOT NULL REFERENCES device_item_profiles(id) ON DELETE RESTRICT,
    revision bigint NOT NULL CHECK (revision > 0),
    brand_cloud_id uuid NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    catalog_revision bigint NOT NULL CHECK (catalog_revision > 0),
    options jsonb NOT NULL,
    bindings jsonb NOT NULL,
    snapshot_sha256 text NOT NULL CHECK (snapshot_sha256 ~ '^[0-9a-f]{64}$'),
    created_by uuid REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (product_id, revision)
);

CREATE INDEX product_service_grants_cloud_product
    ON product_service_grants(brand_cloud_id, product_id, revision DESC);

ALTER TABLE factory_production_runs
    ADD COLUMN product_service_revision bigint,
    ADD CONSTRAINT factory_run_service_grant_fk
      FOREIGN KEY (device_item_profile_id, product_service_revision)
      REFERENCES product_service_grants(product_id, revision);

CREATE INDEX factory_production_runs_service_revision
    ON factory_production_runs(device_item_profile_id, product_service_revision);
