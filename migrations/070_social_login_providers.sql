DROP INDEX IF EXISTS identity_providers_enabled_unique_idx;

ALTER TABLE identity_providers
    DROP CONSTRAINT IF EXISTS identity_providers_type_check;

ALTER TABLE identity_providers
    ADD CONSTRAINT identity_providers_type_check
    CHECK (type IN ('oidc', 'oauth2'));
