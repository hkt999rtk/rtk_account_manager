#!/usr/bin/env bash

set -euo pipefail

export GOWORK=off
repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

REPORT_DIR="${REPORT_DIR:-reports}"
REPORT_FILE="${REPORT_FILE:-docs/test_report.md}"
COVERAGE_THRESHOLD="${COVERAGE_THRESHOLD:-80.0}"
TEST_DATABASE_URL="${TEST_DATABASE_URL:-postgres://rtk:rtk_password@localhost:5432/rtk_account_manager?sslmode=disable}"

mkdir -p "$REPORT_DIR" "$(dirname "$REPORT_FILE")"
if ! mkdir "$REPORT_DIR/.test-report.lock" 2>/dev/null; then
	echo "another report execution owns this evidence directory: $REPORT_DIR" >&2
	exit 1
fi
report_temporary=""
report_lock=""
cleanup() {
	[ -z "$report_temporary" ] || rm -f "$report_temporary"
	[ -z "$report_lock" ] || rmdir "$report_lock"
	rmdir "$REPORT_DIR/.test-report.lock"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if ! mkdir "$REPORT_FILE.lock" 2>/dev/null; then
	echo "another report execution owns this candidate: $REPORT_FILE" >&2
	exit 1
fi
report_lock="$REPORT_FILE.lock"

TEST_EVENTS="$REPORT_DIR/test-events.json"
COVERAGE_OUT="$REPORT_DIR/coverage.out"
COVERAGE_FUNC="$REPORT_DIR/coverage.txt"
COVERAGE_HTML="$REPORT_DIR/coverage.html"
FORMAT_OUT="$REPORT_DIR/gofmt.txt"
BUILD_OUT="$REPORT_DIR/build.txt"
TEST_CASES_MD="$REPORT_DIR/test-cases.md"

started_at="${REPORT_GENERATED_AT:-$(date -u +"%Y-%m-%dT%H:%M:%SZ")}"

require_postgres() {
	local authority host port

	authority="${TEST_DATABASE_URL#*@}"
	if [ "$authority" = "$TEST_DATABASE_URL" ]; then
		return 0
	fi
	authority="${authority%%/*}"
	host="${authority%%:*}"
	port="${authority##*:}"
	if [ "$port" = "$authority" ]; then
		port=5432
	fi
	if [ -z "$host" ]; then
		return 0
	fi

	if ! ( : >/dev/tcp/"$host"/"$port" ) >/dev/null 2>&1; then
		echo "Postgres is unreachable at $host:$port from TEST_DATABASE_URL." >&2
		echo "Start the local database with 'make db-up' or point TEST_DATABASE_URL at a reachable Postgres instance before running make test-report." >&2
		exit 1
	fi
}

format_status=0
test_status=0
build_status=0
coverage_status=0
correctness_status=0

for tool in go gofmt git python3; do
	command -v "$tool" >/dev/null || { echo "required report tool is unavailable: $tool" >&2; exit 1; }
done

if [ -n "${REPORT_REUSE_DIR:-}" ]; then
	if [ "$(cd "$REPORT_REUSE_DIR" && pwd)" != "$(cd "$REPORT_DIR" && pwd)" ] && [ -d "$REPORT_REUSE_DIR/.test-report.lock" ]; then
		echo "the source evidence directory has an active writer; choose a completed execution" >&2
		exit 1
	fi
	python3 scripts/test-report-evidence.py validate "$REPORT_REUSE_DIR"
	if [ "$(cd "$REPORT_REUSE_DIR" && pwd)" != "$(cd "$REPORT_DIR" && pwd)" ]; then
		for artifact in test-events.json coverage.out gofmt.txt build.txt execution-evidence.json; do
			cp "$REPORT_REUSE_DIR/$artifact" "$REPORT_DIR/$artifact"
		done
		# Detect a source writer starting between validation and the copy. Only a
		# complete destination packet with matching hashes can be rendered.
		python3 scripts/test-report-evidence.py validate "$REPORT_DIR"
	fi
else
	# Invalidate an earlier pass before any preflight can fail. Expensive tests
	# begin only after formatting, dependencies and PostgreSQL are ready.
	rm -f "$REPORT_DIR/execution-evidence.json"
	gofmt -l . >"$FORMAT_OUT"
	if [ -s "$FORMAT_OUT" ]; then
		echo "formatting check failed; see $FORMAT_OUT" >&2
		exit 1
	fi
	go list -deps -test ./... >/dev/null
	require_postgres
	python3 scripts/test-report-evidence.py begin "$REPORT_DIR"
	# Integration packages share a database-wide advisory lock. Running packages
	# serially avoids spending their timeout queued behind another package.
	if ! TEST_DATABASE_URL="$TEST_DATABASE_URL" go test -p=1 -json -count=1 -timeout=20m ./... -coverpkg=./internal/... -coverprofile="$COVERAGE_OUT" -covermode=atomic >"$TEST_EVENTS" 2>"$REPORT_DIR/test-stderr.txt"; then
		echo "go test failed; inspect $TEST_EVENTS and $REPORT_DIR/test-stderr.txt locally. No maintained report was replaced." >&2
		exit 1
	fi
	if ! go build ./... >"$BUILD_OUT" 2>&1; then
		echo "build failed; inspect $BUILD_OUT locally. No maintained report was replaced." >&2
		exit 1
	fi
fi

if [ -f "$COVERAGE_OUT" ]; then
	go tool cover -func="$COVERAGE_OUT" >"$COVERAGE_FUNC"
	go tool cover -html="$COVERAGE_OUT" -o "$COVERAGE_HTML"
else
	: >"$COVERAGE_FUNC"
	coverage_status=1
fi

coverage_total="0.0%"
if [ -s "$COVERAGE_FUNC" ]; then
	coverage_total="$(awk '/^total:/ {print $3}' "$COVERAGE_FUNC")"
fi
coverage_number="${coverage_total%%%}"
if ! awk -v actual="$coverage_number" -v minimum="$COVERAGE_THRESHOLD" 'BEGIN { exit actual + 0 >= minimum + 0 ? 0 : 1 }'; then
	coverage_status=1
fi

package_count="$(go list ./... | wc -l | tr -d ' ')"
test_count="$(grep -c '"Action":"run"' "$TEST_EVENTS" 2>/dev/null || true)"
pass_count="$(grep -c '"Action":"pass"' "$TEST_EVENTS" 2>/dev/null || true)"
fail_count="$(grep -c '"Action":"fail"' "$TEST_EVENTS" 2>/dev/null || true)"

coverage_total_display="$coverage_total"
test_count_display="$test_count"
pass_count_display="$pass_count"
fail_count_display="$fail_count"
if [ "${REPORT_CANONICAL:-false}" = "true" ]; then
	coverage_total_display="recorded in reports/coverage.txt"
	test_count_display="recorded in reports/test-events.json"
	pass_count_display="recorded in reports/test-events.json"
	fail_count_display="recorded in reports/test-events.json"
fi

grep '"Action":"pass".*"Test":' "$TEST_EVENTS" 2>/dev/null \
	| sed -E 's/.*"Package":"([^"]+)","Test":"([^"]+)".*/- `\1`: `\2`/' \
	| LC_ALL=C sort -u >"$TEST_CASES_MD" || true

CORRECTNESS_GATES="$REPORT_DIR/correctness-gates.md"
cat >"$CORRECTNESS_GATES" <<'EOF'
| Behavior group | Required test | Result |
| --- | --- | --- |
EOF

require_passed_test() {
	local group test_name
	group="$1"
	test_name="$2"

	if grep -Eq '"Action":"pass".*"Test":"'"$test_name"'("|/)' "$TEST_EVENTS" 2>/dev/null; then
		printf '| %s | `%s` | PASS |\n' "$group" "$test_name" >>"$CORRECTNESS_GATES"
	else
		printf '| %s | `%s` | FAIL |\n' "$group" "$test_name" >>"$CORRECTNESS_GATES"
		correctness_status=1
	fi
}

require_passed_test "Auth and sessions" "TestIntegrationRegisterLoginRefreshAndLogout"
require_passed_test "Social login pending-account activation" "TestSocialLoginActivatesExistingPendingUser"
require_passed_test "Social login disabled-account policy" "TestSocialLoginDoesNotReactivateDisabledUser"
require_passed_test "Email outbox required" "TestEmailIssuanceRequiresOutbox"
require_passed_test "Disabled users" "TestIntegrationDisabledUserCannotUseExistingTokens"
require_passed_test "Organization access" "TestIntegrationOwnerCanUpdateOrganization"
require_passed_test "Member management" "TestIntegrationLastOwnerCannotBeRemovedOrDowngraded"
require_passed_test "Device lifecycle" "TestIntegrationRoleAuthorizationDeviceScopeAndSerialUniqueness"
require_passed_test "Authorization and tenancy matrix" "TestIntegrationAuthorizationAndTenancyMatrix"
require_passed_test "ACL persistence and system roles" "TestACLSeedPermissionCatalogAndSystemRoles"
require_passed_test "ACL scoped assignments" "TestACLRoleAssignmentsAuthorizeInsideScopeOnly"
require_passed_test "ACL admin workflow" "TestIntegrationACLAdminWorkflow"
require_passed_test "Provisioning API" "TestIntegrationProvisioningEndpoints"
require_passed_test "Claim Token persistence" "TestResolveDeviceClaimTokenCreatesDeviceAndClaim"
require_passed_test "Claim Token admin workflow" "TestDeviceClaimTokenAdminLifecycle"
require_passed_test "Claim Token transfer and reclaim" "TestIntegrationAdminDeviceClaimOverrideWorkflow"
require_passed_test "Claim resolve API" "TestIntegrationClaimResolveEndpoint"
require_passed_test "Deactivation API" "TestIntegrationDeactivateEndpointUsesProjectedVideoMetadata"
require_passed_test "Device user unprovision" "TestIntegrationDeviceUserUnprovisionWorkflow"
require_passed_test "Device unprovision override" "TestIntegrationAdminDeviceUnprovisionOverride"
require_passed_test "Message validation" "TestValidateRejectsEnvelopeContractMismatches"
require_passed_test "Contract parser fuzz seeds" "FuzzEnvelopeStrictJSONAndValidation"
require_passed_test "Strict API bind fuzz seeds" "FuzzBindStrictRequestShape"
require_passed_test "Outbox worker" "TestRunOnceMarksSuccessfulPublishes"
require_passed_test "Inbox worker" "TestRunOnceSkipsPreviouslyProcessedDuplicates"
require_passed_test "Projection idempotency and metadata merge" "TestApplyProjectionMetadataPreservesExistingFieldsAndClearsNil"
require_passed_test "Account readiness projection" "TestReadinessFromProjectionStates"
require_passed_test "Registry-only readiness" "TestIntegrationProvisioningStateReturnsRegistryOnlyReadiness"
require_passed_test "Admin quota and audit visibility" "TestIntegrationSignupEvaluationQuotaAndRaiseWorkflow"
require_passed_test "Lifecycle observability" "TestIntegrationAdminMetricsIncludesLifecycleVisibility"
require_passed_test "Broker adapters" "TestAzureEventHubsPublisherPublishesJSONRecord"
require_passed_test "Database invariants" "TestIntegrationDatabaseSchemaInvariants"
require_passed_test "OpenAPI contract" "TestIntegrationResponsesMatchOpenAPIContract"
require_passed_test "Global user authentication and retired tenant auth" "TestIntegrationPlatformAdminCreatesActiveBrandCloudUser"
require_passed_test "Global app certificate rotation" "TestIntegrationGlobalLoginExplicitlyRotatesAppCertificate"
require_passed_test "Developer-owned brand clouds" "TestIntegrationDeveloperSignupCreatesDefaultBrandCloudAndDeveloperCanCreateWithinLimit"
require_passed_test "Brand-cloud owner transfer" "TestIntegrationBrandCloudOwnerTransferRequiresEmailTokenAndTargetSession"
require_passed_test "ChipSet SDK provider lifecycle and ACL" "TestIntegrationChipsetProviderACLRefreshVisibilityAndAudit"
require_passed_test "OIDC provider persistence" "TestIdentityProviderStoreCRUDAndMultipleEnabledProviders"
require_passed_test "OIDC provider admin CRUD" "TestIntegrationAdminIdentityProviderWorkflow"
require_passed_test "OIDC state and nonce replay guards" "TestOIDCLoginStateStoresHashesAndRejectsReplay"
require_passed_test "OIDC public login callback" "TestIntegrationOIDCProviderLoginAndCallback"
require_passed_test "OIDC unknown disabled and unverified users" "TestIntegrationOIDCCallbackRejectsUnknownDisabledAndUnverifiedUsers"
require_passed_test "OIDC disabled provider behavior" "TestIntegrationOIDCDisabledDiscoveryAndLogin"
require_passed_test "OIDC current-user identities" "TestIntegrationCurrentUserOIDCIdentityManagement"
require_passed_test "OIDC token validation" "TestOIDCClientExchangeAndValidateIDToken"
require_passed_test "OIDC invalid token rejection" "TestOIDCClientRejectsInvalidNonce"
require_passed_test "OIDC secret redaction" "TestOIDCTokenErrorsDoNotContainProviderTokens"
require_passed_test "OIDC raw secret rejection" "TestIdentityProviderRejectsRawClientSecretRef"
require_passed_test "Configuration and maintenance" "TestLoadReadsEnvironmentAndDurations"

overall_status="PASS"
if [ "$format_status" -ne 0 ] || [ "$test_status" -ne 0 ] || [ "$build_status" -ne 0 ] || [ "$coverage_status" -ne 0 ] || [ "$correctness_status" -ne 0 ]; then
	echo "coverage or required correctness gates failed; see $COVERAGE_FUNC and $CORRECTNESS_GATES. No maintained report was replaced." >&2
	exit 1
fi

if [ -z "${REPORT_REUSE_DIR:-}" ]; then
	python3 scripts/test-report-evidence.py seal "$REPORT_DIR"
fi

report_temporary="$(mktemp "$(dirname "$REPORT_FILE")/.test-report.XXXXXX")"
cat >"$report_temporary" <<EOF
# Test Report

Generated: $started_at

## Summary

| Check | Result |
| --- | --- |
| Overall | $overall_status |
| Formatting | $(if [ "$format_status" -eq 0 ]; then echo PASS; else echo FAIL; fi) |
| Tests | $(if [ "$test_status" -eq 0 ]; then echo PASS; else echo FAIL; fi) |
| Build | $(if [ "$build_status" -eq 0 ]; then echo PASS; else echo FAIL; fi) |
| Coverage threshold | $(if [ "$coverage_status" -eq 0 ]; then echo PASS; else echo FAIL; fi) |
| Correctness gates | $(if [ "$correctness_status" -eq 0 ]; then echo PASS; else echo FAIL; fi) |

## Coverage

| Metric | Value |
| --- | --- |
| Total statement coverage | $coverage_total_display |
| Minimum required coverage | ${COVERAGE_THRESHOLD}% |
| Coverage mode | atomic |
| Coverage scope | ./internal/... |

## Test Execution

| Metric | Value |
| --- | --- |
| Go packages | $package_count |
| Test cases started | $test_count_display |
| JSON pass events | $pass_count_display |
| JSON fail events | $fail_count_display |
| Integration database | Postgres via TEST_DATABASE_URL |

## Correctness Gates

\`make test-report\` fails when any required behavior group below is missing a passing representative test. These gates protect the maintained report from drifting away from the executable suite.

$(cat "$CORRECTNESS_GATES")

## Correctness Validation

Coverage is only a signal that code executed. Correctness is validated by assertions in the automated tests. This report confirms the following behavior groups were exercised:

| Behavior group | Evidence |
| --- | --- |
| Auth and sessions | Register, login, invalid login, password change with current-password validation, refresh-token revocation after password change, refresh rotation, old refresh rejection, logout revocation, expired token parsing, wrong-secret parsing. |
| Social login account activation | \`TestSocialLoginActivatesExistingPendingUser\` verifies a Google or GitHub verified email activates the matching pending account whether matched by email or an existing identity, links the identity to that account, and invalidates outstanding email-verification tokens. \`TestSocialLoginDoesNotReactivateDisabledUser\` verifies social login cannot bypass an administrator-disabled account. |
| Disabled users | Disabled users cannot use existing access tokens, refresh tokens, or login until re-enabled. |
| Organization access | Current-user organization listing, organization create/get/update, cross-organization organization access rejection. |
| Member management | Owner add/update/remove/disable/enable member flows, admin/member forbidden paths, last-owner downgrade/remove/disable protection. |
| Device lifecycle | Device create/list/get/update/status update/soft-delete, disabled-device read-only behavior, duplicate serial rejection, same serial in another org allowed. |
| Authorization and tenancy matrix | \`TestIntegrationAuthorizationAndTenancyMatrix\` verifies owner/admin/member/platform-admin/outsider/disabled-user behavior across device reads/writes, claim resolve, provisioning, deactivation, quota visibility, audit visibility, and foreign organization access. |
| ACL persistence and system roles | \`TestACLSeedPermissionCatalogAndSystemRoles\`, \`TestACLRoleAssignmentsAuthorizeInsideScopeOnly\`, and \`TestIntegrationDatabaseSchemaInvariants\` verify ACL tables, indexes, permission catalog seed, system roles, explicit role-permission bindings, scoped assignment behavior, and read-only observer write denial. |
| ACL admin workflow | \`TestIntegrationACLAdminWorkflow\` verifies platform-admin-only permission/role catalog access, role create/show/update/delete, permission binding, role assignment create/list/delete, external group mapping create/list/delete, and ACL audit event listing. |
| Provisioning API | \`TestIntegrationProvisioningEndpoints\` verifies owner/admin/member initiation, raw claim-material rejection, transactional \`device_operations\` plus \`device_message_outbox\` writes, projected command payload shape, account-side readiness source facts, disabled-device rejection, and idempotent \`operation_id\` reuse. |
| Claim Token persistence | \`TestResolveDeviceClaimTokenCreatesDeviceAndClaim\`, \`TestResolveDeviceClaimTokenRejectsDuplicateDeviceClaim\`, \`TestResolveDeviceClaimTokenRejectsInvalidToken\`, \`TestResolveDeviceClaimTokenRejectsExpiredToken\`, \`TestResolveDeviceClaimTokenRejectsAlreadyClaimedToken\`, \`TestResolveDeviceClaimTokenRejectsCrossOrganizationToken\`, and \`TestResolveDeviceClaimTokenRejectsUnsupportedCategory\` verify account-manager-owned Claim Token storage, raw-token non-persistence, hashed-token lookup, expiry, duplicate device rejection, category policy, and organization boundaries. |
| Claim Token admin workflow | \`TestDeviceClaimTokenAdminLifecycle\` and \`TestIntegrationAdminDeviceClaimTokenWorkflow\` verify platform-admin token creation/import/list/show/revoke, raw-token non-persistence, generated raw-token one-time return, platform-admin-only access, and revoked-token resolve rejection. |
| Claim Token transfer and reclaim | \`TestDeviceClaimTransferMovesOwnershipAndAudits\`, \`TestDeviceClaimReclaimRequiresEvidenceAndRejectsInvalidTransitions\`, and \`TestIntegrationAdminDeviceClaimOverrideWorkflow\` verify platform-admin-only transfer/reclaim, operator evidence requirements, invalid state transitions, unchanged normal claim-resolve rejection, and audit event emission. |
| Claim resolve API | \`TestIntegrationClaimResolveEndpoint\` verifies owner/admin/member claim resolution, invalid/expired/already-claimed/cross-organization/unsupported-category/quota error codes, returned provisioning input with service options, and that resolve does not create provisioning operations or outbox messages. |
| Claim resolve retryability | \`TestWriteClaimResolveErrorIncludesRetryability\` and \`TestIntegrationClaimResolveEndpoint\` verify machine-readable \`retryable\` and \`resolution_action\` hints for non-retryable policy failures, quota failures, and retryable service-unavailable errors. |
| Deactivation API | \`TestIntegrationDeactivateEndpointUsesProjectedVideoMetadata\` verifies projected metadata is required for new deactivation work, disabled account devices may still enqueue deactivation, default reason propagation, and transactional outbox creation from projected video metadata. |
| Device user unprovision | \`TestIntegrationDeviceUserUnprovisionWorkflow\` verifies member self-service unprovision releases the user/org binding, old users can no longer read or deactivate the device, the original Claim Token remains one-time, a fresh Claim Token can onboard the same factory identity again, and outsiders are rejected. |
| Device unprovision override | \`TestIntegrationAdminDeviceUnprovisionOverride\` verifies platform-admin override requires reason/evidence, non-platform users are rejected, and \`device_unprovisioned\` audit events are written. |
| Device scoping | Lifecycle endpoints reject cross-organization reads and writes without leaking foreign device access. |
| Authorization boundaries | owner/admin/member role permissions are enforced across device CRUD, provisioning, deactivation, and member management paths. |
| Operation idempotency | Reusing the same lifecycle \`operation_id\` returns the existing operation and preserves the original outbox \`message_id\`, including retries after device disablement or missing live metadata. |
| Operation conflicts | Reusing a lifecycle \`operation_id\` with conflicting provision activity data or deactivation reason returns \`409 Conflict\`. |
| Message validation | Envelope fields, supported \`schema_version\`, message-type/stream/service pairing, lifecycle UUIDs, UTC timestamps, and \`partition_key\` validation for cross-service messages. |
| Contract parser fuzz seeds | \`FuzzEnvelopeStrictJSONAndValidation\` and \`FuzzBindStrictRequestShape\` run seeded malformed JSON, unknown-field, wrong stream/message type, wrong partition key, and strict bind request-shape cases during normal \`go test\`. |
| Outbox worker | \`TestRunOnceMarksSuccessfulPublishes\`, \`TestRunOnceSchedulesTransientRetry\`, \`TestRunOnceDeadLettersExhaustedPublishFailures\`, \`TestRunOnceIgnoresStaleLeaseTransitionConflict\`, and \`TestRunOnceIgnoresConflictWhenRetryLosesToPublished\` verify publish success, retry, dead-letter, and stale-lease conflict handling when another worker already won the publish race. |
| Outbox publish race recovery | \`TestRecordOutboxPublishTransitionRejectsStaleLease\`, \`TestRecordOutboxPublishTransitionLetsPublishedOutcomeOverrideLaterFailure\`, and \`TestRecordOutboxPublishTransitionPreservesInboxCompletedOperation\` verify stale workers cannot roll back a published outbox row or overwrite an inbox-completed device operation. |
| Inbox worker | \`TestRunOnceSkipsPreviouslyProcessedDuplicates\`, \`TestRunOnceDeadLettersInvalidMessages\`, and \`TestRunOnceRetriesTransientProjectionFailures\` verify message-id dedupe, dead-lettering, and transient projection retry behavior. |
| Inbox replay guards and dead-letter payloads | \`TestRunOnceSkipsCompletedLifecycleReplayWithNewMessageID\`, \`TestRunOnceSkipsCompletedLifecycleReplayForRetryingMessage\`, \`TestRunOnceDeadLettersMalformedAndUnmappedMessages/malformed_payload_keeps_inspectable_inbox_row\`, and \`TestCreateOrGetInboxMessagePreservesDeadLetterPayloadSnapshot\` verify terminal lifecycle replays stay side-effect free and malformed payload bytes remain inspectable in persisted inbox rows. |
| Projection idempotency and metadata merge | \`TestRunOnceProcessesProvisionSuccess\`, \`TestRunOnceProcessesFailureAndProjectionEvents\`, \`TestApplyProjectionMetadataPreservesExistingFieldsAndClearsNil\`, and \`TestMetadataChangedProjectionFiltersNonVideoCloudKeys\` cover replay-safe projection and selective \`video_cloud_*\` metadata updates. |
| Activation and online projection | \`TestProjectDeviceProvisioningAndOnlineRules\` proves provisioning success does not set account-manager \`status=online\`, while \`DeviceOnlineChanged\` remains the only event that updates \`status\` and \`last_seen_at\`. |
| Account readiness projection | \`TestReadinessFromProjectionStates\` verifies activation pending, activation failed, activation succeeded but offline, ready, deactivation pending, deactivation failed, and deactivated aggregate states from explicit source facts. |
| Failure projection | \`TestProjectDeviceRejectsDisabledDevicesExceptDeactivateResults\` and \`TestRunOnceProcessesFailureAndProjectionEvents\` verify provision/deactivation failures keep stable error metadata and terminal operation state, including disabled-device deactivation results. |
| Readiness failure attribution | \`TestReadinessFromProjectionStates\`, \`TestIntegrationProvisioningEndpoints\`, and \`TestIntegrationDeactivateEndpointUsesProjectedVideoMetadata\` verify failed/dead-lettered provisioning and deactivation responses include \`readiness.failure\` with layer, source state, retryability, error fields, operation id, and occurrence time while pending states omit false failure details. |
| Registry-only readiness | \`TestIntegrationProvisioningStateReturnsRegistryOnlyReadiness\` verifies enabled and disabled registry-only devices return \`200 OK\`, \`operation: null\`, account-side readiness, \`product_state=registered\`, and preserve \`404 Not Found\` for truly missing devices. |
| Admin quota and audit visibility | \`TestIntegrationSignupEvaluationQuotaAndRaiseWorkflow\`, \`TestIntegrationResponsesMatchOpenAPIContract\`, and \`TestListAuditEventsReturnsRecordedLifecycleEvents\` verify platform-admin-only quota request list/show, audit event filters, pagination metadata, and existing approve/decline behavior. |
| Lifecycle observability | \`TestIntegrationAdminMetricsIncludesLifecycleVisibility\` and \`TestLifecycleMetricsAggregatesQueueAndOperationHealth\` verify platform-admin-only lifecycle metrics for outbox/inbox status counts, dead-letter breakdowns, operation status/type counts, and active-operation age. |
| Broker adapters | \`TestNewPublisherCreatesLogPublisherAndRejectsUnsupportedKinds\`, \`TestNewConsumerCreatesLogConsumerAndRejectsUnsupportedKinds\`, \`TestLogPublisherWritesEnvelopeJSON\`, \`TestLogConsumerReadsEnvelopeJSON\`, \`TestAzureEventHubsPublisherPublishesJSONRecord\`, \`TestAzureEventHubsConsumerReadsAcrossPartitions\`, \`TestAzureEventHubsConsumerAcknowledgesAndResumesFromCheckpoint\`, and \`TestOpenAzurePartitionsUsesStoredCheckpointWhenPresent\` cover the deterministic local default adapter plus Azure Event Hubs publish/consume and durable checkpoint resume behavior without requiring live Azure. |
| Database invariants | \`TestIntegrationDatabaseSchemaInvariants\` plus existing migration tests verify idempotent migrations, normalized email constraint, non-blank organization/device names, owner invariant, critical tables/columns/constraints/indexes, and automatic \`updated_at\` triggers. |
| OpenAPI contract | \`TestIntegrationResponsesMatchOpenAPIContract\` plus OpenAPI schema validation cover representative Claim Token resolve/admin, registry-only provisioning-state with nullable \`operation\`, provisioned/failed provisioning-state, provisioning, deactivation, quota visibility, audit visibility, public OIDC, current-user identity, and admin identity-provider responses against \`openapi.yaml\`. |
| Global user authentication and retired tenant auth | \`TestGlobalEmailLoginDoesNotDependOnMembershipNamespace\`, \`TestIntegrationPlatformAdminCreatesActiveBrandCloudUser\`, \`TestIntegrationRetiredTenantAuthenticationAndTokensRejected\`, and \`TestMultiCloudRegisterSignupOnboardingParityIntegration\` verify one global human identity across memberships, global login and activation, tenant auth endpoint retirement, legacy JWT rejection, and organization-scoped authorization. |
| Global app certificate rotation | \`TestIntegrationGlobalLoginExplicitlyRotatesAppCertificate\` verifies that authenticated global login requires an explicit rotation flag and same-user CSR, atomically revokes the previous active certificate, accepts the replacement, and keeps unflagged CSR login idempotent without any tenant identity input. |
| Developer-owned brand clouds | \`TestDeveloperSignupCreatesDefaultBrandCloudAndEnforcesCloudLimit\`, \`TestEnsurePlatformAdminCreatesRealtekConnectBrandCloud\`, and \`TestIntegrationDeveloperSignupCreatesDefaultBrandCloudAndDeveloperCanCreateWithinLimit\` verify global developer signup, default brand cloud creation, root \`Realtek Connect+\` bootstrap, and developer cloud limits. |
| Brand-cloud owner transfer | \`TestBrandCloudOwnerTransferRequiresExistingTargetAndAcceptsWithLoggedInDeveloper\` and \`TestIntegrationBrandCloudOwnerTransferRequiresEmailTokenAndTargetSession\` verify existing-target checks, email token delivery, target-session acceptance, old-owner downgrade, and replay rejection. |
| ChipSet SDK information providers | \`TestIntegrationChipsetProviderACLRefreshVisibilityAndAudit\`, \`TestChipsetProviderSnapshotLifecycle\`, and manifest fetch security tests verify independent read/edit/publish ACLs, draft/published/unpublished visibility, synchronous and background refresh, ETag 304, atomic snapshots, stale last-known-good fallback, audit correlation, SSRF controls, timeout, redirect, response-size, and JSON-complexity limits. |
| OIDC provider persistence | \`TestIdentityProviderStoreCRUDAndMultipleEnabledProviders\`, \`TestIdentityProviderRejectsRawClientSecretRef\`, and \`TestIntegrationDatabaseSchemaInvariants\` verify provider CRUD, multiple enabled providers, secret-reference-only storage, identity link uniqueness, and OIDC schema/index presence. |
| OIDC provider admin CRUD | \`TestIntegrationAdminIdentityProviderWorkflow\` verifies platform-admin-only create/list/show/update/disable, pagination, multiple enabled providers, audit events, \`env:VAR_NAME\` secret references, and raw-secret non-persistence/non-response behavior. |
| OIDC state and nonce replay guards | \`TestOIDCLoginStateStoresHashesAndRejectsReplay\`, \`TestOIDCLoginStateRejectsExpiredState\`, and \`TestIntegrationOIDCProviderLoginAndCallback\` verify raw state/nonce non-persistence, one-time state consumption, replay rejection, and callback nonce validation through hashed state records. |
| OIDC public login callback | \`TestIntegrationOIDCProviderLoginAndCallback\` verifies discovery, login redirect, state/nonce creation, callback success, verified-email auto-link to an existing local user, external group mapping to scoped product role assignment, Account Manager JWT issuance, identity persistence, replay rejection, and local email/password login compatibility. |
| OIDC user rejection policy | \`TestIntegrationOIDCCallbackRejectsUnknownDisabledAndUnverifiedUsers\` verifies unknown users return \`user_not_provisioned\`, disabled linked users cannot login through SSO, and unverified provider emails return \`unverified_oidc_email\`. |
| OIDC disabled provider behavior | \`TestIntegrationOIDCDisabledDiscoveryAndLogin\` verifies disabled OIDC returns no public providers and rejects login with \`oidc_disabled\`. |
| OIDC current-user identities | \`TestIntegrationCurrentUserOIDCIdentityManagement\` and \`TestIntegrationDisabledUserCannotManageOIDCIdentities\` verify current-user list/unlink behavior, cross-user isolation, disabled-user rejection, and that unlinking an identity does not break local password login. |
| OIDC token validation | \`TestOIDCClientExchangeAndValidateIDToken\`, \`TestOIDCClientRejectsInvalidIssuer\`, \`TestOIDCClientRejectsInvalidAudience\`, \`TestOIDCClientRejectsInvalidSignature\`, \`TestOIDCClientRejectsExpiredToken\`, \`TestOIDCClientRejectsInvalidNonce\`, \`TestOIDCClientRejectsUnverifiedEmail\`, \`TestOIDCClientRejectsUnexpectedSigningMethod\`, and JWKS/discovery/token-response negative tests verify authorization-code exchange and ID-token issuer, audience, signature, expiry, nonce, signing method, and verified-email validation without live Keycloak. |
| OIDC secret redaction | \`TestOIDCTokenErrorsDoNotContainProviderTokens\`, \`TestIdentityProviderRejectsRawClientSecretRef\`, and \`TestIntegrationAdminIdentityProviderWorkflow\` verify Keycloak token values and raw client secrets are not persisted, returned, or included in typed validation errors. |
| Configuration and maintenance | \`.env\` loading, TTL parsing/fallbacks, worker-specific broker config defaults, required JWT secrets, and refresh-token cleanup behavior. |

## Executed Test Cases

$(if [ -s "$TEST_CASES_MD" ]; then cat "$TEST_CASES_MD"; else echo "No test case list was captured."; fi)

## Commands

\`\`\`sh
gofmt -l .
GOWORK=off TEST_DATABASE_URL='***' go test -p=1 -json -count=1 -timeout=20m ./... -coverpkg=./internal/... -coverprofile=$COVERAGE_OUT -covermode=atomic
go tool cover -func=$COVERAGE_OUT
go tool cover -html=$COVERAGE_OUT -o $COVERAGE_HTML
go build ./...
\`\`\`

## Artifacts

| Artifact | Purpose |
| --- | --- |
| $TEST_EVENTS | Machine-readable Go test event log. |
| $COVERAGE_OUT | Go coverage profile. |
| $COVERAGE_FUNC | Function-level coverage summary. |
| $COVERAGE_HTML | HTML coverage report. |
| $FORMAT_OUT | Files requiring gofmt, empty when formatting passes. |
| $BUILD_OUT | Build output, empty when build passes. |
| $TEST_CASES_MD | Markdown list of passing test cases captured from Go JSON events. |
| $CORRECTNESS_GATES | Required correctness behavior gates and pass/fail status. |
| $REPORT_DIR/execution-evidence.json | Completed execution provenance and artifact hashes; required for report-only rendering. |

## Coverage Gaps To Watch

- Command entry points under \`cmd/*\` are intentionally validated by \`go build ./...\`, not unit coverage.
- Store and database behavior are primarily covered through API integration tests.
- Add or update tests whenever authorization, membership, token, migration, device lifecycle, or cross-service channel validation behavior changes.
EOF

if [ "${REPORT_CANONICAL:-false}" = "true" ]; then
	# Canonical references are stable across local and CI evidence directories.
	python3 - "$report_temporary" "$REPORT_DIR" <<'PY'
from pathlib import Path
import sys
path = Path(sys.argv[1])
path.write_text(path.read_text().replace(sys.argv[2].rstrip('/') + '/', 'reports/'))
PY
fi
./scripts/validate-report-candidate.sh docs/test_report.md "$report_temporary" >/dev/null
mv "$report_temporary" "$REPORT_FILE"
report_temporary=""
echo "test report PASS: $REPORT_FILE (execution evidence: $REPORT_DIR/execution-evidence.json)"
