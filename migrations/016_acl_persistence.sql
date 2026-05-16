CREATE TABLE IF NOT EXISTS permissions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE,
    domain TEXT NOT NULL,
    action TEXT NOT NULL,
    description TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT permissions_name_not_blank CHECK (btrim(name) <> ''),
    CONSTRAINT permissions_domain_not_blank CHECK (btrim(domain) <> ''),
    CONSTRAINT permissions_action_not_blank CHECK (btrim(action) <> ''),
    CONSTRAINT permissions_name_matches_parts CHECK (name = domain || '.' || action)
);

CREATE TABLE IF NOT EXISTS roles (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE,
    scope_type TEXT NOT NULL,
    description TEXT,
    system_role BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at TIMESTAMPTZ,
    CONSTRAINT roles_name_not_blank CHECK (btrim(name) <> ''),
    CONSTRAINT roles_scope_type_check CHECK (scope_type IN ('platform', 'organization'))
);

CREATE TABLE IF NOT EXISTS role_permissions (
    role_id UUID NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    permission_id UUID NOT NULL REFERENCES permissions(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (role_id, permission_id)
);

CREATE TABLE IF NOT EXISTS role_assignments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    role_id UUID NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    actor_type TEXT NOT NULL,
    actor_id TEXT NOT NULL,
    scope_type TEXT NOT NULL,
    scope_id TEXT,
    organization_id UUID REFERENCES organizations(id) ON DELETE CASCADE,
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at TIMESTAMPTZ,
    CONSTRAINT role_assignments_actor_type_check CHECK (actor_type IN ('user', 'service_account')),
    CONSTRAINT role_assignments_actor_id_not_blank CHECK (btrim(actor_id) <> ''),
    CONSTRAINT role_assignments_scope_type_check CHECK (scope_type IN ('platform', 'organization')),
    CONSTRAINT role_assignments_scope_consistency CHECK (
        (scope_type = 'platform' AND scope_id IS NULL AND organization_id IS NULL)
        OR
        (scope_type = 'organization' AND scope_id IS NOT NULL AND organization_id IS NOT NULL AND scope_id = organization_id::text)
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS role_assignments_active_unique_idx
    ON role_assignments (role_id, actor_type, actor_id, scope_type, COALESCE(scope_id, ''))
    WHERE disabled_at IS NULL;

CREATE INDEX IF NOT EXISTS role_assignments_actor_scope_idx
    ON role_assignments (actor_type, actor_id, scope_type, scope_id)
    WHERE disabled_at IS NULL;

CREATE INDEX IF NOT EXISTS role_assignments_org_idx
    ON role_assignments (organization_id, created_at DESC)
    WHERE disabled_at IS NULL;

CREATE TABLE IF NOT EXISTS external_group_mappings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id TEXT NOT NULL,
    external_group TEXT NOT NULL,
    role_id UUID NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    scope_type TEXT NOT NULL,
    scope_id TEXT,
    organization_id UUID REFERENCES organizations(id) ON DELETE CASCADE,
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at TIMESTAMPTZ,
    CONSTRAINT external_group_mappings_provider_not_blank CHECK (btrim(provider_id) <> ''),
    CONSTRAINT external_group_mappings_group_not_blank CHECK (btrim(external_group) <> ''),
    CONSTRAINT external_group_mappings_scope_type_check CHECK (scope_type IN ('platform', 'organization')),
    CONSTRAINT external_group_mappings_scope_consistency CHECK (
        (scope_type = 'platform' AND scope_id IS NULL AND organization_id IS NULL)
        OR
        (scope_type = 'organization' AND scope_id IS NOT NULL AND organization_id IS NOT NULL AND scope_id = organization_id::text)
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS external_group_mappings_active_unique_idx
    ON external_group_mappings (provider_id, external_group, role_id, scope_type, COALESCE(scope_id, ''))
    WHERE disabled_at IS NULL;

CREATE INDEX IF NOT EXISTS external_group_mappings_provider_group_idx
    ON external_group_mappings (provider_id, external_group)
    WHERE disabled_at IS NULL;

CREATE TABLE IF NOT EXISTS acl_audit_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type TEXT NOT NULL,
    actor_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    organization_id UUID REFERENCES organizations(id) ON DELETE SET NULL,
    subject_type TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT acl_audit_events_event_type_not_blank CHECK (btrim(event_type) <> ''),
    CONSTRAINT acl_audit_events_subject_type_not_blank CHECK (btrim(subject_type) <> ''),
    CONSTRAINT acl_audit_events_subject_id_not_blank CHECK (btrim(subject_id) <> '')
);

CREATE INDEX IF NOT EXISTS acl_audit_events_event_type_idx
    ON acl_audit_events (event_type, created_at DESC);

CREATE INDEX IF NOT EXISTS acl_audit_events_subject_idx
    ON acl_audit_events (subject_type, subject_id, created_at DESC);

CREATE INDEX IF NOT EXISTS acl_audit_events_org_idx
    ON acl_audit_events (organization_id, created_at DESC);

DROP TRIGGER IF EXISTS permissions_set_updated_at ON permissions;
CREATE TRIGGER permissions_set_updated_at
    BEFORE UPDATE ON permissions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS roles_set_updated_at ON roles;
CREATE TRIGGER roles_set_updated_at
    BEFORE UPDATE ON roles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS role_assignments_set_updated_at ON role_assignments;
CREATE TRIGGER role_assignments_set_updated_at
    BEFORE UPDATE ON role_assignments
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS external_group_mappings_set_updated_at ON external_group_mappings;
CREATE TRIGGER external_group_mappings_set_updated_at
    BEFORE UPDATE ON external_group_mappings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS acl_audit_events_set_updated_at ON acl_audit_events;
CREATE TRIGGER acl_audit_events_set_updated_at
    BEFORE UPDATE ON acl_audit_events
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
