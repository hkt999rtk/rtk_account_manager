-- Ownership responsibility can only move after every reviewed monetary and
-- resource boundary has supplied durable evidence. A reduced inventory is not
-- a compatibility mode.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM cloud_ownership_handoffs h
        WHERE ARRAY(
            SELECT p.participant
            FROM cloud_handoff_participants p
            WHERE p.operation_id=h.id
            ORDER BY p.participant
        ) <> ARRAY['billing','factory','mqtt_usage','video_control_plane']::TEXT[]
    ) THEN
        RAISE EXCEPTION 'existing cloud handoff has an unreviewed participant inventory'
            USING ERRCODE='23514';
    END IF;
END;
$$;

CREATE FUNCTION verify_exact_cloud_handoff_participants() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    operation UUID;
BEGIN
    operation := COALESCE((to_jsonb(NEW)->>'operation_id')::UUID,(to_jsonb(NEW)->>'id')::UUID);
    IF ARRAY(
        SELECT p.participant
        FROM cloud_handoff_participants p
        WHERE p.operation_id=operation
        ORDER BY p.participant
    ) <> ARRAY['billing','factory','mqtt_usage','video_control_plane']::TEXT[] THEN
        RAISE EXCEPTION 'cloud handoff requires the exact reviewed participant inventory'
            USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER cloud_handoff_exact_inventory_from_operation
    AFTER INSERT ON cloud_ownership_handoffs
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION verify_exact_cloud_handoff_participants();

CREATE CONSTRAINT TRIGGER cloud_handoff_exact_inventory_from_participant
    AFTER INSERT ON cloud_handoff_participants
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION verify_exact_cloud_handoff_participants();
