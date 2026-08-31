-- Scheduling metadata never grants ownership or releases a lifecycle fence.
CREATE TABLE cloud_handoff_jobs (
    operation_id UUID PRIMARY KEY REFERENCES cloud_ownership_handoffs(id) ON DELETE RESTRICT,
    generation BIGINT NOT NULL DEFAULT 1 CHECK(generation>0),
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_id UUID,
    lease_until TIMESTAMPTZ,
    claim_count BIGINT NOT NULL DEFAULT 0 CHECK(claim_count>=0),
    retry_count INTEGER NOT NULL DEFAULT 0 CHECK(retry_count>=0),
    last_outcome TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK((lease_id IS NULL)=(lease_until IS NULL))
);
CREATE INDEX cloud_handoff_jobs_ready ON cloud_handoff_jobs(available_at,operation_id);
INSERT INTO cloud_handoff_jobs(operation_id)
    SELECT id FROM cloud_ownership_handoffs WHERE phase NOT IN ('succeeded','canceled');

CREATE FUNCTION wake_cloud_handoff_job() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE operation UUID;
BEGIN
    IF TG_TABLE_NAME='cloud_handoff_confirmation_acknowledgments' THEN
        SELECT operation_id INTO STRICT operation FROM cloud_handoff_confirmation_requests WHERE id=NEW.request_id;
    ELSIF TG_TABLE_NAME='cloud_ownership_handoffs' THEN operation:=NEW.id;
    ELSE operation:=NEW.operation_id; END IF;
    INSERT INTO cloud_handoff_jobs(operation_id) VALUES(operation)
    ON CONFLICT(operation_id) DO UPDATE SET generation=cloud_handoff_jobs.generation+1,
        available_at=LEAST(cloud_handoff_jobs.available_at,clock_timestamp()),updated_at=clock_timestamp();
    RETURN NULL;
END;
$$;
CREATE TRIGGER cloud_handoff_job_phase AFTER INSERT OR UPDATE ON cloud_ownership_handoffs
    FOR EACH ROW EXECUTE FUNCTION wake_cloud_handoff_job();
CREATE TRIGGER cloud_handoff_job_prepared AFTER INSERT ON cloud_handoff_prepare_acknowledgments
    FOR EACH ROW EXECUTE FUNCTION wake_cloud_handoff_job();
CREATE TRIGGER cloud_handoff_job_confirmation_intent AFTER INSERT ON cloud_handoff_confirmation_requests
    FOR EACH ROW EXECUTE FUNCTION wake_cloud_handoff_job();
CREATE TRIGGER cloud_handoff_job_confirmation_ack AFTER INSERT ON cloud_handoff_confirmation_acknowledgments
    FOR EACH ROW EXECUTE FUNCTION wake_cloud_handoff_job();
CREATE TRIGGER cloud_handoff_job_finalization_ack AFTER INSERT ON cloud_handoff_finalization_acknowledgments
    FOR EACH ROW EXECUTE FUNCTION wake_cloud_handoff_job();

CREATE TABLE cloud_handoff_canceled_decisions (
    operation_id UUID PRIMARY KEY REFERENCES cloud_ownership_handoffs(id) ON DELETE RESTRICT,
    cancellation_id UUID NOT NULL UNIQUE DEFAULT gen_random_uuid(),
    authorization_id UUID,
    canceled_at TIMESTAMPTZ NOT NULL,
    decision_sha256 TEXT NOT NULL CHECK(decision_sha256 ~ '^[0-9a-f]{64}$')
);
CREATE TRIGGER cloud_handoff_canceled_decision_immutable BEFORE UPDATE OR DELETE ON cloud_handoff_canceled_decisions
    FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();
CREATE FUNCTION verify_cloud_handoff_canceled_decision() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS(SELECT 1 FROM cloud_ownership_handoffs h WHERE h.id=NEW.operation_id AND h.phase IN ('canceling','canceled'))
        OR EXISTS(SELECT 1 FROM cloud_handoff_committed_decisions WHERE operation_id=NEW.operation_id)
        OR NEW.authorization_id IS DISTINCT FROM (SELECT authorization_id FROM cloud_handoff_commit_requests WHERE operation_id=NEW.operation_id) THEN
        RAISE EXCEPTION 'handoff cancellation must match a durable precommit decision' USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER cloud_handoff_cancellation_precommit AFTER INSERT ON cloud_handoff_canceled_decisions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION verify_cloud_handoff_canceled_decision();
