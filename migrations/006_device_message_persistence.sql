CREATE TABLE IF NOT EXISTS device_operations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    operation_id TEXT NOT NULL UNIQUE,
    correlation_id TEXT NOT NULL,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    operation_type TEXT NOT NULL CHECK (operation_type IN ('provision', 'deactivate')),
    status TEXT NOT NULL CHECK (status IN ('pending', 'published', 'succeeded', 'failed', 'retrying', 'dead_lettered')),
    requested_by TEXT,
    request_payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    result_payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    error_code TEXT,
    error_message TEXT,
    retryable BOOLEAN,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS device_operations_org_device_created_idx
    ON device_operations (organization_id, device_id, created_at DESC);

CREATE INDEX IF NOT EXISTS device_operations_status_created_idx
    ON device_operations (status, created_at ASC);

CREATE TABLE IF NOT EXISTS device_message_outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id TEXT NOT NULL UNIQUE,
    operation_id TEXT NOT NULL REFERENCES device_operations(operation_id) ON DELETE CASCADE,
    correlation_id TEXT NOT NULL,
    causation_id TEXT,
    stream TEXT NOT NULL CHECK (stream = 'account.video.commands'),
    message_type TEXT NOT NULL CHECK (message_type IN ('DeviceProvisionRequested', 'DeviceDeactivateRequested')),
    schema_version TEXT NOT NULL CHECK (schema_version IN ('1.0')),
    partition_key TEXT NOT NULL CHECK (btrim(partition_key) <> ''),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    status TEXT NOT NULL CHECK (status IN ('pending', 'published', 'retrying', 'dead_lettered')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    last_error TEXT,
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS device_message_outbox_status_available_idx
    ON device_message_outbox (status, available_at ASC, created_at ASC);

CREATE INDEX IF NOT EXISTS device_message_outbox_operation_idx
    ON device_message_outbox (operation_id);

CREATE TABLE IF NOT EXISTS device_message_inbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id TEXT NOT NULL UNIQUE,
    operation_id TEXT NOT NULL,
    correlation_id TEXT NOT NULL,
    causation_id TEXT,
    stream TEXT NOT NULL CHECK (stream = 'video.account.events'),
    message_type TEXT NOT NULL CHECK (message_type IN ('DeviceProvisionSucceeded', 'DeviceProvisionFailed', 'DeviceDeactivateSucceeded', 'DeviceDeactivateFailed', 'DeviceOnlineChanged', 'DeviceMetadataChanged')),
    schema_version TEXT NOT NULL CHECK (schema_version IN ('1.0')),
    partition_key TEXT NOT NULL CHECK (btrim(partition_key) <> ''),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    status TEXT NOT NULL CHECK (status IN ('processed', 'failed', 'retrying', 'dead_lettered')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    last_error TEXT,
    received_at TIMESTAMPTZ NOT NULL,
    processed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS device_message_inbox_status_received_idx
    ON device_message_inbox (status, received_at ASC, created_at ASC);

CREATE INDEX IF NOT EXISTS device_message_inbox_operation_idx
    ON device_message_inbox (operation_id);

DROP TRIGGER IF EXISTS device_operations_set_updated_at ON device_operations;
CREATE TRIGGER device_operations_set_updated_at
    BEFORE UPDATE ON device_operations
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS device_message_outbox_set_updated_at ON device_message_outbox;
CREATE TRIGGER device_message_outbox_set_updated_at
    BEFORE UPDATE ON device_message_outbox
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS device_message_inbox_set_updated_at ON device_message_inbox;
CREATE TRIGGER device_message_inbox_set_updated_at
    BEFORE UPDATE ON device_message_inbox
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();
