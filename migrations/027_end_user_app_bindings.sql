ALTER TABLE app_certificates
    ADD COLUMN IF NOT EXISTS subject_type TEXT NOT NULL DEFAULT 'platform_user',
    ADD COLUMN IF NOT EXISTS subject_id TEXT;

UPDATE app_certificates
SET subject_id = user_id::text
WHERE subject_id IS NULL;

ALTER TABLE app_certificates
    ALTER COLUMN user_id DROP NOT NULL,
    ALTER COLUMN subject_id SET NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'app_certificates_subject_type_check'
    ) THEN
        ALTER TABLE app_certificates
            ADD CONSTRAINT app_certificates_subject_type_check
            CHECK (subject_type IN ('platform_user', 'brand_cloud_user', 'end_user'));
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS app_certificates_subject_active_unique
    ON app_certificates (subject_type, subject_id)
    WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS app_certificates_subject_validity_idx
    ON app_certificates (subject_type, subject_id, not_after)
    WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS end_users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    primary_email TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    display_name TEXT,
    status TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at TIMESTAMPTZ,
    CONSTRAINT end_users_primary_email_not_blank CHECK (btrim(primary_email) <> ''),
    CONSTRAINT end_users_primary_email_normalized CHECK (primary_email = lower(btrim(primary_email))),
    CONSTRAINT end_users_status_check CHECK (status IN ('active', 'disabled'))
);

CREATE UNIQUE INDEX IF NOT EXISTS end_users_primary_email_key
    ON end_users (primary_email);

CREATE TABLE IF NOT EXISTS end_user_identities (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    end_user_id UUID NOT NULL REFERENCES end_users(id) ON DELETE CASCADE,
    identity_provider TEXT NOT NULL,
    provider_subject TEXT NOT NULL,
    email TEXT,
    phone TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT end_user_identities_provider_not_blank CHECK (btrim(identity_provider) <> ''),
    CONSTRAINT end_user_identities_subject_not_blank CHECK (btrim(provider_subject) <> ''),
    CONSTRAINT end_user_identities_provider_subject_key UNIQUE (identity_provider, provider_subject)
);

CREATE INDEX IF NOT EXISTS end_user_identities_end_user_idx
    ON end_user_identities (end_user_id);

CREATE TABLE IF NOT EXISTS end_user_refresh_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    end_user_id UUID NOT NULL REFERENCES end_users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS end_user_refresh_tokens_user_idx
    ON end_user_refresh_tokens (end_user_id)
    WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS brand_cloud_end_users (
    brand_cloud_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    end_user_id UUID NOT NULL REFERENCES end_users(id) ON DELETE CASCADE,
    display_alias TEXT,
    status TEXT NOT NULL DEFAULT 'active',
    consent JSONB NOT NULL DEFAULT '{}'::jsonb,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT brand_cloud_end_users_status_check CHECK (status IN ('active', 'blocked')),
    CONSTRAINT brand_cloud_end_users_key UNIQUE (brand_cloud_id, end_user_id)
);

CREATE INDEX IF NOT EXISTS brand_cloud_end_users_end_user_idx
    ON brand_cloud_end_users (end_user_id);

CREATE TABLE IF NOT EXISTS device_user_bindings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    brand_cloud_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    end_user_id UUID NOT NULL REFERENCES end_users(id) ON DELETE CASCADE,
    role TEXT NOT NULL DEFAULT 'owner',
    created_from_claim_id UUID REFERENCES device_claims(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at TIMESTAMPTZ,
    CONSTRAINT device_user_bindings_role_check CHECK (role IN ('owner', 'member', 'viewer')),
    CONSTRAINT device_user_bindings_device_end_user_key UNIQUE (device_id, end_user_id)
);

CREATE INDEX IF NOT EXISTS device_user_bindings_end_user_idx
    ON device_user_bindings (end_user_id)
    WHERE disabled_at IS NULL;

CREATE INDEX IF NOT EXISTS device_user_bindings_brand_user_idx
    ON device_user_bindings (brand_cloud_id, end_user_id)
    WHERE disabled_at IS NULL;

DROP TRIGGER IF EXISTS end_users_set_updated_at ON end_users;
CREATE TRIGGER end_users_set_updated_at
    BEFORE UPDATE ON end_users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS end_user_identities_set_updated_at ON end_user_identities;
CREATE TRIGGER end_user_identities_set_updated_at
    BEFORE UPDATE ON end_user_identities
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS brand_cloud_end_users_set_updated_at ON brand_cloud_end_users;
CREATE TRIGGER brand_cloud_end_users_set_updated_at
    BEFORE UPDATE ON brand_cloud_end_users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS device_user_bindings_set_updated_at ON device_user_bindings;
CREATE TRIGGER device_user_bindings_set_updated_at
    BEFORE UPDATE ON device_user_bindings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
