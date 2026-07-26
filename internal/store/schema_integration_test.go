package store

import (
	"context"
	"testing"
)

func TestIntegrationDatabaseSchemaInvariants(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	requiredTables := []string{
		"auth_tokens",
		"audit_events",
		"device_claim_tokens",
		"device_claims",
		"device_item_profiles",
		"device_message_inbox",
		"device_message_outbox",
		"email_outbox",
		"device_operations",
		"identity_providers",
		"oidc_login_states",
		"permissions",
		"roles",
		"role_permissions",
		"role_assignments",
		"external_group_mappings",
		"acl_audit_events",
		"app_certificates",
		"end_users",
		"end_user_identities",
		"end_user_refresh_tokens",
		"brand_cloud_end_users",
		"device_user_bindings",
		"quota_raise_requests",
		"user_identities",
		"brand_cloud_users",
		"brand_cloud_memberships",
		"brand_cloud_refresh_tokens",
		"brand_cloud_owner_transfers",
		"factory_production_runs",
	}
	for _, table := range requiredTables {
		requireTable(t, ctx, env, table)
	}

	requiredColumns := map[string][]string{
		"organizations":              {"organization_kind", "status", "metadata", "tenant_slug"},
		"users":                      {"developer_cloud_limit"},
		"auth_tokens":                {"user_id", "subject_type", "subject_id", "purpose", "scope", "token_hash", "expires_at", "consumed_at"},
		"email_outbox":               {"idempotency_key", "message_type", "template_version", "payload_nonce", "payload_ciphertext", "status", "attempt_count", "available_at", "lease_until", "last_error", "expires_at", "sent_at"},
		"brand_cloud_users":          {"brand_cloud_id", "email", "password_hash", "display_name", "email_verified", "email_verified_at", "signup_pending_verification", "disabled_at"},
		"brand_cloud_memberships":    {"brand_cloud_id", "brand_cloud_user_id", "role"},
		"brand_cloud_refresh_tokens": {"brand_cloud_user_id", "brand_cloud_id", "token_hash", "expires_at", "revoked_at"},
		"brand_cloud_owner_transfers": {"brand_cloud_id", "requested_by_user_id", "target_user_id", "token_hash", "status",
			"expires_at", "accepted_at", "canceled_at"},
		"device_claim_tokens":  {"token_hash", "organization_id", "created_by", "device_item_profile_id", "revoked_at", "service_options", "metadata", "notes", "expires_at", "claimed_at"},
		"device_item_profiles": {"brand_cloud_id", "profile_key", "display_name", "status", "category", "manufacturer", "model", "metadata_defaults", "metadata_schema", "ca_profile", "issuer_profile", "service_options", "claim_policy", "provisioning_policy", "disabled_at"},
		"device_claims":        {"claim_token_id", "organization_id", "device_id", "claimed_by", "status", "provision_input", "overridden_by", "override_reason", "override_evidence", "overridden_at"},
		"device_message_inbox": {"message_id", "operation_id", "stream", "message_type", "schema_version", "partition_key", "status", "payload", "attempt_count"},
		"device_message_outbox": {"message_id", "operation_id", "stream", "message_type", "schema_version", "partition_key", "status", "payload",
			"attempt_count", "available_at"},
		"audit_events":       {"event_type", "subject_type", "subject_id", "actor_user_id", "organization_id", "payload"},
		"identity_providers": {"provider_id", "name", "type", "issuer_url", "client_id", "client_secret_ref", "scopes", "enabled", "metadata"},
		"oidc_login_states":  {"provider_id", "state_hash", "nonce_hash", "redirect_url", "post_login_redirect_url", "expires_at", "consumed_at"},
		"permissions":        {"name", "domain", "action", "description"},
		"roles":              {"name", "scope_type", "description", "system_role", "disabled_at"},
		"role_permissions":   {"role_id", "permission_id"},
		"role_assignments":   {"role_id", "actor_type", "actor_id", "scope_type", "scope_id", "organization_id", "created_by", "disabled_at"},
		"external_group_mappings": {"provider_id", "external_group", "role_id", "scope_type", "scope_id", "organization_id",
			"created_by", "disabled_at"},
		"acl_audit_events": {"event_type", "subject_type", "subject_id", "actor_user_id", "organization_id", "payload"},
		"app_certificates": {"user_id", "subject", "csr_sha256", "certificate_pem", "certificate_chain_pem", "fingerprint_sha256",
			"serial_number", "issuer_request_id", "not_before", "not_after", "revoked_at", "subject_type", "subject_id"},
		"end_users":               {"primary_email", "password_hash", "display_name", "status", "disabled_at"},
		"end_user_identities":     {"end_user_id", "identity_provider", "provider_subject", "email", "phone"},
		"end_user_refresh_tokens": {"end_user_id", "token_hash", "expires_at", "revoked_at"},
		"brand_cloud_end_users":   {"brand_cloud_id", "end_user_id", "display_alias", "status", "consent", "first_seen_at", "last_seen_at"},
		"device_user_bindings":    {"device_id", "brand_cloud_id", "end_user_id", "role", "created_from_claim_id", "disabled_at"},
		"quota_raise_requests": {"organization_id", "requested_by", "requested_quota", "status", "contact_info",
			"decision_reason"},
		"user_identities":   {"user_id", "provider_id", "issuer_url", "subject", "email", "email_verified", "claims", "linked_at", "last_login_at"},
		"device_operations": {"operation_id", "organization_id", "device_id", "operation_type", "status", "request_payload", "result_payload"},
		"factory_production_runs": {"brand_cloud_id", "device_item_profile_id", "factory_id", "batch_id", "status", "allowed_quantity",
			"issued_quantity", "valid_from", "valid_until", "created_by"},
	}
	for table, columns := range requiredColumns {
		for _, column := range columns {
			requireColumn(t, ctx, env, table, column)
		}
	}

	requiredConstraints := []struct {
		table string
		name  string
	}{
		{table: "organizations", name: "organizations_name_not_blank"},
		{table: "organizations", name: "organizations_tier_check"},
		{table: "organizations", name: "organizations_evaluation_device_quota_check"},
		{table: "organizations", name: "organizations_kind_check"},
		{table: "organizations", name: "organizations_status_check"},
		{table: "auth_tokens", name: "auth_tokens_purpose_check"},
		{table: "users", name: "users_email_normalized"},
		{table: "users", name: "users_developer_cloud_limit_check"},
		{table: "brand_cloud_owner_transfers", name: "brand_cloud_owner_transfers_status_check"},
		{table: "brand_cloud_owner_transfers", name: "brand_cloud_owner_transfers_different_users"},
		{table: "brand_cloud_users", name: "brand_cloud_users_brand_email_key"},
		{table: "brand_cloud_memberships", name: "brand_cloud_memberships_role_check"},
		{table: "brand_cloud_memberships", name: "brand_cloud_memberships_brand_user_key"},
		{table: "factory_production_runs", name: "factory_production_runs_status_check"},
		{table: "factory_production_runs", name: "factory_production_runs_allowed_quantity_positive"},
		{table: "factory_production_runs", name: "factory_production_runs_issued_quantity_non_negative"},
		{table: "factory_production_runs", name: "factory_production_runs_period_check"},
		{table: "devices", name: "devices_name_not_blank"},
		{table: "devices", name: "devices_category_check"},
		{table: "devices", name: "devices_status_check"},
		{table: "devices", name: "devices_org_id_unique"},
		{table: "organization_members", name: "organization_members_role_check"},
		{table: "device_operations", name: "device_operations_operation_id_key"},
		{table: "device_operations", name: "device_operations_operation_type_check"},
		{table: "device_operations", name: "device_operations_status_check"},
		{table: "device_message_outbox", name: "device_message_outbox_message_id_key"},
		{table: "device_message_outbox", name: "device_message_outbox_stream_check"},
		{table: "device_message_outbox", name: "device_message_outbox_message_type_check"},
		{table: "device_message_inbox", name: "device_message_inbox_message_id_key"},
		{table: "device_message_inbox", name: "device_message_inbox_stream_check"},
		{table: "device_message_inbox", name: "device_message_inbox_message_type_check"},
		{table: "device_claim_tokens", name: "device_claim_tokens_token_hash_key"},
		{table: "device_item_profiles", name: "device_item_profiles_brand_key_unique"},
		{table: "device_item_profiles", name: "device_item_profiles_status_check"},
		{table: "device_item_profiles", name: "device_item_profiles_category_check"},
		{table: "device_claims", name: "device_claims_claim_token_id_key"},
		{table: "device_claims", name: "device_claims_status_check"},
		{table: "identity_providers", name: "identity_providers_provider_id_key"},
		{table: "identity_providers", name: "identity_providers_provider_id_not_blank"},
		{table: "identity_providers", name: "identity_providers_type_check"},
		{table: "identity_providers", name: "identity_providers_client_secret_ref_check"},
		{table: "oidc_login_states", name: "oidc_login_states_state_hash_key"},
		{table: "permissions", name: "permissions_name_key"},
		{table: "permissions", name: "permissions_name_matches_parts"},
		{table: "roles", name: "roles_name_key"},
		{table: "roles", name: "roles_scope_type_check"},
		{table: "role_permissions", name: "role_permissions_pkey"},
		{table: "role_assignments", name: "role_assignments_actor_type_check"},
		{table: "role_assignments", name: "role_assignments_scope_type_check"},
		{table: "role_assignments", name: "role_assignments_scope_consistency"},
		{table: "external_group_mappings", name: "external_group_mappings_scope_consistency"},
		{table: "acl_audit_events", name: "acl_audit_events_event_type_not_blank"},
		{table: "quota_raise_requests", name: "quota_raise_requests_requested_quota_check"},
		{table: "quota_raise_requests", name: "quota_raise_requests_status_check"},
		{table: "user_identities", name: "user_identities_provider_subject_key"},
		{table: "user_identities", name: "user_identities_user_provider_key"},
		{table: "user_identities", name: "user_identities_email_normalized"},
		{table: "user_identities", name: "user_identities_email_verified_check"},
		{table: "app_certificates", name: "app_certificates_subject_type_check"},
		{table: "end_users", name: "end_users_primary_email_normalized"},
		{table: "end_users", name: "end_users_status_check"},
		{table: "end_user_identities", name: "end_user_identities_provider_subject_key"},
		{table: "brand_cloud_end_users", name: "brand_cloud_end_users_key"},
		{table: "device_user_bindings", name: "device_user_bindings_device_end_user_key"},
	}
	for _, constraint := range requiredConstraints {
		requireConstraint(t, ctx, env, constraint.table, constraint.name)
	}
	requireNoConstraint(t, ctx, env, "device_claims", "device_claims_claimed_by_fkey")

	requiredIndexes := []string{
		"audit_events_event_type_idx",
		"audit_events_subject_idx",
		"device_claim_tokens_active_idx",
		"device_claim_tokens_created_by_idx",
		"device_claim_tokens_org_idx",
		"device_claim_tokens_profile_idx",
		"device_item_profiles_brand_status_idx",
		"factory_production_runs_brand_profile_idx",
		"factory_production_runs_status_validity_idx",
		"device_claims_device_idx",
		"device_claims_org_created_idx",
		"device_claims_override_idx",
		"device_message_inbox_operation_idx",
		"device_message_inbox_status_received_idx",
		"device_message_outbox_operation_idx",
		"device_message_outbox_status_available_idx",
		"device_operations_org_device_created_idx",
		"device_operations_status_created_idx",
		"devices_org_serial_unique",
		"organizations_brand_tenant_slug_key",
		"organizations_kind_status_idx",
		"identity_providers_enabled_unique_idx",
		"oidc_login_states_provider_created_idx",
		"oidc_login_states_active_idx",
		"role_assignments_active_unique_idx",
		"role_assignments_actor_scope_idx",
		"role_assignments_org_idx",
		"external_group_mappings_active_unique_idx",
		"external_group_mappings_provider_group_idx",
		"acl_audit_events_event_type_idx",
		"acl_audit_events_subject_idx",
		"acl_audit_events_org_idx",
		"app_certificates_user_active_unique",
		"app_certificates_user_validity_idx",
		"app_certificates_subject_active_unique",
		"app_certificates_subject_validity_idx",
		"end_users_primary_email_key",
		"end_user_identities_end_user_idx",
		"end_user_refresh_tokens_user_idx",
		"brand_cloud_end_users_end_user_idx",
		"device_user_bindings_end_user_idx",
		"device_user_bindings_brand_user_idx",
		"quota_raise_requests_org_status_idx",
		"user_identities_user_idx",
	}
	for _, index := range requiredIndexes {
		requireIndex(t, ctx, env, index)
	}
}

func requireTable(t *testing.T, ctx context.Context, env storeIntegrationEnv, table string) {
	t.Helper()
	var exists bool
	if err := env.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = $1
		)
	`, table).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatalf("missing required table %s", table)
	}
}

func requireColumn(t *testing.T, ctx context.Context, env storeIntegrationEnv, table, column string) {
	t.Helper()
	var exists bool
	if err := env.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
		)
	`, table, column).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatalf("missing required column %s.%s", table, column)
	}
}

func requireConstraint(t *testing.T, ctx context.Context, env storeIntegrationEnv, table, constraint string) {
	t.Helper()
	var exists bool
	if err := env.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_constraint c
			JOIN pg_class t ON t.oid = c.conrelid
			JOIN pg_namespace n ON n.oid = t.relnamespace
			WHERE n.nspname = 'public' AND t.relname = $1 AND c.conname = $2
		)
	`, table, constraint).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatalf("missing required constraint %s on table %s", constraint, table)
	}
}

func requireNoConstraint(t *testing.T, ctx context.Context, env storeIntegrationEnv, table, constraint string) {
	t.Helper()
	var exists bool
	if err := env.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_constraint c
			JOIN pg_class t ON t.oid = c.conrelid
			JOIN pg_namespace n ON n.oid = t.relnamespace
			WHERE n.nspname = 'public' AND t.relname = $1 AND c.conname = $2
		)
	`, table, constraint).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatalf("unexpected constraint %s on table %s", constraint, table)
	}
}

func requireIndex(t *testing.T, ctx context.Context, env storeIntegrationEnv, index string) {
	t.Helper()
	var exists bool
	if err := env.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_indexes
			WHERE schemaname = 'public' AND indexname = $1
		)
	`, index).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatalf("missing required index %s", index)
	}
}
