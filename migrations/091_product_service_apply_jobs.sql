CREATE TABLE product_service_apply_jobs (
    id text PRIMARY KEY CHECK (id ~ '^job-[0-9a-f]{24}$'),
    organization_id uuid NOT NULL REFERENCES organizations(id),
    product_id uuid NOT NULL REFERENCES device_item_profiles(id),
    actor_user_id uuid NOT NULL REFERENCES users(id),
    target_revision bigint NOT NULL,
    target_digest text NOT NULL CHECK (target_digest ~ '^[0-9a-f]{64}$'),
    target_options jsonb NOT NULL,
    target_log_retention_days integer CHECK (target_log_retention_days IN (7,30,90)),
    preview_token text NOT NULL CHECK (preview_token ~ '^[0-9a-f]{64}$'),
    total_devices integer NOT NULL CHECK (total_devices >= 0),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','completed','canceled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    FOREIGN KEY (product_id,target_revision) REFERENCES product_service_grants(product_id,revision)
);
CREATE UNIQUE INDEX product_service_apply_one_active
    ON product_service_apply_jobs(product_id) WHERE status='active';

CREATE TABLE product_service_apply_items (
    job_id text NOT NULL REFERENCES product_service_apply_jobs(id),
    device_id uuid NOT NULL,
    operation_id text NOT NULL UNIQUE,
    baseline_revision bigint NOT NULL CHECK (baseline_revision >= 0),
    baseline_product_revision bigint NOT NULL CHECK (baseline_product_revision > 0),
    baseline_digest text NOT NULL CHECK (baseline_digest ~ '^[0-9a-f]{64}$'),
    baseline_options jsonb NOT NULL,
    baseline_state text NOT NULL CHECK (baseline_state IN ('active','suspended','revoked')),
    video_cloud_devid text NOT NULL,
    dispatched_at timestamptz,
    PRIMARY KEY (job_id,device_id)
);
CREATE INDEX product_service_apply_items_page ON product_service_apply_items(job_id,device_id);
