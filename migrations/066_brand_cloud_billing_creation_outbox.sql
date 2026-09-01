-- Future brand-cloud creation only. Existing clouds have no inferred billing
-- owner history and must use the reviewed migration/provenance workflow.
CREATE TABLE brand_cloud_billing_creation_outbox (
    event_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    cloud_id UUID NOT NULL UNIQUE REFERENCES organizations(id) ON DELETE RESTRICT,
    owner_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    ownership_version BIGINT NOT NULL DEFAULT 1 CHECK(ownership_version=1),
    occurred_at TIMESTAMPTZ NOT NULL,
    available_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    lease_id UUID, lease_until TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts>=0),
    delivered_at TIMESTAMPTZ, billing_account_id UUID, receipt_sha256 TEXT,
    CHECK((lease_id IS NULL)=(lease_until IS NULL)),
    CHECK((delivered_at IS NULL AND billing_account_id IS NULL AND receipt_sha256 IS NULL)
       OR (delivered_at IS NOT NULL AND billing_account_id IS NOT NULL AND receipt_sha256 IS NOT NULL AND receipt_sha256 ~ '^[a-f0-9]{64}$'))
);
CREATE INDEX brand_cloud_billing_creation_ready ON brand_cloud_billing_creation_outbox(available_at)
    WHERE delivered_at IS NULL;
CREATE FUNCTION guard_brand_cloud_billing_creation_event() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN RAISE EXCEPTION 'billing creation evidence is immutable'; END IF;
    IF ROW(NEW.event_id,NEW.cloud_id,NEW.owner_user_id,NEW.ownership_version,NEW.occurred_at)
       IS DISTINCT FROM ROW(OLD.event_id,OLD.cloud_id,OLD.owner_user_id,OLD.ownership_version,OLD.occurred_at)
       OR OLD.delivered_at IS NOT NULL
    THEN RAISE EXCEPTION 'billing creation evidence is immutable'; END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER brand_cloud_billing_creation_immutable BEFORE UPDATE OR DELETE ON brand_cloud_billing_creation_outbox
    FOR EACH ROW EXECUTE FUNCTION guard_brand_cloud_billing_creation_event();
CREATE FUNCTION enqueue_brand_cloud_billing_creation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE initial_owner UUID; cloud_created_at TIMESTAMPTZ;
BEGIN
    IF NEW.organization_kind<>'brand_cloud' THEN RETURN NULL; END IF;
    -- Deferred: the complete organization + unique owner transaction is visible.
    BEGIN
        SELECT o.created_at,m.user_id INTO STRICT cloud_created_at,initial_owner
          FROM organizations o JOIN organization_members m ON m.organization_id=o.id AND m.role='owner'
          WHERE o.id=NEW.id AND o.organization_kind='brand_cloud';
    EXCEPTION WHEN NO_DATA_FOUND OR TOO_MANY_ROWS THEN
        -- This trigger may run before the existing deferred owner validator;
        -- preserve its constraint-violation contract, not a PL/pgSQL error.
        RAISE EXCEPTION 'Brand Cloud must have exactly one designated owner' USING ERRCODE='23514';
    END;
    INSERT INTO brand_cloud_billing_creation_outbox(cloud_id,owner_user_id,occurred_at)
      VALUES(NEW.id,initial_owner,cloud_created_at);
    RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER brand_cloud_billing_creation_enqueue AFTER INSERT ON organizations
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION enqueue_brand_cloud_billing_creation();
