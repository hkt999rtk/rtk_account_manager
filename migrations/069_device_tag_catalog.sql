CREATE TABLE IF NOT EXISTS device_tag_catalog (
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    tag TEXT NOT NULL CHECK (btrim(tag) <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, tag)
);

INSERT INTO device_tag_catalog (organization_id, tag)
SELECT DISTINCT organization_id, tag FROM device_tags
ON CONFLICT DO NOTHING;

CREATE INDEX IF NOT EXISTS device_tag_catalog_org_tag_idx ON device_tag_catalog (organization_id, tag);
