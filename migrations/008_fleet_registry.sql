CREATE TABLE IF NOT EXISTS device_groups (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    description TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (organization_id, id),
    UNIQUE (organization_id, name)
);

CREATE TABLE IF NOT EXISTS device_group_members (
    organization_id UUID NOT NULL,
    group_id UUID NOT NULL,
    device_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, device_id),
    FOREIGN KEY (organization_id, group_id)
        REFERENCES device_groups (organization_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (organization_id, device_id)
        REFERENCES devices (organization_id, id)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS device_group_members_org_device_idx
    ON device_group_members (organization_id, device_id);

CREATE TABLE IF NOT EXISTS device_tags (
    organization_id UUID NOT NULL,
    device_id UUID NOT NULL,
    tag TEXT NOT NULL CHECK (btrim(tag) <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, device_id, tag),
    FOREIGN KEY (organization_id, device_id)
        REFERENCES devices (organization_id, id)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS device_tags_org_tag_idx
    ON device_tags (organization_id, tag);

DROP TRIGGER IF EXISTS device_groups_set_updated_at ON device_groups;
CREATE TRIGGER device_groups_set_updated_at
    BEFORE UPDATE ON device_groups
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS device_tags_set_updated_at ON device_tags;
CREATE TRIGGER device_tags_set_updated_at
    BEFORE UPDATE ON device_tags
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();
