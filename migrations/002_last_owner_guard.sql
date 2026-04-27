CREATE OR REPLACE FUNCTION prevent_last_owner_removal()
RETURNS trigger AS $$
DECLARE
    remaining_owner_count integer;
BEGIN
    IF TG_OP = 'DELETE' OR (TG_OP = 'UPDATE' AND OLD.role = 'owner' AND NEW.role <> 'owner') THEN
        SELECT count(*)
        INTO remaining_owner_count
        FROM organization_members
        WHERE organization_id = OLD.organization_id
          AND role = 'owner'
          AND user_id <> OLD.user_id;

        IF remaining_owner_count = 0 THEN
            RAISE EXCEPTION 'last owner cannot be removed or downgraded'
                USING ERRCODE = '23514';
        END IF;
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS organization_members_prevent_last_owner_delete
    ON organization_members;
CREATE TRIGGER organization_members_prevent_last_owner_delete
    BEFORE DELETE ON organization_members
    FOR EACH ROW
    EXECUTE FUNCTION prevent_last_owner_removal();

DROP TRIGGER IF EXISTS organization_members_prevent_last_owner_update
    ON organization_members;
CREATE TRIGGER organization_members_prevent_last_owner_update
    BEFORE UPDATE OF role ON organization_members
    FOR EACH ROW
    EXECUTE FUNCTION prevent_last_owner_removal();
