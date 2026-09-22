-- Run through the offline migrator after application writers have stopped.
SET LOCAL lock_timeout = '5s';
LOCK TABLE audit_events, acl_audit_events IN ACCESS EXCLUSIVE MODE;
DROP INDEX IF EXISTS identity_providers_provider_id_idx;
DROP INDEX IF EXISTS device_tag_catalog_org_tag_idx;
ALTER TABLE audit_events ADD COLUMN audit_domain TEXT NOT NULL DEFAULT 'general';
ALTER TABLE audit_events ADD CONSTRAINT audit_events_domain_check CHECK (audit_domain IN ('general','acl'));
ALTER TABLE audit_events ADD CONSTRAINT audit_events_acl_fields_check CHECK (
 audit_domain <> 'acl' OR (btrim(event_type)<>'' AND btrim(subject_type)<>'' AND btrim(subject_id)<>''));
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM audit_events a JOIN acl_audit_events c USING(id)) THEN
  RAISE EXCEPTION 'audit event identity collision; reconcile before migration';
 END IF;
END $$;
INSERT INTO audit_events(id,event_type,actor_user_id,organization_id,subject_type,subject_id,payload,created_at,updated_at,audit_domain)
 SELECT id,event_type,actor_user_id,organization_id,subject_type,subject_id,payload,created_at,updated_at,'acl' FROM acl_audit_events;
DO $$ BEGIN
 IF EXISTS(SELECT id,event_type,actor_user_id,organization_id,subject_type,subject_id,payload,created_at,updated_at FROM acl_audit_events
 EXCEPT SELECT id,event_type,actor_user_id,organization_id,subject_type,subject_id,payload,created_at,updated_at FROM audit_events WHERE audit_domain='acl') THEN
  RAISE EXCEPTION 'audit copy verification failed';
 END IF;
END $$;
DROP TABLE acl_audit_events;
CREATE INDEX audit_events_domain_created_idx ON audit_events(audit_domain,created_at);
CREATE INDEX audit_events_acl_org_idx ON audit_events(organization_id,created_at DESC) WHERE audit_domain='acl';
-- Validate new ACL actors without deleting their historical identity later.
CREATE FUNCTION validate_acl_audit_actor() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.audit_domain='acl' AND NEW.actor_user_id IS NOT NULL THEN
  PERFORM 1 FROM users WHERE id=NEW.actor_user_id FOR KEY SHARE;
  IF NOT FOUND THEN
   RAISE EXCEPTION 'ACL audit actor does not exist' USING ERRCODE='23503';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER audit_events_validate_acl_actor BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION validate_acl_audit_actor();
