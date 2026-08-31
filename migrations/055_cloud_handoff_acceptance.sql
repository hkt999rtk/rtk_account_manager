-- Acceptance is a durable preparation request, never an immediate owner swap.
-- Historical requests do not acquire inferred ownership/evidence on migration.
ALTER TABLE brand_cloud_owner_transfers
    ADD COLUMN ownership_version BIGINT CHECK(ownership_version>0),
    ADD COLUMN request_eligibility JSONB,
    ADD CONSTRAINT transfer_handoff_identity UNIQUE(id,brand_cloud_id,requested_by_user_id,target_user_id,ownership_version);

CREATE TABLE cloud_ownership_handoffs (
    id UUID PRIMARY KEY,
    brand_cloud_id UUID NOT NULL,
    source_user_id UUID NOT NULL,
    target_user_id UUID NOT NULL,
    ownership_version BIGINT NOT NULL,
    phase TEXT NOT NULL DEFAULT 'preparing' CHECK(phase IN ('preparing','canceling','canceled')),
    version BIGINT NOT NULL DEFAULT 1 CHECK(version>0),
    cutoff TIMESTAMPTZ NOT NULL,
    acceptance_eligibility JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY(id,brand_cloud_id,source_user_id,target_user_id,ownership_version)
        REFERENCES brand_cloud_owner_transfers(id,brand_cloud_id,requested_by_user_id,target_user_id,ownership_version) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX cloud_handoff_one_active ON cloud_ownership_handoffs(brand_cloud_id) WHERE phase<>'canceled';
CREATE INDEX cloud_handoff_incoming_reservations ON cloud_ownership_handoffs(target_user_id) WHERE phase<>'canceled';

CREATE TABLE cloud_handoff_participants (
    operation_id UUID NOT NULL REFERENCES cloud_ownership_handoffs(id) ON DELETE RESTRICT,
    participant TEXT NOT NULL CHECK(participant ~ '^[a-z][a-z0-9_-]{0,63}$'),
    PRIMARY KEY(operation_id,participant)
);
CREATE TABLE cloud_handoff_outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    operation_id UUID NOT NULL,
    participant TEXT NOT NULL,
    action TEXT NOT NULL CHECK(action IN ('prepare','abort')),
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY(operation_id,participant) REFERENCES cloud_handoff_participants(operation_id,participant) ON DELETE RESTRICT,
    UNIQUE(operation_id,participant,action)
);
CREATE TABLE cloud_handoff_abort_acknowledgments (
    operation_id UUID NOT NULL,
    participant TEXT NOT NULL,
    receipt_sha256 TEXT NOT NULL CHECK(receipt_sha256 ~ '^[0-9a-f]{64}$'),
    acknowledged_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(operation_id,participant),
    FOREIGN KEY(operation_id,participant) REFERENCES cloud_handoff_participants(operation_id,participant) ON DELETE RESTRICT
);
CREATE FUNCTION reject_cloud_handoff_evidence_mutation() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'cloud handoff evidence is append-only' USING ERRCODE='23514';
END;
$$;
CREATE TRIGGER cloud_handoff_participants_immutable BEFORE UPDATE OR DELETE ON cloud_handoff_participants
    FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();
CREATE TRIGGER cloud_handoff_outbox_immutable BEFORE UPDATE OR DELETE ON cloud_handoff_outbox
    FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();
CREATE TRIGGER cloud_handoff_abort_ack_immutable BEFORE UPDATE OR DELETE ON cloud_handoff_abort_acknowledgments
    FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();

CREATE FUNCTION guard_cloud_handoff_transition() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'cloud handoff history cannot be deleted' USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' AND (ROW(NEW.id,NEW.brand_cloud_id,NEW.source_user_id,NEW.target_user_id,NEW.ownership_version,NEW.cutoff,NEW.acceptance_eligibility)
            IS DISTINCT FROM ROW(OLD.id,OLD.brand_cloud_id,OLD.source_user_id,OLD.target_user_id,OLD.ownership_version,OLD.cutoff,OLD.acceptance_eligibility)
        OR NOT ((OLD.phase='preparing' AND NEW.phase='canceling') OR (OLD.phase='canceling' AND NEW.phase='canceled'))
        OR NEW.version<>OLD.version+1) THEN
        RAISE EXCEPTION 'invalid cloud handoff transition' USING ERRCODE='23514';
    END IF;
    PERFORM id FROM users WHERE id IN (NEW.source_user_id,NEW.target_user_id) ORDER BY id FOR UPDATE;
    UPDATE organizations SET authorization_version=authorization_version+1 WHERE id=NEW.brand_cloud_id;
    RETURN NEW;
END;
$$;
CREATE TRIGGER cloud_handoff_transition BEFORE INSERT OR UPDATE OR DELETE ON cloud_ownership_handoffs
    FOR EACH ROW EXECUTE FUNCTION guard_cloud_handoff_transition();

-- Preserve the existing eligibility rules, adding a lifecycle fence. Participant
-- status/cancellation uses a separate global-session boundary, not cloud access.
ALTER FUNCTION user_can_access_brand_cloud(TEXT,TEXT) RENAME TO user_can_access_brand_cloud_without_handoff;
CREATE FUNCTION user_can_access_brand_cloud(actor TEXT,cloud TEXT) RETURNS BOOLEAN LANGUAGE sql STABLE AS $$
    SELECT user_can_access_brand_cloud_without_handoff(actor,cloud)
        AND NOT EXISTS(SELECT 1 FROM cloud_ownership_handoffs h WHERE h.brand_cloud_id::text=cloud AND h.phase<>'canceled');
$$;
