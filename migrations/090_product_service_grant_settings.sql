-- Settings are pinned with a Product grant and forwarded with each device snapshot.
-- Existing grants stay null: migration must not silently grant or alter old devices.
ALTER TABLE product_service_grants
    ADD COLUMN log_retention_days integer CHECK (log_retention_days IN (7, 30, 90));

ALTER TABLE device_entitlement_snapshots
    ADD COLUMN log_retention_days integer CHECK (log_retention_days IN (7, 30, 90));
