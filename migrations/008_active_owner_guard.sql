CREATE OR REPLACE FUNCTION prevent_last_owner_removal()
RETURNS trigger AS $$
DECLARE
    remaining_owner_count integer;
BEGIN
    IF TG_OP = 'DELETE' OR (TG_OP = 'UPDATE' AND OLD.role = 'owner' AND NEW.role <> 'owner') THEN
        SELECT count(*)
        INTO remaining_owner_count
        FROM organization_members m
        JOIN users ON users.id = m.user_id
        WHERE m.organization_id = OLD.organization_id
          AND m.role = 'owner'
          AND m.user_id <> OLD.user_id
          AND users.disabled_at IS NULL;

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
