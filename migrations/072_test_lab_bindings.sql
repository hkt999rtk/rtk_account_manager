-- Test-only delegated account access; no passwords, device keys or bearer tokens.
CREATE TABLE test_lab_accounts (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 brand_cloud_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
 end_user_id UUID NOT NULL REFERENCES end_users(id) ON DELETE CASCADE,
 expires_at TIMESTAMPTZ NOT NULL DEFAULT now()+interval '30 minutes',
 revoked_at TIMESTAMPTZ
);
CREATE INDEX test_lab_accounts_actor ON test_lab_accounts(user_id,brand_cloud_id);
CREATE TABLE test_lab_login_attempts(user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE, attempted_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE INDEX test_lab_login_attempts_actor ON test_lab_login_attempts(user_id,attempted_at);
CREATE TABLE test_lab_bind_grants (
 token_hash TEXT PRIMARY KEY,
 account_id UUID NOT NULL REFERENCES test_lab_accounts(id) ON DELETE CASCADE,
 device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
 expires_at TIMESTAMPTZ NOT NULL DEFAULT now()+interval '2 minutes',
 consumed_at TIMESTAMPTZ
);
ALTER TABLE test_lab_sessions ADD COLUMN account_id UUID REFERENCES test_lab_accounts(id) ON DELETE CASCADE;
ALTER TABLE test_lab_sessions DROP CONSTRAINT test_lab_sessions_device_id_fkey;
ALTER TABLE test_lab_sessions ADD CONSTRAINT test_lab_sessions_device_id_fkey FOREIGN KEY(device_id) REFERENCES devices(id) ON DELETE CASCADE;
UPDATE test_lab_sessions SET revoked_at=now() WHERE revoked_at IS NULL;
