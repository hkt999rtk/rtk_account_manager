CREATE OR REPLACE FUNCTION organizations_require_owner()
RETURNS TRIGGER AS $$
DECLARE
    target_org_id UUID;
    target_org_kind TEXT;
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

    SELECT organization_kind
    INTO target_org_kind
    FROM organizations
    WHERE id = target_org_id;

    IF target_org_kind IS NULL THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;

    IF target_org_kind = 'customer_org' AND NOT EXISTS (
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
