ALTER TABLE device_claims
    DROP CONSTRAINT IF EXISTS device_claims_device_id_fkey;

ALTER TABLE device_operations
    DROP CONSTRAINT IF EXISTS device_operations_device_id_fkey;

ALTER TABLE device_operations
    DROP CONSTRAINT IF EXISTS device_operations_org_device_fkey;

ALTER TABLE device_operations
    DROP CONSTRAINT IF EXISTS device_operations_operation_type_check;

ALTER TABLE device_operations
    ADD CONSTRAINT device_operations_operation_type_check
    CHECK (operation_type IN ('provision', 'deactivate', 'unprovision'));

ALTER TABLE device_message_outbox
    DROP CONSTRAINT IF EXISTS device_message_outbox_message_type_check;

ALTER TABLE device_message_outbox
    ADD CONSTRAINT device_message_outbox_message_type_check
    CHECK (message_type IN ('DeviceProvisionRequested', 'DeviceDeactivateRequested', 'DeviceUnprovisionRequested'));

ALTER TABLE device_message_inbox
    DROP CONSTRAINT IF EXISTS device_message_inbox_message_type_check;

ALTER TABLE device_message_inbox
    ADD CONSTRAINT device_message_inbox_message_type_check
    CHECK (message_type IN ('DeviceProvisionSucceeded', 'DeviceProvisionFailed', 'DeviceDeactivateSucceeded', 'DeviceDeactivateFailed', 'DeviceUnprovisionSucceeded', 'DeviceUnprovisionFailed', 'DeviceOnlineChanged', 'DeviceMetadataChanged'));
