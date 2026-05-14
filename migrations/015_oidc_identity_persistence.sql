CREATE TABLE IF NOT EXISTS identity_providers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    type TEXT NOT NULL,
    issuer_url TEXT NOT NULL,
    client_id TEXT NOT NULL,
    client_secret_ref TEXT,
    scopes TEXT[] NOT NULL DEFAULT ARRAY['openid', 'email', 'profile'],
    enabled BOOLEAN NOT NULL DEFAULT false,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT identity_providers_provider_id_not_blank CHECK (btrim(provider_id) <> ''),
    CONSTRAINT identity_providers_name_not_blank CHECK (btrim(name) <> ''),
    CONSTRAINT identity_providers_type_check CHECK (type IN ('oidc')),
    CONSTRAINT identity_providers_issuer_url_not_blank CHECK (btrim(issuer_url) <> ''),
    CONSTRAINT identity_providers_client_id_not_blank CHECK (btrim(client_id) <> ''),
    CONSTRAINT identity_providers_client_secret_ref_check
        CHECK (client_secret_ref IS NULL OR client_secret_ref ~ '^env:[A-Za-z_][A-Za-z0-9_]*$')
);

CREATE UNIQUE INDEX IF NOT EXISTS identity_providers_enabled_unique_idx
    ON identity_providers (enabled)
    WHERE enabled IS TRUE;

CREATE INDEX IF NOT EXISTS identity_providers_provider_id_idx
    ON identity_providers (provider_id);

DROP TRIGGER IF EXISTS identity_providers_set_updated_at ON identity_providers;
CREATE TRIGGER identity_providers_set_updated_at
    BEFORE UPDATE ON identity_providers
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS user_identities (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider_id UUID NOT NULL REFERENCES identity_providers(id) ON DELETE CASCADE,
    issuer_url TEXT NOT NULL,
    subject TEXT NOT NULL,
    email TEXT NOT NULL,
    email_verified BOOLEAN NOT NULL,
    claims JSONB NOT NULL DEFAULT '{}'::jsonb,
    linked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT user_identities_provider_subject_key UNIQUE (provider_id, subject),
    CONSTRAINT user_identities_user_provider_key UNIQUE (user_id, provider_id),
    CONSTRAINT user_identities_issuer_url_not_blank CHECK (btrim(issuer_url) <> ''),
    CONSTRAINT user_identities_subject_not_blank CHECK (btrim(subject) <> ''),
    CONSTRAINT user_identities_email_normalized CHECK (email = lower(btrim(email)) AND email <> ''),
    CONSTRAINT user_identities_email_verified_check CHECK (email_verified IS TRUE)
);

CREATE INDEX IF NOT EXISTS user_identities_user_idx
    ON user_identities (user_id);

DROP TRIGGER IF EXISTS user_identities_set_updated_at ON user_identities;
CREATE TRIGGER user_identities_set_updated_at
    BEFORE UPDATE ON user_identities
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS oidc_login_states (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id UUID NOT NULL REFERENCES identity_providers(id) ON DELETE CASCADE,
    state_hash TEXT NOT NULL UNIQUE,
    nonce_hash TEXT NOT NULL,
    redirect_url TEXT NOT NULL,
    post_login_redirect_url TEXT,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT oidc_login_states_state_hash_not_blank CHECK (btrim(state_hash) <> ''),
    CONSTRAINT oidc_login_states_nonce_hash_not_blank CHECK (btrim(nonce_hash) <> ''),
    CONSTRAINT oidc_login_states_redirect_url_not_blank CHECK (btrim(redirect_url) <> '')
);

CREATE INDEX IF NOT EXISTS oidc_login_states_provider_created_idx
    ON oidc_login_states (provider_id, created_at DESC);

CREATE INDEX IF NOT EXISTS oidc_login_states_active_idx
    ON oidc_login_states (expires_at)
    WHERE consumed_at IS NULL;
