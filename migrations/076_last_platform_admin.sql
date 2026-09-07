-- A write lock (not just an advisory/read lock) makes concurrent destructive
-- changes serialize even under repeatable-read transactions.
CREATE TABLE platform_admin_guard (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 generation bigint NOT NULL DEFAULT 0
);
INSERT INTO platform_admin_guard(singleton) VALUES(true);

CREATE FUNCTION protect_last_platform_admin() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
 protected_change boolean := false;
 survives boolean;
 excluded_user text := '';
 excluded_assignment uuid;
BEGIN
 IF NOT EXISTS(SELECT 1 FROM platform_bootstrap WHERE singleton AND sealed_at IS NOT NULL) THEN
  IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
 END IF;

 IF TG_TABLE_NAME='users' THEN
  IF OLD.platform_admin AND OLD.disabled_at IS NULL AND NOT OLD.signup_pending_verification THEN
   IF TG_OP='DELETE' THEN protected_change := true;
   ELSE protected_change := NOT NEW.platform_admin OR NEW.disabled_at IS NOT NULL OR NEW.signup_pending_verification;
   END IF;
  END IF;
  excluded_user := OLD.id::text;
 ELSIF TG_TABLE_NAME='role_assignments' THEN
  IF OLD.actor_type='user' AND OLD.scope_type='platform' AND OLD.disabled_at IS NULL
     AND EXISTS(SELECT 1 FROM roles WHERE id=OLD.role_id AND name='platform_admin' AND disabled_at IS NULL) THEN
   IF TG_OP='DELETE' THEN protected_change := true;
   ELSE protected_change := NEW.disabled_at IS NOT NULL OR NEW.role_id<>OLD.role_id OR NEW.actor_type<>OLD.actor_type
                           OR NEW.actor_id<>OLD.actor_id OR NEW.scope_type<>OLD.scope_type;
   END IF;
  END IF;
  excluded_assignment := OLD.id;
 ELSIF TG_TABLE_NAME='roles' AND OLD.name='platform_admin' THEN
  -- This is a singleton system role: disabling or renaming it removes every
  -- administrator's canonical assignment, regardless of the legacy user flag.
  IF TG_OP='DELETE' THEN protected_change := true;
  ELSE protected_change := NEW.disabled_at IS NOT NULL OR NEW.name<>OLD.name OR NEW.scope_type<>OLD.scope_type;
  END IF;
  IF protected_change THEN
   RAISE EXCEPTION 'platform administrator system role is protected' USING ERRCODE='23514', CONSTRAINT='platform_admin_preserved';
  END IF;
 END IF;

 IF protected_change THEN
  UPDATE platform_admin_guard SET generation=generation+1 WHERE singleton;
  IF NOT FOUND THEN RAISE EXCEPTION 'platform administrator guard unavailable'; END IF;
  SELECT EXISTS(
   SELECT 1 FROM users u JOIN role_assignments a ON a.actor_id=u.id::text
   JOIN roles r ON r.id=a.role_id
   WHERE u.platform_admin AND u.disabled_at IS NULL AND NOT u.signup_pending_verification
    AND u.id::text<>excluded_user AND a.actor_type='user' AND a.scope_type='platform'
    AND a.disabled_at IS NULL AND r.name='platform_admin' AND r.disabled_at IS NULL
    AND (excluded_assignment IS NULL OR a.id<>excluded_assignment)
  ) INTO survives;
  IF NOT survives THEN
   RAISE EXCEPTION 'last platform administrator cannot be removed' USING ERRCODE='23514', CONSTRAINT='platform_admin_preserved';
  END IF;
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
END;
$$;

CREATE TRIGGER users_last_platform_admin BEFORE UPDATE OR DELETE ON users
FOR EACH ROW EXECUTE FUNCTION protect_last_platform_admin();
CREATE TRIGGER assignments_last_platform_admin BEFORE UPDATE OR DELETE ON role_assignments
FOR EACH ROW EXECUTE FUNCTION protect_last_platform_admin();
CREATE TRIGGER roles_last_platform_admin BEFORE UPDATE OR DELETE ON roles
FOR EACH ROW EXECUTE FUNCTION protect_last_platform_admin();
