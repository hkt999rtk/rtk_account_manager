-- Durable producer evidence precedes any confirmable balance or owner commit.
-- A prepare HTTP delivery alone is not evidence of a hold and drained cutoff.
CREATE TABLE cloud_handoff_prepare_acknowledgments (
    operation_id UUID NOT NULL,
    participant TEXT NOT NULL,
    hold_receipt_sha256 TEXT NOT NULL CHECK(hold_receipt_sha256 ~ '^[0-9a-f]{64}$'),
    drain_checkpoint_sha256 TEXT NOT NULL CHECK(drain_checkpoint_sha256 ~ '^[0-9a-f]{64}$'),
    acknowledged_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(operation_id,participant),
    FOREIGN KEY(operation_id,participant) REFERENCES cloud_handoff_participants(operation_id,participant) ON DELETE RESTRICT
);
CREATE TRIGGER cloud_handoff_prepare_ack_immutable BEFORE UPDATE OR DELETE ON cloud_handoff_prepare_acknowledgments
    FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();

-- Preserve the 055 transition graph, additionally requiring initial preparation
-- and all durable release receipts before cancellation can release its fence.
-- Published migration files are not modified.
CREATE OR REPLACE FUNCTION guard_cloud_handoff_transition() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'cloud handoff history cannot be deleted' USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND (NEW.phase<>'preparing' OR NEW.version<>1) THEN
        RAISE EXCEPTION 'cloud handoff must begin in preparation' USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' AND (ROW(NEW.id,NEW.brand_cloud_id,NEW.source_user_id,NEW.target_user_id,NEW.ownership_version,NEW.cutoff,NEW.acceptance_eligibility)
            IS DISTINCT FROM ROW(OLD.id,OLD.brand_cloud_id,OLD.source_user_id,OLD.target_user_id,OLD.ownership_version,OLD.cutoff,OLD.acceptance_eligibility)
        OR NOT ((OLD.phase='preparing' AND NEW.phase='canceling') OR (OLD.phase='canceling' AND NEW.phase='canceled'))
        OR NEW.version<>OLD.version+1) THEN
        RAISE EXCEPTION 'invalid cloud handoff transition' USING ERRCODE='23514';
    END IF;
    PERFORM id FROM users WHERE id IN (NEW.source_user_id,NEW.target_user_id) ORDER BY id FOR UPDATE;
    PERFORM id FROM organizations WHERE id=NEW.brand_cloud_id FOR UPDATE;
    IF TG_OP='UPDATE' AND NEW.phase='canceled' AND (
        NOT EXISTS(SELECT 1 FROM cloud_handoff_participants WHERE operation_id=NEW.id AND participant='billing')
        OR NOT EXISTS(SELECT 1 FROM cloud_handoff_participants WHERE operation_id=NEW.id AND participant<>'billing')
        OR EXISTS(SELECT 1 FROM cloud_handoff_participants p WHERE p.operation_id=NEW.id AND NOT EXISTS(
            SELECT 1 FROM cloud_handoff_abort_acknowledgments a
            WHERE a.operation_id=p.operation_id AND a.participant=p.participant))) THEN
        RAISE EXCEPTION 'cloud handoff holds have not all been released' USING ERRCODE='23514';
    END IF;
    UPDATE organizations SET authorization_version=authorization_version+1 WHERE id=NEW.brand_cloud_id;
    RETURN NEW;
END;
$$;
