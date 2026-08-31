-- Product roles are subordinate to a cloud-owner-approved collaboration scope.
-- Existing explicit Product assignments are migration evidence; a broad cloud
-- membership/ACL alone never invents access to every Product.
ALTER TABLE device_item_profiles ADD CONSTRAINT device_item_profiles_cloud_identity_key
    UNIQUE (id, brand_cloud_id);

CREATE TABLE brand_cloud_product_admissions (
    organization_id UUID NOT NULL,
    user_id UUID NOT NULL,
    product_id UUID NOT NULL,
    provenance TEXT NOT NULL CHECK (provenance IN ('owner_invitation', 'legacy_explicit_product_assignment')),
    approved_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, user_id, product_id),
    FOREIGN KEY (organization_id, user_id) REFERENCES organization_members(organization_id, user_id) ON DELETE CASCADE,
    FOREIGN KEY (product_id, organization_id) REFERENCES device_item_profiles(id, brand_cloud_id) ON DELETE CASCADE,
    CHECK (provenance <> 'owner_invitation' OR approved_by IS NOT NULL)
);

INSERT INTO brand_cloud_product_admissions (organization_id,user_id,product_id,provenance)
SELECT DISTINCT m.organization_id,m.user_id,p.id,'legacy_explicit_product_assignment'
FROM organization_members m
JOIN organizations o ON o.id=m.organization_id AND o.organization_kind='brand_cloud'
JOIN role_assignments ra ON ra.actor_type='user' AND ra.actor_id=m.user_id::text
    AND ra.organization_id=m.organization_id AND ra.scope_type='product' AND ra.disabled_at IS NULL
JOIN roles r ON r.id=ra.role_id AND r.disabled_at IS NULL
JOIN device_item_profiles p ON p.id::text=ra.scope_id AND p.brand_cloud_id=m.organization_id
WHERE m.role <> 'owner';

CREATE FUNCTION user_can_access_brand_cloud_product(actor TEXT, cloud TEXT, product TEXT)
RETURNS BOOLEAN LANGUAGE sql STABLE AS $$
    SELECT user_can_access_brand_cloud(actor,cloud) AND EXISTS (
        SELECT 1 FROM organizations o WHERE o.id::text=cloud AND (
            o.organization_kind <> 'brand_cloud' OR (
                (COALESCE(product,'')='' OR EXISTS (SELECT 1 FROM device_item_profiles p WHERE p.id::text=product AND p.brand_cloud_id=o.id))
                AND (
                    EXISTS (SELECT 1 FROM organization_members m WHERE m.organization_id=o.id AND m.user_id::text=actor AND m.role='owner')
                    OR EXISTS (SELECT 1 FROM brand_cloud_product_admissions a WHERE a.organization_id=o.id AND a.user_id::text=actor AND a.product_id::text=product)
                )
            )
        )
    );
$$;

-- Membership deletion is revocation, unlike temporary suspension. Keeping old
-- disabled ACL/audit records is deliberate; a later INSERT cannot revive them.
CREATE FUNCTION revoke_removed_brand_cloud_member_grants()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM organizations WHERE id=OLD.organization_id AND organization_kind='brand_cloud') THEN
        UPDATE role_assignments SET disabled_at=COALESCE(disabled_at,now()),updated_at=now()
        WHERE actor_type='user' AND actor_id=OLD.user_id::text AND organization_id=OLD.organization_id;
        UPDATE product_collaborator_invitations SET status='canceled',canceled_at=now(),updated_at=now()
        WHERE brand_cloud_id=OLD.organization_id AND status='pending'
            AND (target_user_id=OLD.user_id OR invited_by_user_id=OLD.user_id);
        UPDATE brand_cloud_member_invitations SET status='canceled',canceled_at=now(),updated_at=now()
        WHERE brand_cloud_id=OLD.organization_id AND status='pending'
            AND (target_user_id=OLD.user_id OR invited_by_user_id=OLD.user_id);
    END IF;
    RETURN OLD;
END;
$$;
CREATE TRIGGER brand_cloud_removed_member_grants
    AFTER DELETE ON organization_members
    FOR EACH ROW EXECUTE FUNCTION revoke_removed_brand_cloud_member_grants();
