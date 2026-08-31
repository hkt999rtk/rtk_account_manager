-- Preserve 061 commands/history; replacements require durable Billing retirement.
ALTER TABLE cloud_deletion_operations DROP CONSTRAINT cloud_deletion_operations_phase_check;
ALTER TABLE cloud_deletion_operations ADD CONSTRAINT cloud_deletion_operations_phase_check CHECK(phase IN ('preparing','closing','canceling','canceled','succeeded'));
ALTER TABLE cloud_deletion_operations DROP CONSTRAINT cloud_deletion_operations_brand_cloud_id_key;
CREATE UNIQUE INDEX cloud_deletion_one_active ON cloud_deletion_operations(brand_cloud_id) WHERE phase<>'canceled';
CREATE TABLE cloud_deletion_close_attempts (
    operation_id UUID NOT NULL REFERENCES cloud_deletion_operations(id) ON DELETE RESTRICT,
    readiness_sha256 TEXT NOT NULL CHECK(readiness_sha256 ~ '^[0-9a-f]{64}$'),
    settlement_id UUID NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(operation_id,readiness_sha256)
);
INSERT INTO cloud_deletion_close_attempts(operation_id,readiness_sha256,settlement_id,created_at)
    SELECT operation_id,readiness_sha256,settlement_id,created_at FROM cloud_deletion_close_commands;
CREATE TABLE cloud_deletion_command_retirements (
    operation_id UUID NOT NULL, readiness_sha256 TEXT NOT NULL,
    receipt_sha256 TEXT NOT NULL CHECK(receipt_sha256 ~ '^[0-9a-f]{64}$'),
    retired_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(operation_id,readiness_sha256),
    FOREIGN KEY(operation_id,readiness_sha256) REFERENCES cloud_deletion_close_attempts(operation_id,readiness_sha256) ON DELETE RESTRICT
);
CREATE TABLE cloud_deletion_cancellations (
    operation_id UUID PRIMARY KEY REFERENCES cloud_deletion_operations(id) ON DELETE RESTRICT,
    id UUID NOT NULL UNIQUE DEFAULT gen_random_uuid(),
    decision_sha256 TEXT NOT NULL CHECK(decision_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE cloud_deletion_release_receipts (
    operation_id UUID NOT NULL REFERENCES cloud_deletion_cancellations(operation_id) ON DELETE RESTRICT,
    participant TEXT NOT NULL,
    receipt_sha256 TEXT NOT NULL CHECK(receipt_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(operation_id,participant)
);
CREATE TRIGGER cloud_deletion_attempts_immutable BEFORE UPDATE OR DELETE ON cloud_deletion_close_attempts FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();
CREATE TRIGGER cloud_deletion_retirements_immutable BEFORE UPDATE OR DELETE ON cloud_deletion_command_retirements FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();
CREATE TRIGGER cloud_deletion_cancel_immutable BEFORE UPDATE OR DELETE ON cloud_deletion_cancellations FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();
CREATE TRIGGER cloud_deletion_release_immutable BEFORE UPDATE OR DELETE ON cloud_deletion_release_receipts FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();

CREATE FUNCTION guard_cloud_deletion_recovery_receipt() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE op cloud_deletion_operations%ROWTYPE;
BEGIN
    SELECT * INTO STRICT op FROM cloud_deletion_operations WHERE id=NEW.operation_id;
    PERFORM id FROM users WHERE id=op.owner_user_id FOR UPDATE;
    PERFORM id FROM organizations WHERE id=op.brand_cloud_id FOR UPDATE;
    IF EXISTS(SELECT 1 FROM cloud_deletion_completions WHERE operation_id=NEW.operation_id) THEN
        RAISE EXCEPTION 'completed deletion cannot gain retirement or release evidence' USING ERRCODE='23514';
    END IF;
    IF TG_TABLE_NAME='cloud_deletion_command_retirements' THEN
        IF NOT EXISTS(SELECT 1 FROM cloud_deletion_operations WHERE id=NEW.operation_id AND phase IN ('closing','canceling')) THEN
            RAISE EXCEPTION 'retirement requires a pending close command' USING ERRCODE='23514';
        END IF;
    ELSE
        IF NOT EXISTS(SELECT 1 FROM cloud_deletion_operations WHERE id=NEW.operation_id AND phase='canceling')
            OR (NEW.participant<>'billing' AND NOT EXISTS(SELECT 1 FROM cloud_deletion_participants WHERE operation_id=NEW.operation_id AND participant=NEW.participant))
            OR EXISTS(SELECT 1 FROM cloud_deletion_close_attempts a WHERE a.operation_id=NEW.operation_id AND NOT EXISTS(
                SELECT 1 FROM cloud_deletion_command_retirements r WHERE r.operation_id=a.operation_id AND r.readiness_sha256=a.readiness_sha256)) THEN
            RAISE EXCEPTION 'release requires cancellation and retirement of every pending command' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER cloud_deletion_retirement_guard BEFORE INSERT ON cloud_deletion_command_retirements FOR EACH ROW EXECUTE FUNCTION guard_cloud_deletion_recovery_receipt();
CREATE TRIGGER cloud_deletion_release_guard BEFORE INSERT ON cloud_deletion_release_receipts FOR EACH ROW EXECUTE FUNCTION guard_cloud_deletion_recovery_receipt();

CREATE FUNCTION guard_cloud_deletion_attempt() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE op cloud_deletion_operations%ROWTYPE;
BEGIN
    SELECT * INTO STRICT op FROM cloud_deletion_operations WHERE id=NEW.operation_id;
    PERFORM id FROM users WHERE id=op.owner_user_id FOR UPDATE;
    PERFORM id FROM organizations WHERE id=op.brand_cloud_id FOR UPDATE;
    IF NOT EXISTS(SELECT 1 FROM cloud_deletion_operations WHERE id=NEW.operation_id AND phase IN ('preparing','closing'))
       OR EXISTS(SELECT 1 FROM cloud_deletion_cancellations WHERE operation_id=NEW.operation_id)
       OR EXISTS(SELECT 1 FROM cloud_deletion_close_attempts a WHERE a.operation_id=NEW.operation_id AND NOT EXISTS(
           SELECT 1 FROM cloud_deletion_command_retirements r WHERE r.operation_id=a.operation_id AND r.readiness_sha256=a.readiness_sha256)) THEN
        RAISE EXCEPTION 'previous close must be retired before replacement; cancellation prohibits new commands' USING ERRCODE='23514';
    END IF;
    -- A lock alone does not invalidate a waiting REPEATABLE READ snapshot.
    UPDATE organizations SET updated_at=clock_timestamp() WHERE id=op.brand_cloud_id;
    RETURN NEW;
END $$;
CREATE TRIGGER cloud_deletion_attempt_guard BEFORE INSERT ON cloud_deletion_close_attempts FOR EACH ROW EXECUTE FUNCTION guard_cloud_deletion_attempt();
CREATE FUNCTION copy_initial_cloud_deletion_command() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO cloud_deletion_close_attempts(operation_id,readiness_sha256,settlement_id,created_at)
        VALUES(NEW.operation_id,NEW.readiness_sha256,NEW.settlement_id,NEW.created_at);
    RETURN NULL;
END $$;
CREATE TRIGGER cloud_deletion_initial_attempt AFTER INSERT ON cloud_deletion_close_commands FOR EACH ROW EXECUTE FUNCTION copy_initial_cloud_deletion_command();

CREATE OR REPLACE FUNCTION guard_cloud_deletion_operation() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN RAISE EXCEPTION 'deletion history cannot be removed' USING ERRCODE='23514'; END IF;
    PERFORM id FROM users WHERE id=NEW.owner_user_id FOR UPDATE;
    PERFORM id FROM organizations WHERE id=NEW.brand_cloud_id FOR UPDATE;
    IF TG_OP='INSERT' THEN
        IF NEW.phase<>'preparing' OR NOT EXISTS(SELECT 1 FROM organizations o JOIN organization_members m ON m.organization_id=o.id AND m.role='owner'
            WHERE o.id=NEW.brand_cloud_id AND o.organization_kind='brand_cloud' AND o.deleted_at IS NULL
            AND o.ownership_version=NEW.ownership_version AND o.authorization_version=NEW.authorization_version AND m.user_id=NEW.owner_user_id
            AND user_can_access_brand_cloud(NEW.owner_user_id::text,o.id::text)) THEN
            RAISE EXCEPTION 'invalid deletion owner or lifecycle' USING ERRCODE='23514';
        END IF;
        UPDATE organizations SET authorization_version=authorization_version+1 WHERE id=NEW.brand_cloud_id;
    ELSE
        IF ROW(NEW.id,NEW.brand_cloud_id,NEW.owner_user_id,NEW.ownership_version,NEW.authorization_version,NEW.idempotency_key,NEW.cutoff,NEW.request_sha256,NEW.created_at)
            IS DISTINCT FROM ROW(OLD.id,OLD.brand_cloud_id,OLD.owner_user_id,OLD.ownership_version,OLD.authorization_version,OLD.idempotency_key,OLD.cutoff,OLD.request_sha256,OLD.created_at)
            OR NOT (NEW.phase=OLD.phase OR (OLD.phase='preparing' AND NEW.phase IN ('closing','canceling'))
                OR (OLD.phase='closing' AND NEW.phase IN ('succeeded','canceling')) OR (OLD.phase='canceling' AND NEW.phase IN ('canceled','succeeded'))) THEN
            RAISE EXCEPTION 'invalid deletion transition' USING ERRCODE='23514';
        END IF;
        IF NEW.phase IN ('closing','succeeded') AND (NOT EXISTS(SELECT 1 FROM cloud_deletion_close_commands WHERE operation_id=NEW.id)
            OR NOT EXISTS(SELECT 1 FROM cloud_deletion_participants WHERE operation_id=NEW.id)
            OR EXISTS(SELECT 1 FROM cloud_deletion_participants p WHERE p.operation_id=NEW.id AND NOT EXISTS(
                SELECT 1 FROM cloud_deletion_resource_receipts r WHERE r.operation_id=p.operation_id AND r.participant=p.participant))) THEN
            RAISE EXCEPTION 'closure command needs all producer holds' USING ERRCODE='23514';
        END IF;
        IF NEW.phase IN ('canceling','canceled') AND (NOT EXISTS(SELECT 1 FROM cloud_deletion_cancellations WHERE operation_id=NEW.id)
            OR EXISTS(SELECT 1 FROM cloud_deletion_completions WHERE operation_id=NEW.id)) THEN
            RAISE EXCEPTION 'completed deletion cannot be canceled' USING ERRCODE='23514';
        END IF;
        IF NEW.phase='canceled' AND (NOT EXISTS(SELECT 1 FROM cloud_deletion_release_receipts WHERE operation_id=NEW.id AND participant='billing')
            OR EXISTS(SELECT 1 FROM cloud_deletion_participants p WHERE p.operation_id=NEW.id AND NOT EXISTS(
                SELECT 1 FROM cloud_deletion_release_receipts r WHERE r.operation_id=p.operation_id AND r.participant=p.participant))
            OR EXISTS(SELECT 1 FROM cloud_deletion_close_attempts a WHERE a.operation_id=NEW.id AND NOT EXISTS(
                SELECT 1 FROM cloud_deletion_command_retirements r WHERE r.operation_id=a.operation_id AND r.readiness_sha256=a.readiness_sha256))) THEN
            RAISE EXCEPTION 'all close attempts and remote holds must be resolved before cancellation completes' USING ERRCODE='23514';
        END IF;
        IF NEW.phase='succeeded' AND NOT EXISTS(SELECT 1 FROM cloud_deletion_completions c JOIN organizations o ON o.id=NEW.brand_cloud_id
            WHERE c.operation_id=NEW.id AND o.deleted_at=c.deleted_at) THEN
            RAISE EXCEPTION 'deletion requires Billing completion and matching tombstone' USING ERRCODE='23514';
        END IF;
        IF NEW.phase='canceled' AND OLD.phase<>'canceled' THEN
            UPDATE organizations SET authorization_version=authorization_version+1 WHERE id=NEW.brand_cloud_id;
        END IF;
        IF NEW.phase<>OLD.phase AND NEW.phase IN ('closing','canceling') THEN
            UPDATE organizations SET updated_at=clock_timestamp() WHERE id=NEW.brand_cloud_id;
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION user_can_access_brand_cloud(actor TEXT,cloud TEXT) RETURNS BOOLEAN LANGUAGE sql STABLE AS $$
    SELECT user_can_access_brand_cloud_without_handoff(actor,cloud)
        AND NOT EXISTS(SELECT 1 FROM cloud_ownership_handoffs h WHERE h.brand_cloud_id::text=cloud AND h.phase NOT IN ('canceled','succeeded'))
        AND NOT EXISTS(SELECT 1 FROM cloud_deletion_operations d WHERE d.brand_cloud_id::text=cloud AND d.phase<>'canceled');
$$;
CREATE OR REPLACE FUNCTION fence_cloud_deletion_write() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE target UUID;
BEGIN
    IF TG_TABLE_NAME IN ('device_item_profiles','factory_production_runs','brand_cloud_member_invitations','brand_cloud_owner_transfers','cloud_ownership_handoffs') THEN target:=NEW.brand_cloud_id;
    ELSE target:=NEW.organization_id; END IF;
    PERFORM id FROM organizations WHERE id=target FOR UPDATE;
    IF EXISTS(SELECT 1 FROM cloud_deletion_operations WHERE brand_cloud_id=target AND phase<>'canceled')
       OR EXISTS(SELECT 1 FROM organizations WHERE id=target AND organization_kind='brand_cloud' AND deleted_at IS NOT NULL) THEN
        RAISE EXCEPTION 'cloud deletion fences new work or ownership' USING ERRCODE='55000',CONSTRAINT='cloud_deletion_fence';
    END IF;
    RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION guard_cloud_deletion_tombstone() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.organization_kind<>'brand_cloud' THEN RETURN NEW; END IF;
    IF OLD.deleted_at IS NOT NULL AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'deleted cloud cannot be modified or restored' USING ERRCODE='55000',CONSTRAINT='cloud_deletion_fence';
    END IF;
    IF NEW.deleted_at IS DISTINCT FROM OLD.deleted_at AND NOT EXISTS(SELECT 1 FROM cloud_deletion_operations d JOIN cloud_deletion_completions c ON c.operation_id=d.id
        WHERE d.brand_cloud_id=OLD.id AND d.phase IN ('closing','canceling') AND c.deleted_at=NEW.deleted_at) THEN
        RAISE EXCEPTION 'tombstone requires durable Billing closure evidence' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
