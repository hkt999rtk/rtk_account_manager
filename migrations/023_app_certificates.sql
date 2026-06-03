CREATE TABLE IF NOT EXISTS app_certificates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    subject TEXT NOT NULL,
    csr_sha256 TEXT NOT NULL,
    certificate_pem TEXT NOT NULL,
    certificate_chain_pem TEXT NOT NULL,
    fingerprint_sha256 TEXT NOT NULL UNIQUE,
    serial_number TEXT NOT NULL,
    issuer_request_id TEXT NOT NULL UNIQUE,
    not_before TIMESTAMPTZ NOT NULL,
    not_after TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS app_certificates_user_active_unique
    ON app_certificates (user_id)
    WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS app_certificates_user_validity_idx
    ON app_certificates (user_id, not_after)
    WHERE revoked_at IS NULL;
