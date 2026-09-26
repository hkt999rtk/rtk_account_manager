ALTER TABLE job_authorizations DROP CONSTRAINT IF EXISTS job_authorizations_capability_check;
ALTER TABLE job_authorizations ADD CONSTRAINT job_authorizations_capability_check
    CHECK (capability IN ('provisioning.create','product_services.apply'));
