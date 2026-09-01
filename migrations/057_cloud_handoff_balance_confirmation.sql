-- AM retains the preview and authenticated confirmation intent; Billing remains
-- authoritative for live settlement validity and financial consent receipts.
CREATE TABLE cloud_handoff_billing_snapshots (
    operation_id UUID NOT NULL REFERENCES cloud_ownership_handoffs(id) ON DELETE RESTRICT,
    billing_snapshot_version BIGINT NOT NULL CHECK(billing_snapshot_version>=2),
    balance_minor BIGINT NOT NULL CHECK(balance_minor>=0),
    currency TEXT NOT NULL CHECK(currency='TWD'),
    cutoff TIMESTAMPTZ NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(operation_id,billing_snapshot_version)
);
CREATE TABLE cloud_handoff_confirmation_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    operation_id UUID NOT NULL,
    billing_snapshot_version BIGINT NOT NULL,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    idempotency_key TEXT NOT NULL CHECK(idempotency_key ~ '^[!-~]{1,128}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY(operation_id,billing_snapshot_version)
        REFERENCES cloud_handoff_billing_snapshots(operation_id,billing_snapshot_version) ON DELETE RESTRICT,
    UNIQUE(operation_id,user_id,idempotency_key)
);
CREATE TABLE cloud_handoff_confirmation_acknowledgments (
    request_id UUID PRIMARY KEY REFERENCES cloud_handoff_confirmation_requests(id) ON DELETE RESTRICT,
    receipt_sha256 TEXT NOT NULL CHECK(receipt_sha256 ~ '^[0-9a-f]{64}$'),
    acknowledged_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER cloud_handoff_billing_snapshot_immutable BEFORE UPDATE OR DELETE ON cloud_handoff_billing_snapshots
    FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();
CREATE TRIGGER cloud_handoff_confirmation_request_immutable BEFORE UPDATE OR DELETE ON cloud_handoff_confirmation_requests
    FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();
CREATE TRIGGER cloud_handoff_confirmation_ack_immutable BEFORE UPDATE OR DELETE ON cloud_handoff_confirmation_acknowledgments
    FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();

CREATE FUNCTION guard_cloud_handoff_confirmation_evidence() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE op cloud_ownership_handoffs;
BEGIN
    SELECT * INTO STRICT op FROM cloud_ownership_handoffs WHERE id=NEW.operation_id;
    IF TG_TABLE_NAME='cloud_handoff_billing_snapshots' THEN
        IF NEW.cutoff<>op.cutoff THEN
            RAISE EXCEPTION 'handoff snapshot cutoff mismatch' USING ERRCODE='23514';
        END IF;
    ELSIF NEW.user_id NOT IN (op.source_user_id,op.target_user_id) THEN
        RAISE EXCEPTION 'handoff confirmation requires a participant' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER cloud_handoff_billing_snapshot_scope BEFORE INSERT ON cloud_handoff_billing_snapshots
    FOR EACH ROW EXECUTE FUNCTION guard_cloud_handoff_confirmation_evidence();
CREATE TRIGGER cloud_handoff_confirmation_request_scope BEFORE INSERT ON cloud_handoff_confirmation_requests
    FOR EACH ROW EXECUTE FUNCTION guard_cloud_handoff_confirmation_evidence();
