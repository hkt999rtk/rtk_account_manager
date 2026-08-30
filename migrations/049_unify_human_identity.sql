-- Consolidate every legacy tenant-scoped human identity into the global users
-- table. This migration is deliberately idempotent so operators can run the
-- same SQL during preflight on a restored production snapshot.

CREATE TABLE IF NOT EXISTS brand_cloud_user_migrations (
    brand_cloud_user_id UUID PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    normalized_email TEXT NOT NULL,
    result TEXT NOT NULL,
    conflict_status TEXT NOT NULL,
    migrated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT brand_cloud_user_migrations_result_check
        CHECK (result IN ('existing_user', 'created_user')),
    CONSTRAINT brand_cloud_user_migrations_conflict_check
        CHECK (conflict_status IN ('none', 'password_reset_required'))
);

CREATE TEMP TABLE identity_cutover_existing_users ON COMMIT DROP AS
SELECT id, email FROM users;

WITH legacy_emails AS (
    SELECT
        lower(btrim(email)) AS email,
        min(password_hash) AS password_hash,
        count(DISTINCT password_hash) AS password_hash_count,
        bool_and(email_verified AND NOT signup_pending_verification) AS fully_verified,
        max(display_name) FILTER (WHERE display_name IS NOT NULL) AS display_name
    FROM brand_cloud_users
    GROUP BY lower(btrim(email))
), missing_users AS (
    SELECT le.*
    FROM legacy_emails le
    LEFT JOIN users u ON u.email = le.email
    WHERE u.id IS NULL
)
INSERT INTO users (
    email,
    password_hash,
    display_name,
    email_verified,
    email_verified_at,
    signup_pending_verification
)
SELECT
    email,
    CASE
        WHEN password_hash_count = 1 AND fully_verified THEN password_hash
        ELSE '!password-reset-required!' || gen_random_uuid()::text
    END,
    display_name,
    password_hash_count = 1 AND fully_verified,
    CASE WHEN password_hash_count = 1 AND fully_verified THEN now() ELSE NULL END,
    NOT (password_hash_count = 1 AND fully_verified)
FROM missing_users
ON CONFLICT (email) DO NOTHING;

INSERT INTO brand_cloud_user_migrations (
    brand_cloud_user_id,
    user_id,
    normalized_email,
    result,
    conflict_status
)
SELECT
    bcu.id,
    u.id,
    lower(btrim(bcu.email)),
    CASE WHEN existing.id IS NOT NULL THEN 'existing_user' ELSE 'created_user' END,
    CASE
        WHEN u.signup_pending_verification OR NOT u.email_verified
            THEN 'password_reset_required'
        ELSE 'none'
    END
FROM brand_cloud_users bcu
JOIN users u ON u.email = lower(btrim(bcu.email))
LEFT JOIN identity_cutover_existing_users existing ON existing.id = u.id
ON CONFLICT (brand_cloud_user_id) DO UPDATE SET
    user_id = EXCLUDED.user_id,
    normalized_email = EXCLUDED.normalized_email,
    conflict_status = EXCLUDED.conflict_status;

INSERT INTO organization_members (
    organization_id,
    user_id,
    role,
    created_at,
    updated_at,
    disabled_at
)
SELECT
    bcm.brand_cloud_id,
    map.user_id,
    bcm.role,
    bcm.created_at,
    now(),
    CASE
        WHEN map.conflict_status = 'password_reset_required' THEN now()
        ELSE bcu.disabled_at
    END
FROM brand_cloud_memberships bcm
JOIN brand_cloud_users bcu ON bcu.id = bcm.brand_cloud_user_id
JOIN brand_cloud_user_migrations map
  ON map.brand_cloud_user_id = bcm.brand_cloud_user_id
ON CONFLICT (organization_id, user_id) DO UPDATE SET
    role = CASE
        WHEN organization_members.role = 'owner' OR EXCLUDED.role = 'owner' THEN 'owner'
        WHEN organization_members.role = 'admin' OR EXCLUDED.role = 'admin' THEN 'admin'
        ELSE 'member'
    END,
    disabled_at = CASE
        WHEN organization_members.disabled_at IS NULL OR EXCLUDED.disabled_at IS NULL THEN NULL
        ELSE GREATEST(organization_members.disabled_at, EXCLUDED.disabled_at)
    END,
    updated_at = now();

UPDATE organization_members om
SET disabled_at = COALESCE(om.disabled_at, now()), updated_at = now()
FROM users u
WHERE u.id = om.user_id
  AND (u.signup_pending_verification OR NOT u.email_verified);

INSERT INTO role_assignments (
    role_id,
    actor_type,
    actor_id,
    scope_type,
    scope_id,
    organization_id,
    created_by,
    created_at,
    updated_at,
    disabled_at
)
SELECT
    ra.role_id,
    'user',
    map.user_id::text,
    ra.scope_type,
    ra.scope_id,
    ra.organization_id,
    ra.created_by,
    ra.created_at,
    now(),
    ra.disabled_at
FROM role_assignments ra
JOIN brand_cloud_user_migrations map
  ON ra.actor_type = 'brand_cloud_user'
 AND ra.actor_id = map.brand_cloud_user_id::text
ON CONFLICT DO NOTHING;

UPDATE role_assignments
SET disabled_at = COALESCE(disabled_at, now()), updated_at = now()
WHERE actor_type = 'brand_cloud_user';

UPDATE brand_cloud_refresh_tokens
SET revoked_at = COALESCE(revoked_at, now());

ALTER TABLE auth_tokens
    DROP CONSTRAINT IF EXISTS auth_tokens_subject_type_check;

UPDATE auth_tokens
SET subject_type = 'user'
WHERE subject_type = 'platform_user';

UPDATE app_certificates
SET revoked_at = COALESCE(revoked_at, now()), updated_at = now()
WHERE subject_type IN ('brand_cloud_user', 'platform_user');

ALTER TABLE app_certificates
    DROP CONSTRAINT IF EXISTS app_certificates_subject_type_check;

ALTER TABLE app_certificates
    ADD CONSTRAINT app_certificates_subject_type_check
    CHECK (subject_type IN ('platform_user', 'user', 'brand_cloud_user', 'end_user'));

DELETE FROM auth_tokens
WHERE subject_type = 'brand_cloud_user';

ALTER TABLE auth_tokens
    ADD CONSTRAINT auth_tokens_subject_type_check
    CHECK (subject_type = 'user');

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM organizations o
        WHERE o.organization_kind = 'brand_cloud'
          AND o.status = 'active'
          AND NOT EXISTS (
              SELECT 1
              FROM organization_members om
              JOIN users u ON u.id = om.user_id
              WHERE om.organization_id = o.id
                AND om.role = 'owner'
                AND om.disabled_at IS NULL
                AND u.disabled_at IS NULL
				AND u.email_verified = true
				AND u.signup_pending_verification = false
          )
    ) THEN
        RAISE EXCEPTION 'identity cutover refused: an active Brand Cloud has no enabled global owner';
    END IF;
END $$;

COMMENT ON TABLE brand_cloud_user_migrations IS
    'Auditable legacy Brand Cloud user to global user mapping retained through cutover validation.';
