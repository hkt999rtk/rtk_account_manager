ALTER TABLE product_service_grants
    ADD COLUMN legacy boolean NOT NULL DEFAULT false;

ALTER TABLE product_service_grants
    DROP CONSTRAINT product_service_grants_catalog_revision_check,
    ADD CONSTRAINT product_service_grants_catalog_revision_source_check
        CHECK ((legacy AND catalog_revision = 0) OR (NOT legacy AND catalog_revision > 0));
