-- A transaction can begin before a billing cutoff and insert a Product grant
-- after the period seal has drained other writers. Record insertion time, not
-- transaction start, so that later insert belongs to the later period.
ALTER TABLE product_service_grants
    ALTER COLUMN created_at SET DEFAULT clock_timestamp();
