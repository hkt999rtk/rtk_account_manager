-- Stable internal App identity for a Console login; never adopt by email.
CREATE TABLE test_lab_console_users (
 user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
 end_user_id UUID NOT NULL UNIQUE REFERENCES end_users(id),
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Retire password-based test delegations, without changing their device bindings.
UPDATE test_lab_accounts SET revoked_at=now() WHERE revoked_at IS NULL;
UPDATE test_lab_sessions SET revoked_at=now() WHERE revoked_at IS NULL;
