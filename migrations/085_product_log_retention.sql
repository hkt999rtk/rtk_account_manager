ALTER TABLE device_item_profiles
    ADD COLUMN log_retention_days SMALLINT;

ALTER TABLE device_item_profiles
    ADD CONSTRAINT device_item_profiles_log_retention_days_check
    CHECK (log_retention_days IS NULL OR log_retention_days IN (7, 30, 90));
