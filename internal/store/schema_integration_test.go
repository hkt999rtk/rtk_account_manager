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
		"device_message_inbox",
		"device_message_outbox",
		"device_operations",
		"quota_raise_requests",
	}
	for _, table := range requiredTables {
		requireTable(t, ctx, env, table)
	}

	requiredColumns := map[string][]string{
		"device_claim_tokens":  {"token_hash", "organization_id", "created_by", "revoked_at", "metadata", "notes", "expires_at", "claimed_at"},
		"device_claims":        {"claim_token_id", "organization_id", "device_id", "claimed_by", "status", "provision_input", "overridden_by", "override_reason", "override_evidence", "overridden_at"},
		"device_message_inbox": {"message_id", "operation_id", "stream", "message_type", "schema_version", "partition_key", "status", "payload", "attempt_count"},
		"device_message_outbox": {"message_id", "operation_id", "stream", "message_type", "schema_version", "partition_key", "status", "payload",
			"attempt_count", "available_at"},
		"audit_events":         {"event_type", "subject_type", "subject_id", "actor_user_id", "organization_id", "payload"},
		"quota_raise_requests": {"organization_id", "requested_by", "requested_quota", "status", "contact_info", "decision_reason"},
		"device_operations":    {"operation_id", "organization_id", "device_id", "operation_type", "status", "request_payload", "result_payload"},
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
		{table: "users", name: "users_email_normalized"},
		{table: "devices", name: "devices_name_not_blank"},
		{table: "devices", name: "devices_category_check"},
		{table: "devices", name: "devices_status_check"},
		{table: "devices", name: "devices_org_id_unique"},
		{table: "organization_members", name: "organization_members_role_check"},
		{table: "device_operations", name: "device_operations_operation_id_key"},
		{table: "device_operations", name: "device_operations_operation_type_check"},
		{table: "device_operations", name: "device_operations_status_check"},
		{table: "device_operations", name: "device_operations_org_device_fkey"},
		{table: "device_message_outbox", name: "device_message_outbox_message_id_key"},
		{table: "device_message_outbox", name: "device_message_outbox_stream_check"},
		{table: "device_message_outbox", name: "device_message_outbox_message_type_check"},
		{table: "device_message_inbox", name: "device_message_inbox_message_id_key"},
		{table: "device_message_inbox", name: "device_message_inbox_stream_check"},
		{table: "device_message_inbox", name: "device_message_inbox_message_type_check"},
		{table: "device_claim_tokens", name: "device_claim_tokens_token_hash_key"},
		{table: "device_claims", name: "device_claims_claim_token_id_key"},
		{table: "device_claims", name: "device_claims_status_check"},
		{table: "quota_raise_requests", name: "quota_raise_requests_requested_quota_check"},
		{table: "quota_raise_requests", name: "quota_raise_requests_status_check"},
	}
	for _, constraint := range requiredConstraints {
		requireConstraint(t, ctx, env, constraint.table, constraint.name)
	}

	requiredIndexes := []string{
		"audit_events_event_type_idx",
		"audit_events_subject_idx",
		"device_claim_tokens_active_idx",
		"device_claim_tokens_created_by_idx",
		"device_claim_tokens_org_idx",
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
		"quota_raise_requests_org_status_idx",
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
