CREATE TABLE IF NOT EXISTS device_item_profiles (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    brand_cloud_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    profile_key TEXT NOT NULL,
    display_name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    category TEXT NOT NULL,
    manufacturer TEXT,
    model TEXT,
    metadata_defaults JSONB NOT NULL DEFAULT '{}'::jsonb,
    metadata_schema JSONB NOT NULL DEFAULT '{}'::jsonb,
    ca_profile TEXT NOT NULL,
    issuer_profile TEXT NOT NULL,
    service_options JSONB NOT NULL DEFAULT '[]'::jsonb,
    claim_policy JSONB NOT NULL DEFAULT '{}'::jsonb,
    provisioning_policy JSONB NOT NULL DEFAULT '{}'::jsonb,
    disabled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT device_item_profiles_key_not_blank CHECK (btrim(profile_key) <> ''),
    CONSTRAINT device_item_profiles_display_name_not_blank CHECK (btrim(display_name) <> ''),
    CONSTRAINT device_item_profiles_status_check CHECK (status IN ('active', 'disabled')),
    CONSTRAINT device_item_profiles_category_check CHECK (category IN ('ip_camera', 'mqtt_device', 'generic')),
    CONSTRAINT device_item_profiles_ca_profile_not_blank CHECK (btrim(ca_profile) <> ''),
    CONSTRAINT device_item_profiles_issuer_profile_not_blank CHECK (btrim(issuer_profile) <> ''),
    CONSTRAINT device_item_profiles_service_options_array CHECK (jsonb_typeof(service_options) = 'array'),
    CONSTRAINT device_item_profiles_metadata_defaults_object CHECK (jsonb_typeof(metadata_defaults) = 'object'),
    CONSTRAINT device_item_profiles_metadata_schema_object CHECK (jsonb_typeof(metadata_schema) = 'object'),
    CONSTRAINT device_item_profiles_claim_policy_object CHECK (jsonb_typeof(claim_policy) = 'object'),
    CONSTRAINT device_item_profiles_provisioning_policy_object CHECK (jsonb_typeof(provisioning_policy) = 'object'),
    CONSTRAINT device_item_profiles_brand_key_unique UNIQUE (brand_cloud_id, profile_key)
);

CREATE INDEX IF NOT EXISTS device_item_profiles_brand_status_idx
    ON device_item_profiles (brand_cloud_id, status, created_at DESC);

ALTER TABLE device_claim_tokens
    ADD COLUMN IF NOT EXISTS device_item_profile_id UUID REFERENCES device_item_profiles(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS device_claim_tokens_profile_idx
    ON device_claim_tokens (device_item_profile_id)
    WHERE device_item_profile_id IS NOT NULL;
