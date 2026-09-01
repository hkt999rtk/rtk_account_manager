-- Durable AM deletion intents. No resource cascade or historical owner rewrite.
CREATE TABLE cloud_deletion_operations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    brand_cloud_id UUID NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    owner_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    ownership_version BIGINT NOT NULL CHECK(ownership_version>0),
    authorization_version BIGINT NOT NULL CHECK(authorization_version>0),
    idempotency_key TEXT NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 200),
    cutoff TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    request_sha256 TEXT NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
    phase TEXT NOT NULL DEFAULT 'preparing' CHECK(phase IN ('preparing','closing','succeeded')),
    blockers JSONB NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(blockers)='array'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(brand_cloud_id), UNIQUE(owner_user_id,brand_cloud_id,idempotency_key)
);
CREATE TABLE cloud_deletion_participants (
    operation_id UUID NOT NULL REFERENCES cloud_deletion_operations(id) ON DELETE RESTRICT,
    participant TEXT NOT NULL CHECK(participant ~ '^[a-z][a-z0-9_-]{0,63}$' AND participant<>'billing'),
    PRIMARY KEY(operation_id,participant)
);
-- One immutable hold/drain acknowledgment per producer. Holds do not expire:
-- the producer must persist the fence and reject new work until lifecycle resolution.
CREATE TABLE cloud_deletion_resource_receipts (
    operation_id UUID NOT NULL, participant TEXT NOT NULL,
    receipt_sha256 TEXT NOT NULL CHECK(receipt_sha256 ~ '^[0-9a-f]{64}$'),
    acknowledged_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(operation_id,participant),
    FOREIGN KEY(operation_id,participant) REFERENCES cloud_deletion_participants(operation_id,participant) ON DELETE RESTRICT
);
-- Outbox decision survives lost close replies; never select a new receipt while
-- the original command might already have closed Billing.
CREATE TABLE cloud_deletion_close_commands (
    operation_id UUID PRIMARY KEY REFERENCES cloud_deletion_operations(id) ON DELETE RESTRICT,
    settlement_id UUID NOT NULL,
    readiness_sha256 TEXT NOT NULL CHECK(readiness_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE cloud_deletion_completions (
    operation_id UUID PRIMARY KEY REFERENCES cloud_deletion_close_commands(operation_id) ON DELETE RESTRICT,
    billing_closed_at TIMESTAMPTZ NOT NULL,
    billing_receipt_sha256 TEXT NOT NULL CHECK(billing_receipt_sha256 ~ '^[0-9a-f]{64}$'),
    deleted_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE TRIGGER cloud_deletion_participants_immutable BEFORE UPDATE OR DELETE ON cloud_deletion_participants FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();
CREATE TRIGGER cloud_deletion_resources_immutable BEFORE UPDATE OR DELETE ON cloud_deletion_resource_receipts FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();
CREATE TRIGGER cloud_deletion_commands_immutable BEFORE UPDATE OR DELETE ON cloud_deletion_close_commands FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();
CREATE TRIGGER cloud_deletion_completions_immutable BEFORE UPDATE OR DELETE ON cloud_deletion_completions FOR EACH ROW EXECUTE FUNCTION reject_cloud_handoff_evidence_mutation();

CREATE FUNCTION guard_cloud_deletion_operation() RETURNS TRIGGER LANGUAGE plpgsql AS $$
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
            OR NOT (NEW.phase=OLD.phase OR (OLD.phase='preparing' AND NEW.phase='closing') OR (OLD.phase='closing' AND NEW.phase='succeeded')) THEN
            RAISE EXCEPTION 'invalid deletion transition' USING ERRCODE='23514';
        END IF;
        IF NEW.phase IN ('closing','succeeded') AND (NOT EXISTS(SELECT 1 FROM cloud_deletion_close_commands WHERE operation_id=NEW.id)
            OR NOT EXISTS(SELECT 1 FROM cloud_deletion_participants WHERE operation_id=NEW.id)
            OR EXISTS(SELECT 1 FROM cloud_deletion_participants p WHERE p.operation_id=NEW.id AND NOT EXISTS(
                SELECT 1 FROM cloud_deletion_resource_receipts r WHERE r.operation_id=p.operation_id AND r.participant=p.participant))) THEN
            RAISE EXCEPTION 'closure command needs all producer holds' USING ERRCODE='23514';
        END IF;
        IF NEW.phase='succeeded' AND NOT EXISTS(SELECT 1 FROM cloud_deletion_completions c JOIN organizations o ON o.id=NEW.brand_cloud_id
            WHERE c.operation_id=NEW.id AND o.deleted_at=c.deleted_at) THEN
            RAISE EXCEPTION 'deletion requires Billing completion and matching tombstone' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER cloud_deletion_transition BEFORE INSERT OR UPDATE OR DELETE ON cloud_deletion_operations FOR EACH ROW EXECUTE FUNCTION guard_cloud_deletion_operation();

CREATE OR REPLACE FUNCTION user_can_access_brand_cloud(actor TEXT,cloud TEXT) RETURNS BOOLEAN LANGUAGE sql STABLE AS $$
    SELECT user_can_access_brand_cloud_without_handoff(actor,cloud)
        AND NOT EXISTS(SELECT 1 FROM cloud_ownership_handoffs h WHERE h.brand_cloud_id::text=cloud AND h.phase NOT IN ('canceled','succeeded'))
        AND NOT EXISTS(SELECT 1 FROM cloud_deletion_operations d WHERE d.brand_cloud_id::text=cloud);
$$;

-- AM-owned resources serialize with admission. External producers must supply
-- independently authenticated persistent holds; these triggers do not prove that.
CREATE FUNCTION fence_cloud_deletion_write() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE target UUID;
BEGIN
    IF TG_TABLE_NAME IN ('device_item_profiles','factory_production_runs','brand_cloud_member_invitations','brand_cloud_owner_transfers','cloud_ownership_handoffs') THEN target:=NEW.brand_cloud_id;
    ELSE target:=NEW.organization_id; END IF;
    PERFORM id FROM organizations WHERE id=target FOR UPDATE;
    IF EXISTS(SELECT 1 FROM cloud_deletion_operations WHERE brand_cloud_id=target)
       OR EXISTS(SELECT 1 FROM organizations WHERE id=target AND organization_kind='brand_cloud' AND deleted_at IS NOT NULL) THEN
        RAISE EXCEPTION 'cloud deletion fences new work or ownership' USING ERRCODE='55000',CONSTRAINT='cloud_deletion_fence';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER deletion_product_fence BEFORE INSERT OR UPDATE OF brand_cloud_id ON device_item_profiles FOR EACH ROW EXECUTE FUNCTION fence_cloud_deletion_write();
CREATE TRIGGER deletion_device_fence BEFORE INSERT OR UPDATE OF organization_id ON devices FOR EACH ROW EXECUTE FUNCTION fence_cloud_deletion_write();
CREATE TRIGGER deletion_job_fence BEFORE INSERT OR UPDATE OF organization_id,status,completed_at,operation_type ON device_operations FOR EACH ROW EXECUTE FUNCTION fence_cloud_deletion_write();
CREATE TRIGGER deletion_factory_fence BEFORE INSERT OR UPDATE OF brand_cloud_id,status,valid_until ON factory_production_runs FOR EACH ROW EXECUTE FUNCTION fence_cloud_deletion_write();
CREATE TRIGGER deletion_invitation_fence BEFORE INSERT ON brand_cloud_member_invitations FOR EACH ROW EXECUTE FUNCTION fence_cloud_deletion_write();
CREATE TRIGGER deletion_transfer_fence BEFORE INSERT ON brand_cloud_owner_transfers FOR EACH ROW EXECUTE FUNCTION fence_cloud_deletion_write();
CREATE TRIGGER deletion_handoff_fence BEFORE INSERT ON cloud_ownership_handoffs FOR EACH ROW EXECUTE FUNCTION fence_cloud_deletion_write();
CREATE TRIGGER deletion_membership_fence BEFORE INSERT OR UPDATE OF organization_id,user_id,role ON organization_members FOR EACH ROW EXECUTE FUNCTION fence_cloud_deletion_write();
CREATE TRIGGER deletion_claim_fence BEFORE INSERT ON device_claim_tokens FOR EACH ROW EXECUTE FUNCTION fence_cloud_deletion_write();

CREATE FUNCTION guard_cloud_deletion_tombstone() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.organization_kind<>'brand_cloud' THEN RETURN NEW; END IF;
    IF OLD.deleted_at IS NOT NULL AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'deleted cloud cannot be modified or restored' USING ERRCODE='55000',CONSTRAINT='cloud_deletion_fence';
    END IF;
    IF NEW.deleted_at IS DISTINCT FROM OLD.deleted_at AND NOT EXISTS(SELECT 1 FROM cloud_deletion_operations d JOIN cloud_deletion_completions c ON c.operation_id=d.id
        WHERE d.brand_cloud_id=OLD.id AND d.phase='closing' AND c.deleted_at=NEW.deleted_at) THEN
        RAISE EXCEPTION 'tombstone requires durable Billing closure evidence' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER cloud_deletion_tombstone BEFORE UPDATE ON organizations FOR EACH ROW EXECUTE FUNCTION guard_cloud_deletion_tombstone();

CREATE FUNCTION verify_cloud_deletion_completion() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS(SELECT 1 FROM cloud_deletion_operations d JOIN organizations o ON o.id=d.brand_cloud_id
        WHERE d.id=NEW.operation_id AND d.phase='succeeded' AND o.deleted_at=NEW.deleted_at
          AND NOT EXISTS(SELECT 1 FROM device_item_profiles WHERE brand_cloud_id=o.id)
          AND NOT EXISTS(SELECT 1 FROM devices WHERE organization_id=o.id)
          AND NOT EXISTS(SELECT 1 FROM organization_members WHERE organization_id=o.id AND disabled_at IS NULL)
          AND NOT EXISTS(SELECT 1 FROM brand_cloud_member_invitations WHERE brand_cloud_id=o.id AND status='pending')) THEN
        RAISE EXCEPTION 'incomplete cloud tombstone' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE CONSTRAINT TRIGGER cloud_deletion_completion_consistency AFTER INSERT ON cloud_deletion_completions DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION verify_cloud_deletion_completion();

CREATE TABLE cloud_deletion_jobs (
    operation_id UUID PRIMARY KEY REFERENCES cloud_deletion_operations(id) ON DELETE RESTRICT,
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(), lease_id UUID, lease_until TIMESTAMPTZ,
    attempts BIGINT NOT NULL DEFAULT 0 CHECK(attempts>=0),
    CHECK((lease_id IS NULL)=(lease_until IS NULL))
);
CREATE INDEX cloud_deletion_jobs_ready ON cloud_deletion_jobs(available_at,operation_id);
CREATE FUNCTION schedule_cloud_deletion() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO cloud_deletion_jobs(operation_id) VALUES(NEW.id);
    RETURN NULL;
END $$;
CREATE TRIGGER cloud_deletion_schedule AFTER INSERT ON cloud_deletion_operations FOR EACH ROW EXECUTE FUNCTION schedule_cloud_deletion();
