CREATE TABLE platform_bootstrap (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    sealed_at timestamptz,
    initial_user_id uuid,
    reason text,
    CHECK ((sealed_at IS NULL AND reason IS NULL) OR
           (sealed_at IS NOT NULL AND reason IS NOT NULL))
);
-- Existing installations are sealed even when their administrators are disabled.
-- Disabling the last account must never reopen the bootstrap path.
INSERT INTO platform_bootstrap(singleton, sealed_at, reason)
SELECT true,
       CASE WHEN EXISTS(SELECT 1 FROM users WHERE platform_admin) OR
                      EXISTS(SELECT 1 FROM role_assignments a JOIN roles r ON r.id=a.role_id
                             WHERE r.name='platform_admin' AND a.scope_type='platform')
            THEN now() END,
       CASE WHEN EXISTS(SELECT 1 FROM users WHERE platform_admin) OR
                      EXISTS(SELECT 1 FROM role_assignments a JOIN roles r ON r.id=a.role_id
                             WHERE r.name='platform_admin' AND a.scope_type='platform')
            THEN 'existing_installation' END;

CREATE FUNCTION protect_platform_bootstrap() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' OR OLD.sealed_at IS NOT NULL THEN
        RAISE EXCEPTION 'platform bootstrap seal is immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER platform_bootstrap_immutable BEFORE UPDATE OR DELETE ON platform_bootstrap
FOR EACH ROW EXECUTE FUNCTION protect_platform_bootstrap();
