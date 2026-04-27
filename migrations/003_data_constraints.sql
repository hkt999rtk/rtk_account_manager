DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'organizations_name_not_blank'
    ) THEN
        ALTER TABLE organizations
            ADD CONSTRAINT organizations_name_not_blank
            CHECK (btrim(name) <> '');
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'users_email_normalized'
    ) THEN
        ALTER TABLE users
            ADD CONSTRAINT users_email_normalized
            CHECK (email = lower(btrim(email)) AND email <> '');
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'devices_name_not_blank'
    ) THEN
        ALTER TABLE devices
            ADD CONSTRAINT devices_name_not_blank
            CHECK (btrim(name) <> '');
    END IF;
END $$;
