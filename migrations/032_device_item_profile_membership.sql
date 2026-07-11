ALTER TABLE devices
    ADD COLUMN IF NOT EXISTS device_item_profile_id UUID REFERENCES device_item_profiles(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS devices_org_profile_idx
    ON devices (organization_id, device_item_profile_id)
    WHERE device_item_profile_id IS NOT NULL;
