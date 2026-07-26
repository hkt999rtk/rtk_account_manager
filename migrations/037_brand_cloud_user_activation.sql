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

ALTER TABLE email_outbox
    DROP CONSTRAINT IF EXISTS email_outbox_message_type_check;

ALTER TABLE email_outbox
    ADD CONSTRAINT email_outbox_message_type_check
    CHECK (message_type IN (
        'email_verification',
        'login_activation',
        'password_reset',
        'brand_cloud_owner_transfer',
        'brand_cloud_user_activation',
        'quota_approved',
        'quota_declined'
    ));
