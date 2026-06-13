ALTER TABLE auth_tokens
    ALTER COLUMN user_id DROP NOT NULL;

ALTER TABLE auth_tokens
    ADD COLUMN IF NOT EXISTS subject_type TEXT NOT NULL DEFAULT 'platform_user',
    ADD COLUMN IF NOT EXISTS subject_id UUID;

UPDATE auth_tokens
SET subject_type = 'platform_user',
    subject_id = user_id
WHERE subject_id IS NULL;

ALTER TABLE auth_tokens
    ALTER COLUMN subject_id SET NOT NULL;

ALTER TABLE auth_tokens
    DROP CONSTRAINT IF EXISTS auth_tokens_subject_type_check;

ALTER TABLE auth_tokens
    ADD CONSTRAINT auth_tokens_subject_type_check
    CHECK (subject_type IN ('platform_user', 'brand_cloud_user'));

CREATE INDEX IF NOT EXISTS auth_tokens_subject_purpose_idx
    ON auth_tokens (subject_type, subject_id, purpose, scope, created_at DESC);
