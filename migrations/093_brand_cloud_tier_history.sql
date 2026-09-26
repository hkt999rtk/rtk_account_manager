-- Billing may only treat a tier as historical evidence from this migration
-- forward. Existing organization rows receive a migration-time observation;
-- their earlier tier is deliberately unknown.
CREATE TABLE brand_cloud_tier_events (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    brand_cloud_id UUID NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    tier TEXT NOT NULL CHECK (tier IN ('evaluation', 'commercial')),
    observed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX brand_cloud_tier_events_history_idx
    ON brand_cloud_tier_events (brand_cloud_id, observed_at DESC, id DESC);

INSERT INTO brand_cloud_tier_events (brand_cloud_id, tier, observed_at)
SELECT id, tier, clock_timestamp()
FROM organizations
WHERE organization_kind = 'brand_cloud';

CREATE FUNCTION record_brand_cloud_tier_event() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' AND NEW.organization_kind = 'brand_cloud' THEN
        INSERT INTO brand_cloud_tier_events (brand_cloud_id, tier)
        VALUES (NEW.id, NEW.tier);
    ELSIF TG_OP = 'UPDATE' AND NEW.organization_kind = 'brand_cloud' AND
          (OLD.organization_kind IS DISTINCT FROM 'brand_cloud' OR OLD.tier IS DISTINCT FROM NEW.tier) THEN
        INSERT INTO brand_cloud_tier_events (brand_cloud_id, tier)
        VALUES (NEW.id, NEW.tier);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER organizations_record_billing_tier
    AFTER INSERT OR UPDATE OF tier, organization_kind ON organizations
    FOR EACH ROW EXECUTE FUNCTION record_brand_cloud_tier_event();

CREATE FUNCTION reject_brand_cloud_tier_event_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'brand_cloud_tier_events are append-only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER brand_cloud_tier_events_immutable
    BEFORE UPDATE OR DELETE ON brand_cloud_tier_events
    FOR EACH ROW EXECUTE FUNCTION reject_brand_cloud_tier_event_mutation();
