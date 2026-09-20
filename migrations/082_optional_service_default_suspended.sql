-- Registration explicitly activates only MQTT. Keep direct SQL inserts fail-closed too.
-- Existing service rows retain their reviewed status; this changes future inserts only.
ALTER TABLE platform_services ALTER COLUMN status SET DEFAULT 'suspended';
