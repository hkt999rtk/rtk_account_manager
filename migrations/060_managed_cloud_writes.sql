-- Durable actor/operation-scoped receipts survive retries and cloud tombstones.
-- No ownership source is introduced; the response owner is derived from membership.
CREATE TABLE managed_cloud_write_receipts (
    actor_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    operation TEXT NOT NULL CHECK (operation IN ('create', 'update')),
    scope TEXT NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    request JSONB NOT NULL CHECK (jsonb_typeof(request) = 'object'),
    response JSONB NOT NULL CHECK (jsonb_typeof(response) = 'object'),
    cloud_id UUID NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (actor_user_id, operation, scope, idempotency_key),
    CHECK ((operation='create' AND scope='') OR (operation='update' AND scope=cloud_id::text))
);
CREATE FUNCTION reject_managed_cloud_receipt_mutation() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'managed cloud write receipts are immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER managed_cloud_receipt_immutable BEFORE UPDATE OR DELETE ON managed_cloud_write_receipts
    FOR EACH ROW EXECUTE FUNCTION reject_managed_cloud_receipt_mutation();
