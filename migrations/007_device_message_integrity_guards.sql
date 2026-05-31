DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'devices_org_id_unique'
    ) THEN
        ALTER TABLE devices
            ADD CONSTRAINT devices_org_id_unique
            UNIQUE (organization_id, id);
    END IF;

    ALTER TABLE device_operations
        DROP CONSTRAINT IF EXISTS device_operations_org_device_fkey;
END $$;
