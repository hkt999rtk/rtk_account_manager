-- Protocol progress/cancellation must win over a stale worker's retry backoff.
-- Generation changes scheduling only; leases, authorization and evidence remain
-- governed by their existing constraints. Never wake on blocker-only updates.
ALTER TABLE cloud_deletion_jobs ADD COLUMN generation BIGINT NOT NULL DEFAULT 0 CHECK(generation>=0);
CREATE FUNCTION wake_cloud_deletion_job() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE operation UUID;
BEGIN
    IF TG_TABLE_NAME='cloud_deletion_operations' THEN
        IF NEW.phase IS NOT DISTINCT FROM OLD.phase THEN RETURN NULL; END IF;
        operation:=NEW.id;
    ELSE
        operation:=NEW.operation_id;
    END IF;
    UPDATE cloud_deletion_jobs SET generation=generation+1,available_at=clock_timestamp() WHERE operation_id=operation;
    RETURN NULL;
END $$;
CREATE TRIGGER cloud_deletion_phase_wake AFTER UPDATE OF phase ON cloud_deletion_operations FOR EACH ROW EXECUTE FUNCTION wake_cloud_deletion_job();
CREATE TRIGGER cloud_deletion_retirement_wake AFTER INSERT ON cloud_deletion_command_retirements FOR EACH ROW EXECUTE FUNCTION wake_cloud_deletion_job();
CREATE TRIGGER cloud_deletion_release_wake AFTER INSERT ON cloud_deletion_release_receipts FOR EACH ROW EXECUTE FUNCTION wake_cloud_deletion_job();
