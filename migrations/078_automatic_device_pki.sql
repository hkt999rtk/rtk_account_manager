-- PKI work commits atomically with every Cloud/Product creation path, including
-- signup, social signup, platform creation and fixture/bootstrap helpers.
ALTER TABLE organizations ADD COLUMN pki_status TEXT NOT NULL DEFAULT 'pending'
 CHECK (pki_status IN ('pending','ready','failed','cancelled'));
ALTER TABLE organizations ADD COLUMN pki_operation_id UUID NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE organizations ADD COLUMN pki_issuer_id UUID;
ALTER TABLE device_item_profiles ADD COLUMN pki_status TEXT NOT NULL DEFAULT 'pending'
 CHECK (pki_status IN ('pending','ready','failed','cancelled'));
ALTER TABLE device_item_profiles ADD COLUMN pki_operation_id UUID NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE device_item_profiles ADD COLUMN pki_issuer_id UUID;

CREATE TABLE device_pki_outbox (
 operation_id UUID PRIMARY KEY,
 cloud_id UUID NOT NULL REFERENCES organizations(id),
 product_id TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','ready','failed','cancelled')),
 issuer_id UUID,
 controller_operation_id UUID,
 attempts INTEGER NOT NULL DEFAULT 0,
 available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 lease_id UUID,
 lease_until TIMESTAMPTZ,
 error_code TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 UNIQUE(cloud_id,product_id)
);
CREATE INDEX device_pki_outbox_pending ON device_pki_outbox(available_at) WHERE status='pending';

CREATE FUNCTION queue_device_pki() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_TABLE_NAME='organizations' THEN
  IF NEW.organization_kind='brand_cloud' THEN
   INSERT INTO device_pki_outbox(operation_id,cloud_id) VALUES(NEW.pki_operation_id,NEW.id);
  END IF;
 ELSE
  INSERT INTO device_pki_outbox(operation_id,cloud_id,product_id)
   VALUES(NEW.pki_operation_id,NEW.brand_cloud_id,NEW.id::text);
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER organization_device_pki AFTER INSERT ON organizations FOR EACH ROW EXECUTE FUNCTION queue_device_pki();
CREATE TRIGGER product_device_pki AFTER INSERT ON device_item_profiles FOR EACH ROW EXECUTE FUNCTION queue_device_pki();

-- Metadata backfill only; no remote key mutation, automatic legacy adoption or
-- deletion of existing CA history is performed by this migration.
INSERT INTO device_pki_outbox(operation_id,cloud_id)
 SELECT pki_operation_id,id FROM organizations WHERE organization_kind='brand_cloud';
INSERT INTO device_pki_outbox(operation_id,cloud_id,product_id)
 SELECT pki_operation_id,brand_cloud_id,id::text FROM device_item_profiles;
