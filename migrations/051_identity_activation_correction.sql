-- Applied atomically by Migrate. Never delete the published 049 marker to
-- force a live rerun. Published 049 remains unchanged; pre-049 legacy cutovers need a separate reviewed path.
CREATE TEMP TABLE identity_correction_context ON COMMIT DROP AS
SELECT NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version='051_identity_activation_correction.sql')
   AND NOT EXISTS (SELECT 1 FROM audit_events WHERE event_type='identity_activation_holds_initialized'
                   AND subject_type='migration'
                   AND subject_id IN ('049_unify_human_identity.sql','051_identity_activation_correction.sql')) AS backfill_required;

CREATE TABLE IF NOT EXISTS organization_member_activation_holds (
    organization_id UUID NOT NULL,
    user_id UUID NOT NULL,
    disabled_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    source TEXT NOT NULL CHECK (source IN ('signup', 'provisioning', 'identity_migration')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, user_id),
    FOREIGN KEY (organization_id, user_id)
        REFERENCES organization_members(organization_id, user_id) ON DELETE CASCADE
);

-- All membership mutations invalidate an earlier hold, even an idempotent
-- administrative disable in the same transaction. A workflow that newly
-- suspends access records its new hold only after the member write succeeds.
CREATE OR REPLACE FUNCTION invalidate_organization_member_activation_hold()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM organization_member_activation_holds
    WHERE organization_id = OLD.organization_id AND user_id = OLD.user_id;
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS organization_member_activation_hold_invalidation ON organization_members;
CREATE TRIGGER organization_member_activation_hold_invalidation
AFTER UPDATE ON organization_members
FOR EACH ROW EXECUTE FUNCTION invalidate_organization_member_activation_hold();

CREATE OR REPLACE FUNCTION invalidate_disabled_user_activation_holds()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.disabled_at IS NOT NULL THEN
        DELETE FROM organization_member_activation_holds WHERE user_id = NEW.id;
    END IF;
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS disabled_user_activation_hold_invalidation ON users;
CREATE TRIGGER disabled_user_activation_hold_invalidation
AFTER UPDATE OF disabled_at ON users
FOR EACH ROW EXECUTE FUNCTION invalidate_disabled_user_activation_holds();

DO $$
DECLARE missing_owners JSONB;
BEGIN
    SELECT jsonb_agg(o.id ORDER BY o.id) INTO missing_owners
    FROM organizations o WHERE o.organization_kind='brand_cloud'
      AND to_jsonb(o)->>'deleted_at' IS NULL
      AND (SELECT count(*) FROM organization_members m
           WHERE m.organization_id=o.id AND m.role='owner') <> 1;
    IF missing_owners IS NOT NULL THEN
        RAISE EXCEPTION 'identity correction refused: preflight requires exactly one designated owner'
            USING DETAIL=missing_owners::text;
    END IF;
END $$;

CREATE TEMP TABLE identity_correction_candidates ON COMMIT DROP AS
WITH legacy AS (
    SELECT map.user_id, min(bcu.password_hash) AS password_hash,
           min(map.migrated_at) AS migrated_at, array_agg(bcu.id ORDER BY bcu.id) AS legacy_ids
    FROM brand_cloud_user_migrations map JOIN brand_cloud_users bcu ON bcu.id=map.brand_cloud_user_id
    GROUP BY map.user_id
    HAVING bool_and(map.result='created_user') AND count(DISTINCT bcu.password_hash)=1
       AND bool_and(bcu.email_verified AND NOT bcu.signup_pending_verification)
       AND NOT bool_and(bcu.password_hash ~ '^\$2[aby]\$(10|11|12)\$[./A-Za-z0-9]{53}$')
)
SELECT u.id AS user_id, legacy.legacy_ids,
       (EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id=u.id)
        OR EXISTS (SELECT 1 FROM audit_events a WHERE a.created_at>=legacy.migrated_at
                   AND a.event_type LIKE 'oidc%'
                   AND (a.actor_user_id=u.id OR (a.subject_type='user' AND a.subject_id=u.id::text)))) AS adopted
FROM legacy JOIN users u ON u.id=legacy.user_id
WHERE u.password_hash=legacy.password_hash AND u.email_verified AND NOT u.signup_pending_verification;

DO $$
DECLARE adopted_users JSONB;
BEGIN
    SELECT jsonb_agg(user_id ORDER BY user_id) INTO adopted_users FROM identity_correction_candidates WHERE adopted;
    IF adopted_users IS NOT NULL THEN
        RAISE EXCEPTION 'identity correction refused: adopted SSO accounts need local credential remediation'
            USING DETAIL=adopted_users::text;
    END IF;
END $$;

-- Backfill only when upgrading the old non-provenance-aware service. Fresh
-- provenance-aware cutovers may already record holds and exclude admin
-- disables; successful 051 records the boundary for idempotent replay.
INSERT INTO organization_member_activation_holds(organization_id,user_id,disabled_at,updated_at,source)
SELECT DISTINCT m.organization_id,m.user_id,m.disabled_at,m.updated_at,'identity_migration'
FROM organization_members m JOIN users u ON u.id=m.user_id
JOIN brand_cloud_user_migrations map ON map.user_id=u.id AND map.result='created_user'
JOIN brand_cloud_memberships bcm ON bcm.brand_cloud_id=m.organization_id AND bcm.brand_cloud_user_id=map.brand_cloud_user_id
JOIN brand_cloud_users bcu ON bcu.id=bcm.brand_cloud_user_id
WHERE (SELECT backfill_required FROM identity_correction_context)
  AND NOT u.email_verified AND u.disabled_at IS NULL AND bcu.disabled_at IS NULL
  AND m.disabled_at=map.migrated_at AND m.updated_at=map.migrated_at
ON CONFLICT DO NOTHING;

INSERT INTO organization_member_activation_holds(organization_id,user_id,disabled_at,updated_at,source)
SELECT DISTINCT m.organization_id,m.user_id,m.disabled_at,m.updated_at,'provisioning'
FROM organization_members m JOIN users u ON u.id=m.user_id
JOIN audit_events a ON a.organization_id=m.organization_id AND a.subject_type='user' AND a.subject_id=u.id::text
WHERE (SELECT backfill_required FROM identity_correction_context)
  AND NOT u.email_verified AND u.disabled_at IS NULL
  AND a.event_type IN ('brand_cloud_account_created','brand_cloud_account_assigned')
  AND a.payload->>'activation_mode'='email'
  AND m.created_at=m.disabled_at AND m.disabled_at=m.updated_at AND m.updated_at=a.created_at
  AND EXISTS (SELECT 1 FROM auth_tokens t WHERE t.subject_type='user' AND t.subject_id=u.id AND t.user_id=u.id
              AND t.purpose='email_verification' AND t.scope='' AND t.consumed_at IS NULL AND t.created_at=a.created_at)
ON CONFLICT DO NOTHING;

-- An operator may reconcile ambiguous pre-upgrade state from backup/audit
-- evidence. Its explicit audit decision is bound to the exact current row;
-- any subsequent edit makes that decision stale. No credentials are recorded.
CREATE TEMP TABLE identity_hold_resolutions ON COMMIT DROP AS
SELECT m.organization_id,m.user_id,m.disabled_at,m.updated_at,a.payload->>'decision' AS decision,
       a.payload->>'source' AS source
FROM organization_members m JOIN audit_events a ON a.organization_id=m.organization_id
 AND a.subject_type='user' AND a.subject_id=m.user_id::text
WHERE a.event_type='identity_activation_hold_resolved' AND a.actor_user_id IS NOT NULL
  AND a.created_at>=m.updated_at AND a.payload->>'disabled_at'=m.disabled_at::text
  AND a.payload->>'updated_at'=m.updated_at::text
  AND a.payload->>'decision' IN ('keep_disabled','restore_after_verification')
  AND a.payload->>'source' IN ('identity_migration','provisioning')
  AND btrim(COALESCE(a.payload->>'evidence',''))<>''
  AND NOT EXISTS (SELECT 1 FROM audit_events newer WHERE newer.event_type=a.event_type
                  AND newer.organization_id=a.organization_id AND newer.subject_id=a.subject_id
                  AND newer.created_at>a.created_at);

DO $$
DECLARE conflicts JSONB;
BEGIN
    SELECT jsonb_agg(jsonb_build_object('organization_id',organization_id,'user_id',user_id)) INTO conflicts
    FROM (SELECT organization_id,user_id FROM identity_hold_resolutions
          GROUP BY organization_id,user_id HAVING count(DISTINCT (decision,source))>1) ambiguous;
    IF conflicts IS NOT NULL THEN
        RAISE EXCEPTION 'identity correction refused: conflicting activation hold resolutions'
            USING DETAIL=conflicts::text;
    END IF;
END $$;

DELETE FROM organization_member_activation_holds h USING identity_hold_resolutions r
WHERE (SELECT backfill_required FROM identity_correction_context)
  AND h.organization_id=r.organization_id AND h.user_id=r.user_id AND r.decision='keep_disabled';

INSERT INTO organization_member_activation_holds(organization_id,user_id,disabled_at,updated_at,source)
SELECT DISTINCT r.organization_id,r.user_id,r.disabled_at,r.updated_at,r.source
FROM identity_hold_resolutions r JOIN users u ON u.id=r.user_id
WHERE (SELECT backfill_required FROM identity_correction_context)
  AND r.decision='restore_after_verification' AND NOT u.email_verified AND u.disabled_at IS NULL
ON CONFLICT DO NOTHING;

DO $$
DECLARE unresolved JSONB;
BEGIN
    IF (SELECT backfill_required FROM identity_correction_context) THEN
        SELECT jsonb_agg(jsonb_build_object('organization_id',m.organization_id,'user_id',m.user_id)
                         ORDER BY m.organization_id,m.user_id) INTO unresolved
        FROM organization_members m JOIN users u ON u.id=m.user_id
        WHERE m.disabled_at IS NOT NULL AND NOT u.email_verified AND u.disabled_at IS NULL
          AND NOT EXISTS (SELECT 1 FROM organization_member_activation_holds h
                          WHERE h.organization_id=m.organization_id AND h.user_id=m.user_id
                            AND h.disabled_at=m.disabled_at AND h.updated_at=m.updated_at)
          AND NOT EXISTS (SELECT 1 FROM identity_hold_resolutions r WHERE r.organization_id=m.organization_id
                          AND r.user_id=m.user_id AND r.decision='keep_disabled')
          AND (EXISTS (SELECT 1 FROM brand_cloud_user_migrations map
                       JOIN brand_cloud_memberships bcm ON bcm.brand_cloud_user_id=map.brand_cloud_user_id
                       JOIN brand_cloud_users bcu ON bcu.id=map.brand_cloud_user_id
                       WHERE map.user_id=u.id AND bcm.brand_cloud_id=m.organization_id AND bcu.disabled_at IS NULL)
               OR EXISTS (SELECT 1 FROM audit_events a WHERE a.organization_id=m.organization_id
                          AND a.subject_type='user' AND a.subject_id=u.id::text
                          AND a.event_type IN ('brand_cloud_account_created','brand_cloud_account_assigned')
                          AND a.payload->>'activation_mode'='email'));
        IF unresolved IS NOT NULL THEN
            RAISE EXCEPTION 'identity correction refused: unresolved activation suspension provenance'
                USING DETAIL=unresolved::text;
        END IF;
    END IF;
END $$;

UPDATE users u SET password_hash='!password-reset-required!'||gen_random_uuid()::text,
    email_verified=false,email_verified_at=NULL,signup_pending_verification=true,updated_at=now()
FROM identity_correction_candidates c WHERE u.id=c.user_id;

CREATE TEMP TABLE identity_correction_suspensions ON COMMIT DROP AS
SELECT m.organization_id,m.user_id FROM organization_members m
JOIN identity_correction_candidates c ON c.user_id=m.user_id WHERE m.disabled_at IS NULL;

UPDATE organization_members m SET disabled_at=now(),updated_at=now()
FROM identity_correction_suspensions s WHERE m.organization_id=s.organization_id AND m.user_id=s.user_id;

INSERT INTO organization_member_activation_holds(organization_id,user_id,disabled_at,updated_at,source)
SELECT m.organization_id,m.user_id,m.disabled_at,m.updated_at,'identity_migration'
FROM organization_members m JOIN identity_correction_suspensions s USING(organization_id,user_id)
ON CONFLICT(organization_id,user_id) DO UPDATE SET disabled_at=EXCLUDED.disabled_at,updated_at=EXCLUDED.updated_at,source=EXCLUDED.source;

UPDATE refresh_tokens rt SET revoked_at=COALESCE(rt.revoked_at,now())
FROM identity_correction_candidates c WHERE rt.user_id=c.user_id AND rt.revoked_at IS NULL;

UPDATE app_certificates ac SET revoked_at=COALESCE(ac.revoked_at,now()),updated_at=now()
FROM identity_correction_candidates c WHERE ac.subject_type='user' AND ac.subject_id=c.user_id::text AND ac.revoked_at IS NULL;

UPDATE brand_cloud_user_migrations map SET conflict_status='password_reset_required'
FROM identity_correction_candidates c WHERE map.user_id=c.user_id;

INSERT INTO audit_events(event_type,subject_type,subject_id,payload)
SELECT 'identity_credential_corrected','user',user_id::text,
       jsonb_build_object('reason','unsupported_inherited_credential','legacy_user_ids',legacy_ids)
FROM identity_correction_candidates;

DO $$
DECLARE missing_owners JSONB;
BEGIN
    SELECT jsonb_agg(o.id ORDER BY o.id) INTO missing_owners
    FROM organizations o WHERE o.organization_kind='brand_cloud'
      AND to_jsonb(o)->>'deleted_at' IS NULL
      AND (SELECT count(*) FROM organization_members m
           WHERE m.organization_id=o.id AND m.role='owner') <> 1;
    IF missing_owners IS NOT NULL THEN
        RAISE EXCEPTION 'identity correction refused: postflight requires exactly one designated owner'
            USING DETAIL=missing_owners::text;
    END IF;
END $$;

INSERT INTO audit_events(event_type,subject_type,subject_id,payload)
SELECT 'identity_activation_holds_initialized','migration','051_identity_activation_correction.sql','{"version":1}'::jsonb
WHERE NOT EXISTS (SELECT 1 FROM audit_events WHERE event_type='identity_activation_holds_initialized'
                  AND subject_type='migration' AND subject_id='051_identity_activation_correction.sql');
