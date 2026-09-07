CREATE TABLE platform_admin_recovery (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 target_user_id uuid NOT NULL REFERENCES users(id),
 requested_by uuid NOT NULL REFERENCES users(id),
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 2048),
 digest text NOT NULL CHECK(digest ~ '^[a-f0-9]{64}$'),
 idempotency_key text NOT NULL,
 status text NOT NULL DEFAULT 'requested' CHECK(status IN ('requested','completed','cancelled')),
 created_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL,
 UNIQUE(requested_by,idempotency_key),
 CHECK(target_user_id<>requested_by)
);
CREATE TABLE platform_admin_recovery_approvals (
 request_id uuid NOT NULL REFERENCES platform_admin_recovery(id),
 principal_id uuid NOT NULL REFERENCES users(id),
 role text NOT NULL CHECK(role IN ('pki_admin','security_custodian')),
 digest text NOT NULL,
 auth_time timestamptz NOT NULL,
 approved_at timestamptz NOT NULL,
 PRIMARY KEY(request_id,principal_id),
 UNIQUE(request_id,role)
);
CREATE TABLE platform_admin_recovery_audit (
 sequence bigserial PRIMARY KEY,
 request_id uuid NOT NULL REFERENCES platform_admin_recovery(id),
 actor_id uuid NOT NULL REFERENCES users(id),
 event text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE FUNCTION protect_admin_recovery_history() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_TABLE_NAME='platform_admin_recovery' AND TG_OP='UPDATE' THEN
  IF (to_jsonb(OLD)-'status')=(to_jsonb(NEW)-'status') AND OLD.status='requested'
     AND NEW.status IN ('completed','cancelled') THEN RETURN NEW; END IF;
 END IF;
 RAISE EXCEPTION 'administrator recovery history is immutable';
END;
$$;
CREATE TRIGGER recovery_request_immutable BEFORE UPDATE OR DELETE ON platform_admin_recovery
FOR EACH ROW EXECUTE FUNCTION protect_admin_recovery_history();
CREATE TRIGGER recovery_approvals_immutable BEFORE UPDATE OR DELETE ON platform_admin_recovery_approvals
FOR EACH ROW EXECUTE FUNCTION protect_admin_recovery_history();
CREATE TRIGGER recovery_audit_immutable BEFORE UPDATE OR DELETE ON platform_admin_recovery_audit
FOR EACH ROW EXECUTE FUNCTION protect_admin_recovery_history();
