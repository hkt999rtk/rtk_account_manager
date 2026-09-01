-- Preserve historical runs; do not infer the authority of previously issued JWTs.
ALTER TABLE factory_production_runs
    ADD COLUMN authorization_ownership_version BIGINT CHECK (authorization_ownership_version > 0),
    ADD COLUMN authorization_platform_override BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE factory_enrollment_reservations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    production_run_id UUID NOT NULL REFERENCES factory_production_runs(id) ON DELETE RESTRICT,
    request_id TEXT NOT NULL CHECK (length(request_id) BETWEEN 1 AND 128),
    device_id TEXT NOT NULL CHECK (length(device_id) BETWEEN 1 AND 256),
    request_sha256 TEXT NOT NULL CHECK (request_sha256 ~ '^[a-f0-9]{64}$'),
    status TEXT NOT NULL DEFAULT 'reserved' CHECK (status IN ('reserved','issued','not_issued')),
    evidence_sha256 TEXT CHECK (evidence_sha256 ~ '^[a-f0-9]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    completed_at TIMESTAMPTZ,
    UNIQUE(production_run_id,request_id),
    CHECK ((status='reserved' AND completed_at IS NULL AND evidence_sha256 IS NULL)
        OR (status<>'reserved' AND completed_at IS NOT NULL AND evidence_sha256 IS NOT NULL))
);
CREATE INDEX factory_enrollment_pending ON factory_enrollment_reservations(production_run_id)
    WHERE status='reserved';

CREATE FUNCTION guard_factory_enrollment_reservation() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'factory enrollment history cannot be deleted' USING ERRCODE='23514';
    END IF;
    IF OLD.status<>'reserved' OR NEW.status='reserved'
        OR (NEW.id,NEW.production_run_id,NEW.request_id,NEW.device_id,NEW.request_sha256,NEW.created_at)
        IS DISTINCT FROM (OLD.id,OLD.production_run_id,OLD.request_id,OLD.device_id,OLD.request_sha256,OLD.created_at) THEN
        RAISE EXCEPTION 'invalid factory enrollment result transition' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER factory_enrollment_reservation_history BEFORE UPDATE OR DELETE ON factory_enrollment_reservations
    FOR EACH ROW EXECUTE FUNCTION guard_factory_enrollment_reservation();
