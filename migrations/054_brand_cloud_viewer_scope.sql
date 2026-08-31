-- Viewer is a cloud membership ceiling, not a writable Product role. Its
-- accepted scope grants reads without materializing roles for future Products.
CREATE FUNCTION valid_cloud_viewer_scope(scope JSONB) RETURNS BOOLEAN
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE ids JSONB; item JSONB; seen TEXT[] := '{}';
BEGIN
    IF scope IS NULL OR jsonb_typeof(scope)<>'object' THEN RETURN false; END IF;
    IF scope->>'kind'='all_products' THEN RETURN scope='{"kind":"all_products"}'::jsonb; END IF;
    IF scope->>'kind' IS DISTINCT FROM 'selected_products' OR scope-'kind'-'product_ids'<>'{}'::jsonb THEN RETURN false; END IF;
    ids:=scope->'product_ids';
    IF ids IS NULL OR jsonb_typeof(ids)<>'array' THEN RETURN false; END IF;
    IF jsonb_array_length(ids)=0 THEN RETURN false; END IF;
    FOR item IN SELECT value FROM jsonb_array_elements(ids) LOOP
        IF jsonb_typeof(item)<>'string' OR (item#>>'{}') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN RETURN false; END IF;
        IF (item#>>'{}')=ANY(seen) THEN RETURN false; END IF;
        seen:=array_append(seen,item#>>'{}');
    END LOOP;
    RETURN true;
END; $$;

ALTER TABLE organization_members ADD COLUMN access_scope JSONB;
ALTER TABLE organization_members DROP CONSTRAINT organization_members_role_check;
ALTER TABLE organization_members ADD CONSTRAINT organization_members_role_check CHECK(role IN ('owner','admin','member','viewer'));
ALTER TABLE organization_members ADD CONSTRAINT organization_members_viewer_scope_check CHECK (
    (role='viewer' AND valid_cloud_viewer_scope(access_scope)) OR (role<>'viewer' AND access_scope IS NULL));
ALTER TABLE brand_cloud_member_invitations ADD COLUMN access_scope JSONB;
ALTER TABLE brand_cloud_member_invitations DROP CONSTRAINT brand_cloud_member_invitations_role_check;
ALTER TABLE brand_cloud_member_invitations ADD CONSTRAINT brand_cloud_member_invitations_role_check CHECK(role IN ('admin','member','viewer'));
ALTER TABLE brand_cloud_member_invitations ADD CONSTRAINT brand_cloud_member_invitations_viewer_scope_check CHECK (
    (role='viewer' AND valid_cloud_viewer_scope(access_scope)) OR (role<>'viewer' AND access_scope IS NULL));

CREATE FUNCTION validate_brand_cloud_viewer_products() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE cloud UUID;
BEGIN
    IF NEW.role<>'viewer' THEN RETURN NEW; END IF;
    IF TG_TABLE_NAME='organization_members' THEN cloud:=NEW.organization_id; ELSE cloud:=NEW.brand_cloud_id; END IF;
    PERFORM 1 FROM organizations WHERE id=cloud AND organization_kind='brand_cloud' FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'viewer requires Brand Cloud' USING ERRCODE='23514'; END IF;
    IF NOT valid_cloud_viewer_scope(NEW.access_scope) THEN RAISE EXCEPTION 'invalid viewer scope' USING ERRCODE='23514'; END IF;
    IF NEW.access_scope->>'kind'='selected_products' AND EXISTS (
        SELECT 1 FROM jsonb_array_elements_text(NEW.access_scope->'product_ids') x(id)
        WHERE NOT EXISTS(SELECT 1 FROM device_item_profiles p WHERE p.id::text=x.id AND p.brand_cloud_id=cloud)
    ) THEN RAISE EXCEPTION 'viewer Product outside cloud' USING ERRCODE='23514'; END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER validate_member_viewer_products BEFORE INSERT OR UPDATE OF role,access_scope ON organization_members
FOR EACH ROW EXECUTE FUNCTION validate_brand_cloud_viewer_products();
CREATE TRIGGER validate_invitation_viewer_products BEFORE INSERT OR UPDATE OF role,access_scope ON brand_cloud_member_invitations
FOR EACH ROW EXECUTE FUNCTION validate_brand_cloud_viewer_products();

CREATE FUNCTION invalidate_changed_viewer_grants() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF (OLD.role='viewer' OR NEW.role='viewer') AND (OLD.role IS DISTINCT FROM NEW.role OR OLD.access_scope IS DISTINCT FROM NEW.access_scope) THEN
        DELETE FROM brand_cloud_product_admissions WHERE organization_id=NEW.organization_id AND user_id=NEW.user_id;
        UPDATE role_assignments SET disabled_at=COALESCE(disabled_at,now()),updated_at=now()
        WHERE actor_type='user' AND actor_id=NEW.user_id::text AND organization_id=NEW.organization_id AND scope_type<>'organization';
        UPDATE product_collaborator_invitations SET status='canceled',canceled_at=now(),updated_at=now()
        WHERE brand_cloud_id=NEW.organization_id AND status='pending' AND (target_user_id=NEW.user_id OR invited_by_user_id=NEW.user_id);
    END IF;
    RETURN NULL;
END; $$;
CREATE TRIGGER invalidate_changed_viewer_grants AFTER UPDATE OF role,access_scope ON organization_members
FOR EACH ROW EXECUTE FUNCTION invalidate_changed_viewer_grants();

INSERT INTO roles(name,scope_type,description,system_role) VALUES('viewer','organization','Cloud-owner-scoped read-only collaborator',true);
INSERT INTO role_permissions(role_id,permission_id)
SELECT r.id,p.id FROM roles r CROSS JOIN permissions p WHERE r.name='viewer'
AND p.name IN ('registry_device.read','device_group.read','device_tag.read','lifecycle_operation.inspect');

CREATE OR REPLACE FUNCTION sync_membership_role_assignment() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE target_org UUID; target_user UUID; target_role UUID; kind TEXT;
BEGIN
    IF TG_OP='DELETE' THEN target_org:=OLD.organization_id; target_user:=OLD.user_id;
    ELSE target_org:=NEW.organization_id; target_user:=NEW.user_id; END IF;
    UPDATE role_assignments SET disabled_at=COALESCE(disabled_at,now()),updated_at=now()
    WHERE actor_type='user' AND actor_id=target_user::text AND scope_type='organization'
      AND scope_id=target_org::text AND role_id IN(SELECT id FROM roles WHERE name IN('owner','admin','member','viewer')) AND disabled_at IS NULL;
    IF TG_OP<>'DELETE' THEN
        SELECT organization_kind INTO kind FROM organizations WHERE id=NEW.organization_id;
        IF kind<>'brand_cloud' OR NEW.role IN('owner','viewer') THEN
            SELECT id INTO target_role FROM roles WHERE name=NEW.role AND disabled_at IS NULL;
            IF target_role IS NOT NULL THEN
                INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type,scope_id,organization_id)
                VALUES(target_role,'user',NEW.user_id::text,'organization',NEW.organization_id::text,NEW.organization_id) ON CONFLICT DO NOTHING;
            END IF;
        END IF;
    END IF;
    RETURN NULL;
END; $$;

CREATE FUNCTION brand_cloud_permission_allowed(actor TEXT,cloud TEXT,permission TEXT) RETURNS BOOLEAN LANGUAGE sql STABLE AS $$
    SELECT NOT EXISTS(SELECT 1 FROM organization_members m WHERE m.organization_id::text=cloud AND m.user_id::text=actor AND m.role='viewer')
        OR permission IN('registry_device.read','device_group.read','device_tag.read','lifecycle_operation.inspect');
$$;

CREATE OR REPLACE FUNCTION user_can_access_brand_cloud_product(actor TEXT,cloud TEXT,product TEXT) RETURNS BOOLEAN LANGUAGE sql STABLE AS $$
    SELECT user_can_access_brand_cloud(actor,cloud) AND EXISTS (
        SELECT 1 FROM organizations o WHERE o.id::text=cloud AND (
            o.organization_kind<>'brand_cloud' OR (
                (COALESCE(product,'')='' OR EXISTS(SELECT 1 FROM device_item_profiles p WHERE p.id::text=product AND p.brand_cloud_id=o.id))
                AND EXISTS(SELECT 1 FROM organization_members m WHERE m.organization_id=o.id AND m.user_id::text=actor AND (
                    m.role='owner'
                    OR (m.role='viewer' AND COALESCE(product,'')<>'' AND (
                        m.access_scope->>'kind'='all_products' OR m.access_scope->'product_ids' ? product))
                    OR (m.role IN('admin','member') AND EXISTS(SELECT 1 FROM brand_cloud_product_admissions a
                        WHERE a.organization_id=o.id AND a.user_id=m.user_id AND a.product_id::text=product))
                ))
            )
        )
    );
$$;
