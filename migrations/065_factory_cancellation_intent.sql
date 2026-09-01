-- A cancellation request is a durable admission fence, not non-issuance proof.
-- Keep all published migration markers and all immutable historical bindings.
ALTER TABLE factory_enrollment_reservations
	ADD COLUMN admitted BOOLEAN NOT NULL DEFAULT true,
    DROP CONSTRAINT factory_enrollment_reservations_status_check,
    DROP CONSTRAINT factory_enrollment_reservations_check,
    ADD CONSTRAINT factory_enrollment_reservations_status_check
        CHECK(status IN ('reserved','cancel_requested','issued','not_issued')),
    ADD CONSTRAINT factory_enrollment_reservations_check
        CHECK((status IN ('reserved','cancel_requested') AND completed_at IS NULL AND evidence_sha256 IS NULL)
            OR (status IN ('issued','not_issued') AND completed_at IS NOT NULL AND evidence_sha256 IS NOT NULL)),
	ADD CONSTRAINT factory_cancellation_never_admitted CHECK(admitted OR status IN ('cancel_requested','not_issued'));

DROP INDEX factory_enrollment_pending;
CREATE INDEX factory_enrollment_pending ON factory_enrollment_reservations(production_run_id)
    WHERE status IN ('reserved','cancel_requested');

CREATE OR REPLACE FUNCTION guard_factory_enrollment_reservation() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'factory enrollment history cannot be deleted' USING ERRCODE='23514';
    END IF;
    IF NOT ((OLD.status='reserved' AND NEW.status IN ('cancel_requested','issued','not_issued'))
        OR (OLD.status='cancel_requested' AND NEW.status IN ('issued','not_issued')))
        OR (NEW.id,NEW.production_run_id,NEW.request_id,NEW.device_id,NEW.request_sha256,NEW.created_at,NEW.admitted)
        IS DISTINCT FROM (OLD.id,OLD.production_run_id,OLD.request_id,OLD.device_id,OLD.request_sha256,OLD.created_at,OLD.admitted) THEN
        RAISE EXCEPTION 'invalid factory enrollment result transition' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
