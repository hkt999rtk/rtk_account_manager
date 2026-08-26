CREATE TABLE IF NOT EXISTS brand_cloud_member_invitations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    brand_cloud_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    invited_by_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    target_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    target_email TEXT NOT NULL,
    role TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL DEFAULT 'pending',
    expires_at TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    canceled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT brand_cloud_member_invitations_role_check CHECK (role IN ('admin', 'member')),
    CONSTRAINT brand_cloud_member_invitations_status_check CHECK (status IN ('pending', 'accepted', 'canceled', 'expired')),
    CONSTRAINT brand_cloud_member_invitations_email_normalized CHECK (target_email = lower(btrim(target_email)))
);

CREATE UNIQUE INDEX IF NOT EXISTS brand_cloud_member_invitations_pending_target_idx
    ON brand_cloud_member_invitations (brand_cloud_id, target_user_id)
    WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS brand_cloud_member_invitations_brand_status_idx
    ON brand_cloud_member_invitations (brand_cloud_id, status, created_at DESC);

DROP TRIGGER IF EXISTS brand_cloud_member_invitations_set_updated_at ON brand_cloud_member_invitations;
CREATE TRIGGER brand_cloud_member_invitations_set_updated_at
    BEFORE UPDATE ON brand_cloud_member_invitations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

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
        'brand_cloud_membership_invitation',
        'quota_approved',
        'quota_declined'
    ));
