CREATE OR REPLACE FUNCTION organizations_require_owner()
RETURNS TRIGGER AS $$
DECLARE
    target_org_id UUID;
BEGIN
    IF TG_TABLE_NAME = 'organizations' THEN
        IF TG_OP = 'DELETE' THEN
            target_org_id := OLD.id;
        ELSE
            target_org_id := NEW.id;
        END IF;
    ELSE
        IF TG_OP = 'DELETE' THEN
            target_org_id := OLD.organization_id;
        ELSE
            target_org_id := NEW.organization_id;
        END IF;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM organizations WHERE id = target_org_id) THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM organization_members
        WHERE organization_id = target_org_id AND role = 'owner'
    ) THEN
        RAISE EXCEPTION 'organization must have at least one owner';
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS organizations_require_owner_after_write
    ON organizations;
CREATE CONSTRAINT TRIGGER organizations_require_owner_after_write
    AFTER INSERT OR UPDATE ON organizations
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION organizations_require_owner();

DROP TRIGGER IF EXISTS organization_members_require_owner_after_write
    ON organization_members;
CREATE CONSTRAINT TRIGGER organization_members_require_owner_after_write
    AFTER INSERT OR UPDATE OR DELETE ON organization_members
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION organizations_require_owner();
