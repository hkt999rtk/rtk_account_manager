DO $$
DECLARE
    duplicate_record RECORD;
BEGIN
    SELECT
        organization_id,
        metadata->>'video_cloud_devid' AS video_cloud_devid,
        count(*) AS duplicate_count
    INTO duplicate_record
    FROM devices
    WHERE disabled_at IS NULL
      AND metadata ? 'video_cloud_devid'
    GROUP BY organization_id, metadata->>'video_cloud_devid'
    HAVING count(*) > 1
    LIMIT 1;

    IF FOUND THEN
        RAISE EXCEPTION 'duplicate active video_cloud_devid before index creation: organization_id=%, video_cloud_devid=%, count=%',
            duplicate_record.organization_id,
            duplicate_record.video_cloud_devid,
            duplicate_record.duplicate_count;
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS devices_org_video_cloud_devid_active_unique
    ON devices (organization_id, (metadata->>'video_cloud_devid'))
    WHERE disabled_at IS NULL AND metadata ? 'video_cloud_devid';
