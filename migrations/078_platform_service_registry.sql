CREATE TABLE platform_service_workloads (
    environment text NOT NULL,
    certificate_subject text NOT NULL,
    issuer_fingerprint_sha256 text NOT NULL CHECK (issuer_fingerprint_sha256 ~ '^[0-9a-f]{64}$'),
    service_id text NOT NULL,
    instance_id text NOT NULL,
    allowed_option_codes text[] NOT NULL,
    approved_by uuid NOT NULL REFERENCES users(id),
    approved_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    PRIMARY KEY (environment, certificate_subject, issuer_fingerprint_sha256),
    UNIQUE (environment, service_id, instance_id, certificate_subject, issuer_fingerprint_sha256)
);

CREATE TABLE platform_service_manifests (
    environment text NOT NULL,
    service_id text NOT NULL,
    manifest_version text NOT NULL,
    digest_sha256 text NOT NULL CHECK (digest_sha256 ~ '^[0-9a-f]{64}$'),
    protocol_version text NOT NULL,
    endpoint_ref text NOT NULL,
    options jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (environment, service_id, manifest_version)
);

CREATE TABLE platform_services (
    environment text NOT NULL,
    service_id text NOT NULL,
    published_version text NOT NULL,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'retired')),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (environment, service_id),
    FOREIGN KEY (environment, service_id, published_version)
        REFERENCES platform_service_manifests(environment, service_id, manifest_version)
);

CREATE TABLE platform_service_instances (
    environment text NOT NULL,
    service_id text NOT NULL,
    instance_id text NOT NULL,
    manifest_version text NOT NULL,
    certificate_subject text NOT NULL,
    issuer_fingerprint_sha256 text NOT NULL,
    ready boolean NOT NULL,
    lease_expires_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (environment, service_id, instance_id),
    FOREIGN KEY (environment, service_id, manifest_version)
        REFERENCES platform_service_manifests(environment, service_id, manifest_version),
    FOREIGN KEY (environment, service_id, instance_id, certificate_subject, issuer_fingerprint_sha256)
        REFERENCES platform_service_workloads(environment, service_id, instance_id, certificate_subject, issuer_fingerprint_sha256)
);

CREATE TABLE platform_service_catalog_revisions (
    environment text PRIMARY KEY,
    revision bigint NOT NULL CHECK (revision > 0)
);

CREATE TABLE platform_service_registration_requests (
    environment text NOT NULL,
    certificate_subject text NOT NULL,
    issuer_fingerprint_sha256 text NOT NULL,
    request_id text NOT NULL,
    request_sha256 text NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (environment, certificate_subject, issuer_fingerprint_sha256, request_id)
);

CREATE INDEX platform_service_instances_current_ready
    ON platform_service_instances(environment, service_id, manifest_version, lease_expires_at)
    WHERE ready;
