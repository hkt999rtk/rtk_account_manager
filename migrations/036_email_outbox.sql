CREATE TABLE IF NOT EXISTS email_outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key TEXT NOT NULL UNIQUE CHECK (btrim(idempotency_key) <> ''),
    message_type TEXT NOT NULL CHECK (message_type IN (
        'email_verification',
        'login_activation',
        'password_reset',
        'brand_cloud_owner_transfer',
        'quota_approved',
        'quota_declined'
    )),
    template_version INTEGER NOT NULL DEFAULT 1 CHECK (template_version > 0),
    payload_nonce BYTEA,
    payload_ciphertext BYTEA,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN (
        'pending',
        'sending',
        'retrying',
        'sent',
        'dead_lettered',
        'expired'
    )),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_until TIMESTAMPTZ,
    last_error TEXT,
    expires_at TIMESTAMPTZ,
    sent_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (status IN ('sent', 'expired') AND payload_nonce IS NULL AND payload_ciphertext IS NULL)
        OR
        (status NOT IN ('sent', 'expired') AND payload_nonce IS NOT NULL AND payload_ciphertext IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS email_outbox_ready_idx
    ON email_outbox (available_at, created_at)
    WHERE status IN ('pending', 'retrying', 'sending');

CREATE INDEX IF NOT EXISTS email_outbox_terminal_idx
    ON email_outbox (status, updated_at DESC)
    WHERE status IN ('dead_lettered', 'expired');

DROP TRIGGER IF EXISTS email_outbox_set_updated_at ON email_outbox;
CREATE TRIGGER email_outbox_set_updated_at
    BEFORE UPDATE ON email_outbox
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();
