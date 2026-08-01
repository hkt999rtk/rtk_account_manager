-- Audit events are emitted by both platform users and tenant-scoped Brand Cloud
-- users. Their UUIDs live in different tables, so actor_user_id is intentionally
-- a retained polymorphic identity and cannot reference only users(id).
ALTER TABLE audit_events
    DROP CONSTRAINT IF EXISTS audit_events_actor_user_id_fkey;

COMMENT ON COLUMN audit_events.actor_user_id IS
    'Stable actor UUID for a platform user or tenant-scoped Brand Cloud user; actor kind is carried by the authenticated event context.';
