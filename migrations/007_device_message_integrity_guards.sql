DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'devices_org_id_unique'
    ) THEN
        ALTER TABLE devices
            ADD CONSTRAINT devices_org_id_unique
            UNIQUE (organization_id, id);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'device_operations_org_device_fkey'
    ) THEN
        ALTER TABLE device_operations
            ADD CONSTRAINT device_operations_org_device_fkey
            FOREIGN KEY (organization_id, device_id)
            REFERENCES devices (organization_id, id)
            ON DELETE CASCADE;
    END IF;
END $$;
