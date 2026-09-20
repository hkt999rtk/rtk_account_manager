ALTER TABLE device_operations DROP CONSTRAINT device_operations_operation_type_check;
ALTER TABLE device_operations ADD CONSTRAINT device_operations_operation_type_check
    CHECK (operation_type IN ('provision', 'deactivate', 'unprovision', 'entitlement_update'));

ALTER TABLE device_message_outbox DROP CONSTRAINT device_message_outbox_message_type_check;
ALTER TABLE device_message_outbox ADD CONSTRAINT device_message_outbox_message_type_check
    CHECK (message_type IN ('DeviceProvisionRequested', 'DeviceDeactivateRequested',
        'DeviceUnprovisionRequested', 'DeviceEntitlementSnapshotRequested'));

ALTER TABLE device_message_inbox DROP CONSTRAINT device_message_inbox_message_type_check;
ALTER TABLE device_message_inbox ADD CONSTRAINT device_message_inbox_message_type_check
    CHECK (message_type IN ('DeviceProvisionSucceeded', 'DeviceProvisionFailed',
        'DeviceDeactivateSucceeded', 'DeviceDeactivateFailed',
        'DeviceUnprovisionSucceeded', 'DeviceUnprovisionFailed',
        'DeviceEntitlementSnapshotSucceeded', 'DeviceEntitlementSnapshotFailed',
        'DeviceOnlineChanged', 'DeviceMetadataChanged'));

CREATE TABLE device_entitlement_snapshots (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Retain stable identities after unprovision/cloud deletion, like claim/audit history.
    -- FKs to live entities would block the existing physical-delete lifecycle.
    organization_id uuid NOT NULL,
    account_device_id uuid NOT NULL,
    video_cloud_devid text NOT NULL CHECK (btrim(video_cloud_devid) <> ''),
    revision bigint NOT NULL CHECK (revision > 0),
    product_id uuid NOT NULL,
    product_service_revision bigint NOT NULL,
    service_grant_sha256 text NOT NULL CHECK (service_grant_sha256 ~ '^[0-9a-f]{64}$'),
    service_options jsonb NOT NULL,
    entitlement_state text NOT NULL CHECK (entitlement_state IN ('active', 'suspended', 'revoked')),
    operation_id text NOT NULL UNIQUE,
    created_by uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_device_id, revision)
);

CREATE INDEX device_entitlement_snapshots_latest
    ON device_entitlement_snapshots(organization_id, account_device_id, revision DESC);

CREATE FUNCTION guard_device_entitlement_snapshot_history() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'device entitlement snapshot history is immutable' USING ERRCODE='23514';
END $$;

CREATE TRIGGER device_entitlement_snapshot_history
    BEFORE UPDATE OR DELETE ON device_entitlement_snapshots
    FOR EACH ROW EXECUTE FUNCTION guard_device_entitlement_snapshot_history();
