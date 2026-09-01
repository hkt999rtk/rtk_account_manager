-- Forward-only multi-cloud foundation. Published migration markers stay intact.
-- Deployment preflight must resolve ambiguous ownership; never choose an owner.
ALTER TABLE organizations
    ADD COLUMN description TEXT NOT NULL DEFAULT '',
    ADD COLUMN deleted_at TIMESTAMPTZ,
    ADD COLUMN ownership_version BIGINT NOT NULL DEFAULT 1 CHECK (ownership_version > 0),
    ADD COLUMN authorization_version BIGINT NOT NULL DEFAULT 1 CHECK (authorization_version > 0);

DO $$
BEGIN
    IF EXISTS (
        SELECT o.id FROM organizations o
        LEFT JOIN organization_members m ON m.organization_id = o.id AND m.role = 'owner'
        WHERE o.organization_kind = 'brand_cloud' AND o.deleted_at IS NULL
        GROUP BY o.id HAVING count(m.user_id) <> 1
    ) THEN
        RAISE EXCEPTION 'multi-cloud preflight: every non-deleted Brand Cloud requires exactly one designated owner'
            USING ERRCODE = '23514';
    END IF;
END $$;

-- Serialize SQL writes as well as application writes. Updating the version (not
-- merely locking) also forces stale REPEATABLE READ writers to retry. User locks
-- precede cloud locks, in UUID order, for quota and multi-row membership moves.
CREATE FUNCTION serialize_brand_cloud_membership_write()
RETURNS TRIGGER AS $$
DECLARE
    old_org UUID;
    new_org UUID;
    old_owner UUID;
    new_owner UUID;
    cloud_id UUID;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        old_org := OLD.organization_id;
        IF OLD.role = 'owner' THEN old_owner := OLD.user_id; END IF;
    END IF;
    IF TG_OP <> 'DELETE' THEN
        new_org := NEW.organization_id;
        IF NEW.role = 'owner' THEN new_owner := NEW.user_id; END IF;
    END IF;
    IF EXISTS (SELECT 1 FROM organizations WHERE id IN (old_org, new_org)
               AND organization_kind = 'brand_cloud') THEN
        PERFORM id FROM users WHERE id IN (old_owner, new_owner) ORDER BY id FOR UPDATE;
        FOR cloud_id IN SELECT id FROM organizations
                        WHERE id IN (old_org, new_org) AND organization_kind = 'brand_cloud'
                        ORDER BY id
        LOOP
            UPDATE organizations SET authorization_version = authorization_version + 1
            WHERE id = cloud_id;
        END LOOP;
    END IF;
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- This is an eligibility boundary, not a permission grant. Existing ACL/resource
-- checks still apply. Keep legacy customer organization behavior unchanged.
CREATE FUNCTION user_can_access_brand_cloud(actor TEXT, cloud TEXT)
RETURNS BOOLEAN LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1 FROM organizations o WHERE o.id::text = cloud AND (
            o.organization_kind <> 'brand_cloud' OR (
                o.deleted_at IS NULL AND o.status = 'active'
                AND EXISTS (
                    SELECT 1 FROM organization_members m JOIN users u ON u.id=m.user_id
                    WHERE m.organization_id=o.id AND m.user_id::text=actor
                      AND m.disabled_at IS NULL AND u.disabled_at IS NULL
                      AND u.email_verified AND NOT u.signup_pending_verification
                )
                AND (SELECT count(*) FROM organization_members m
                     WHERE m.organization_id=o.id AND m.role='owner') = 1
                AND EXISTS (
                    SELECT 1 FROM organization_members m JOIN users u ON u.id=m.user_id
                    WHERE m.organization_id=o.id AND m.role='owner'
                      AND m.disabled_at IS NULL AND u.disabled_at IS NULL
                      AND u.email_verified AND NOT u.signup_pending_verification
                )
            )
        )
    );
$$;

CREATE TRIGGER a_brand_cloud_membership_serialize
    BEFORE INSERT OR UPDATE OR DELETE ON organization_members
    FOR EACH ROW EXECUTE FUNCTION serialize_brand_cloud_membership_write();

-- Keep the published immediate last-owner behavior for customer_org only.
-- Brand Cloud transfers may temporarily have zero/two owners inside one tx;
-- the deferred constraint below must observe exactly one at commit.
CREATE OR REPLACE FUNCTION prevent_last_owner_removal()
RETURNS TRIGGER AS $$
DECLARE remaining_owner_count INTEGER;
BEGIN
    IF EXISTS (SELECT 1 FROM organizations WHERE id = OLD.organization_id
               AND organization_kind = 'brand_cloud') THEN
        IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' OR (TG_OP = 'UPDATE' AND OLD.role = 'owner' AND NEW.role <> 'owner') THEN
        SELECT count(*) INTO remaining_owner_count
        FROM organization_members m JOIN users u ON u.id = m.user_id
        WHERE m.organization_id = OLD.organization_id AND m.role = 'owner'
          AND m.user_id <> OLD.user_id AND u.disabled_at IS NULL;
        IF remaining_owner_count = 0 THEN
            RAISE EXCEPTION 'last owner cannot be removed or downgraded' USING ERRCODE = '23514';
        END IF;
    END IF;
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION organizations_require_owner()
RETURNS TRIGGER AS $$
DECLARE
    old_org UUID;
    new_org UUID;
    org RECORD;
    owner_count INTEGER;
BEGIN
    IF TG_TABLE_NAME = 'organizations' THEN
        IF TG_OP <> 'INSERT' THEN old_org := OLD.id; END IF;
        IF TG_OP <> 'DELETE' THEN new_org := NEW.id; END IF;
    ELSE
        IF TG_OP <> 'INSERT' THEN old_org := OLD.organization_id; END IF;
        IF TG_OP <> 'DELETE' THEN new_org := NEW.organization_id; END IF;
    END IF;
    FOR org IN SELECT id, organization_kind, deleted_at FROM organizations
               WHERE id IN (old_org, new_org) ORDER BY id
    LOOP
        SELECT count(*) INTO owner_count FROM organization_members
        WHERE organization_id = org.id AND role = 'owner';
        IF org.organization_kind = 'brand_cloud' AND org.deleted_at IS NULL AND owner_count <> 1 THEN
            RAISE EXCEPTION 'Brand Cloud must have exactly one designated owner' USING ERRCODE = '23514';
        END IF;
        IF org.organization_kind = 'customer_org' AND owner_count = 0 THEN
            RAISE EXCEPTION 'organization must have at least one owner';
        END IF;
    END LOOP;
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
