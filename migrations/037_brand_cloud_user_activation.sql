ALTER TABLE auth_tokens
    DROP CONSTRAINT IF EXISTS auth_tokens_purpose_check;

ALTER TABLE auth_tokens
    ADD CONSTRAINT auth_tokens_purpose_check
    CHECK (purpose IN (
        'email_verification',
        'password_reset',
        'login_activation',
        'brand_cloud_user_activation'
    ));
