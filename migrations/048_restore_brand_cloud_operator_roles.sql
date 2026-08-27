-- Tenant-local Brand Cloud users are operational identities. SKU project
-- collaboration applies to developer (`user`) actors, so preserve the legacy
-- organization role projection for Brand Cloud login sessions.
INSERT INTO role_assignments (role_id, actor_type, actor_id, scope_type, scope_id, organization_id)
SELECT r.id, 'brand_cloud_user', bcm.brand_cloud_user_id::text,
       'organization', bcm.brand_cloud_id::text, bcm.brand_cloud_id
FROM brand_cloud_memberships bcm
JOIN roles r ON r.name = bcm.role AND r.disabled_at IS NULL
ON CONFLICT DO NOTHING;

CREATE OR REPLACE FUNCTION sync_brand_cloud_membership_role_assignment()
RETURNS trigger AS $$
DECLARE
    target_role_id UUID;
BEGIN
    SELECT id INTO target_role_id FROM roles WHERE name = NEW.role AND disabled_at IS NULL;
    IF target_role_id IS NULL THEN
        RETURN NEW;
    END IF;

    UPDATE role_assignments
    SET disabled_at = now(), updated_at = now()
    WHERE actor_type = 'brand_cloud_user'
      AND actor_id = NEW.brand_cloud_user_id::text
      AND scope_type = 'organization'
      AND scope_id = NEW.brand_cloud_id::text
      AND disabled_at IS NULL
      AND role_id <> target_role_id;

    INSERT INTO role_assignments (role_id, actor_type, actor_id, scope_type, scope_id, organization_id)
    VALUES (target_role_id, 'brand_cloud_user', NEW.brand_cloud_user_id::text,
            'organization', NEW.brand_cloud_id::text, NEW.brand_cloud_id)
    ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
