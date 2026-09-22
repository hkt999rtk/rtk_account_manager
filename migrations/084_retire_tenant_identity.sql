SET LOCAL lock_timeout = '5s';
LOCK TABLE brand_cloud_users, brand_cloud_memberships, brand_cloud_refresh_tokens IN ACCESS EXCLUSIVE MODE;
-- Never silently discard an identity introduced after the global-user cutover.
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM brand_cloud_users b LEFT JOIN brand_cloud_user_migrations m ON m.brand_cloud_user_id=b.id LEFT JOIN users u ON u.id=m.user_id WHERE u.id IS NULL) THEN
  RAISE EXCEPTION 'unmapped legacy identity; reconcile global users before retirement';
 END IF;
 IF EXISTS(SELECT 1 FROM brand_cloud_memberships b JOIN brand_cloud_user_migrations m ON m.brand_cloud_user_id=b.brand_cloud_user_id
 LEFT JOIN organization_members n ON n.organization_id=b.brand_cloud_id AND n.user_id=m.user_id WHERE n.user_id IS NULL) THEN
  RAISE EXCEPTION 'unmapped legacy membership; reconcile before retirement';
 END IF;
 IF EXISTS(SELECT 1 FROM brand_cloud_refresh_tokens WHERE revoked_at IS NULL) THEN
  RAISE EXCEPTION 'unrevoked tenant token; reconcile before retirement';
 END IF;
END $$;
DELETE FROM auth_tokens WHERE subject_type='brand_cloud_user';
-- Retired activation links cannot be delivered or retried after cutover.
UPDATE email_outbox SET status='expired', payload_nonce=NULL, payload_ciphertext=NULL,
 lease_until=NULL, last_error='tenant identity activation retired'
WHERE message_type='brand_cloud_user_activation' AND status NOT IN ('sent','expired');
DROP TABLE brand_cloud_refresh_tokens;
DROP TABLE brand_cloud_memberships;
DROP FUNCTION sync_brand_cloud_membership_role_assignment();
DROP TABLE brand_cloud_users;
