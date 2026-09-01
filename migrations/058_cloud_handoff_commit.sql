-- Durable precommit request, irreversible AM decision, then remote completion.
ALTER TABLE cloud_ownership_handoffs DROP CONSTRAINT cloud_ownership_handoffs_phase_check;
ALTER TABLE cloud_ownership_handoffs ADD CONSTRAINT cloud_ownership_handoffs_phase_check
    CHECK(phase IN ('preparing','committing','finalizing','succeeded','canceling','canceled'));
DROP INDEX cloud_handoff_one_active;
CREATE UNIQUE INDEX cloud_handoff_one_active ON cloud_ownership_handoffs(brand_cloud_id)
    WHERE phase NOT IN ('canceled','succeeded');
DROP INDEX cloud_handoff_incoming_reservations;
CREATE INDEX cloud_handoff_incoming_reservations ON cloud_ownership_handoffs(target_user_id)
    WHERE phase IN ('preparing','committing','canceling');
ALTER TABLE cloud_handoff_outbox DROP CONSTRAINT cloud_handoff_outbox_action_check;
ALTER TABLE cloud_handoff_outbox ADD CONSTRAINT cloud_handoff_outbox_action_check
    CHECK(action IN ('prepare','abort','authorize_commit','finalize','release'));

CREATE TABLE cloud_handoff_commit_requests (
    operation_id UUID PRIMARY KEY REFERENCES cloud_ownership_handoffs(id) ON DELETE RESTRICT,
    authorization_id UUID NOT NULL UNIQUE DEFAULT gen_random_uuid(),
    billing_snapshot_version BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY(operation_id,billing_snapshot_version)
        REFERENCES cloud_handoff_billing_snapshots(operation_id,billing_snapshot_version) ON DELETE RESTRICT,
    UNIQUE(operation_id,authorization_id,billing_snapshot_version)
);
CREATE TABLE cloud_handoff_committed_decisions (
    operation_id UUID PRIMARY KEY,
    authorization_id UUID NOT NULL,
    billing_snapshot_version BIGINT NOT NULL,
    committed_ownership_version BIGINT NOT NULL CHECK(committed_ownership_version>1),
    committed_at TIMESTAMPTZ NOT NULL,
    decision_sha256 TEXT NOT NULL CHECK(decision_sha256 ~ '^[0-9a-f]{64}$'),
    FOREIGN KEY(operation_id,authorization_id,billing_snapshot_version)
        REFERENCES cloud_handoff_commit_requests(operation_id,authorization_id,billing_snapshot_version) ON DELETE RESTRICT
);
CREATE TABLE cloud_handoff_finalization_acknowledgments (
    operation_id UUID NOT NULL REFERENCES cloud_handoff_committed_decisions(operation_id) ON DELETE RESTRICT,
    participant TEXT NOT NULL,
    receipt_sha256 TEXT NOT NULL CHECK(receipt_sha256 ~ '^[0-9a-f]{64}$'),
    acknowledged_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(operation_id,participant),
    FOREIGN KEY(operation_id,participant) REFERENCES cloud_handoff_participants(operation_id,participant) ON DELETE RESTRICT
);
CREATE TRIGGER cloud_handoff_commit_request_immutable BEFORE UPDATE OR DELETE ON cloud_handoff_commit_requests
    FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();
CREATE TRIGGER cloud_handoff_committed_decision_immutable BEFORE UPDATE OR DELETE ON cloud_handoff_committed_decisions
    FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();
CREATE TRIGGER cloud_handoff_finalization_ack_immutable BEFORE UPDATE OR DELETE ON cloud_handoff_finalization_acknowledgments
    FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();

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
        OR NOT ((OLD.phase='preparing' AND NEW.phase IN ('committing','canceling'))
            OR (OLD.phase='committing' AND NEW.phase IN ('finalizing','canceling'))
            OR (OLD.phase='finalizing' AND NEW.phase='succeeded')
            OR (OLD.phase='canceling' AND NEW.phase='canceled'))
        OR NEW.version<>OLD.version+1) THEN
        RAISE EXCEPTION 'invalid cloud handoff transition' USING ERRCODE='23514';
    END IF;
    PERFORM id FROM users WHERE id IN (NEW.source_user_id,NEW.target_user_id) ORDER BY id FOR UPDATE;
    PERFORM id FROM organizations WHERE id=NEW.brand_cloud_id FOR UPDATE;
    IF TG_OP='UPDATE' AND NEW.phase IN ('committing','finalizing') AND (
        NOT EXISTS(SELECT 1 FROM cloud_handoff_commit_requests WHERE operation_id=NEW.id)
        OR NOT EXISTS(SELECT 1 FROM cloud_handoff_participants WHERE operation_id=NEW.id AND participant='billing')
        OR NOT EXISTS(SELECT 1 FROM cloud_handoff_participants WHERE operation_id=NEW.id AND participant<>'billing')
        OR EXISTS(SELECT 1 FROM cloud_handoff_participants p WHERE p.operation_id=NEW.id AND NOT EXISTS(
            SELECT 1 FROM cloud_handoff_prepare_acknowledgments a WHERE a.operation_id=p.operation_id AND a.participant=p.participant))
        OR EXISTS(SELECT actor FROM (VALUES(NEW.source_user_id),(NEW.target_user_id)) participants(actor)
            WHERE NOT EXISTS(SELECT 1 FROM cloud_handoff_commit_requests c
                JOIN cloud_handoff_confirmation_requests r ON r.operation_id=c.operation_id AND r.billing_snapshot_version=c.billing_snapshot_version
                JOIN cloud_handoff_confirmation_acknowledgments a ON a.request_id=r.id
                WHERE c.operation_id=NEW.id AND r.user_id=participants.actor))) THEN
        RAISE EXCEPTION 'handoff commit requires durable preparation and both consents' USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' AND NEW.phase='finalizing' AND NOT EXISTS(
        SELECT 1 FROM cloud_handoff_committed_decisions d JOIN organizations o ON o.id=NEW.brand_cloud_id
        JOIN organization_members m ON m.organization_id=o.id AND m.role='owner'
        WHERE d.operation_id=NEW.id AND d.committed_ownership_version=NEW.ownership_version+1
        AND o.ownership_version=d.committed_ownership_version AND m.user_id=NEW.target_user_id
        AND d.committed_at>=NEW.cutoff
        AND NOT EXISTS(SELECT 1 FROM organization_members WHERE organization_id=o.id AND user_id=NEW.source_user_id)
        AND NOT EXISTS(SELECT 1 FROM role_assignments WHERE organization_id=o.id AND disabled_at IS NULL
            AND ((actor_type='user' AND actor_id=NEW.source_user_id::text) OR (actor_type='service_account' AND created_by=NEW.source_user_id)))) THEN
        RAISE EXCEPTION 'handoff committed decision does not match ownership and revocation' USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' AND NEW.phase IN ('canceling','canceled') AND EXISTS(
        SELECT 1 FROM cloud_handoff_committed_decisions WHERE operation_id=NEW.id) THEN
        RAISE EXCEPTION 'committed handoff cannot be canceled' USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' AND NEW.phase='canceled' AND (
        NOT EXISTS(SELECT 1 FROM cloud_handoff_participants WHERE operation_id=NEW.id AND participant='billing')
        OR NOT EXISTS(SELECT 1 FROM cloud_handoff_participants WHERE operation_id=NEW.id AND participant<>'billing')
        OR EXISTS(SELECT 1 FROM cloud_handoff_participants p WHERE p.operation_id=NEW.id AND NOT EXISTS(
            SELECT 1 FROM cloud_handoff_abort_acknowledgments a WHERE a.operation_id=p.operation_id AND a.participant=p.participant))) THEN
        RAISE EXCEPTION 'cloud handoff holds have not all been released' USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' AND NEW.phase='succeeded' AND (
        NOT EXISTS(SELECT 1 FROM cloud_handoff_committed_decisions WHERE operation_id=NEW.id)
        OR EXISTS(SELECT 1 FROM cloud_handoff_participants p WHERE p.operation_id=NEW.id AND NOT EXISTS(
            SELECT 1 FROM cloud_handoff_finalization_acknowledgments a WHERE a.operation_id=p.operation_id AND a.participant=p.participant))) THEN
        RAISE EXCEPTION 'handoff finalization is incomplete' USING ERRCODE='23514';
    END IF;
    UPDATE organizations SET authorization_version=authorization_version+1 WHERE id=NEW.brand_cloud_id;
    RETURN NEW;
END;
$$;

-- A committed decision must never survive without the matching owner change.
CREATE FUNCTION verify_cloud_handoff_committed_decision() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS(SELECT 1 FROM cloud_ownership_handoffs h JOIN organizations o ON o.id=h.brand_cloud_id
        JOIN organization_members m ON m.organization_id=o.id AND m.role='owner'
        WHERE h.id=NEW.operation_id AND h.phase IN ('finalizing','succeeded')
        AND NEW.committed_ownership_version=h.ownership_version+1 AND o.ownership_version=NEW.committed_ownership_version
        AND m.user_id=h.target_user_id AND NEW.committed_at>=h.cutoff) THEN
        RAISE EXCEPTION 'handoff decision requires atomic owner commit' USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER cloud_handoff_decision_matches_owner AFTER INSERT ON cloud_handoff_committed_decisions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION verify_cloud_handoff_committed_decision();

CREATE OR REPLACE FUNCTION user_can_access_brand_cloud(actor TEXT,cloud TEXT) RETURNS BOOLEAN LANGUAGE sql STABLE AS $$
    SELECT user_can_access_brand_cloud_without_handoff(actor,cloud)
        AND NOT EXISTS(SELECT 1 FROM cloud_ownership_handoffs h WHERE h.brand_cloud_id::text=cloud AND h.phase NOT IN ('canceled','succeeded'));
$$;
