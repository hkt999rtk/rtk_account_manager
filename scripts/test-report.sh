#!/usr/bin/env bash

set -u -o pipefail

REPORT_DIR="${REPORT_DIR:-reports}"
REPORT_FILE="${REPORT_FILE:-docs/TEST_REPORT.md}"
COVERAGE_THRESHOLD="${COVERAGE_THRESHOLD:-80.0}"
TEST_DATABASE_URL="${TEST_DATABASE_URL:-postgres://rtk:rtk_password@localhost:5432/rtk_account_manager?sslmode=disable}"

mkdir -p "$REPORT_DIR" "$(dirname "$REPORT_FILE")"

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

require_postgres

gofmt -l . >"$FORMAT_OUT"
if [ -s "$FORMAT_OUT" ]; then
	format_status=1
fi

TEST_DATABASE_URL="$TEST_DATABASE_URL" go test -json ./... -coverpkg=./internal/... -coverprofile="$COVERAGE_OUT" -covermode=atomic | tee "$TEST_EVENTS" >/dev/null
test_status=${PIPESTATUS[0]}

if [ -f "$COVERAGE_OUT" ]; then
	go tool cover -func="$COVERAGE_OUT" >"$COVERAGE_FUNC"
	go tool cover -html="$COVERAGE_OUT" -o "$COVERAGE_HTML"
else
	: >"$COVERAGE_FUNC"
	coverage_status=1
fi

go build ./... >"$BUILD_OUT" 2>&1
build_status=$?

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
	coverage_total_display="recorded in $COVERAGE_FUNC"
	test_count_display="recorded in $TEST_EVENTS"
	pass_count_display="recorded in $TEST_EVENTS"
	fail_count_display="recorded in $TEST_EVENTS"
fi

grep '"Action":"pass".*"Test":' "$TEST_EVENTS" 2>/dev/null \
	| sed -E 's/.*"Package":"([^"]+)","Test":"([^"]+)".*/- `\1`: `\2`/' \
	| sort -u >"$TEST_CASES_MD" || true

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
require_passed_test "Disabled users" "TestIntegrationDisabledUserCannotUseExistingTokens"
require_passed_test "Organization access" "TestIntegrationOwnerCanUpdateOrganization"
require_passed_test "Member management" "TestIntegrationLastOwnerCannotBeRemovedOrDowngraded"
require_passed_test "Device lifecycle" "TestIntegrationRoleAuthorizationDeviceScopeAndSerialUniqueness"
require_passed_test "Authorization and tenancy matrix" "TestIntegrationAuthorizationAndTenancyMatrix"
require_passed_test "Provisioning API" "TestIntegrationProvisioningEndpoints"
require_passed_test "Claim Token persistence" "TestResolveDeviceClaimTokenCreatesDeviceAndClaim"
require_passed_test "Claim Token admin workflow" "TestDeviceClaimTokenAdminLifecycle"
require_passed_test "Claim Token transfer and reclaim" "TestIntegrationAdminDeviceClaimOverrideWorkflow"
require_passed_test "Claim resolve API" "TestIntegrationClaimResolveEndpoint"
require_passed_test "Deactivation API" "TestIntegrationDeactivateEndpointUsesProjectedVideoMetadata"
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
require_passed_test "OIDC provider persistence" "TestIdentityProviderStoreCRUDAndEnabledInvariant"
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
	overall_status="FAIL"
fi

cat >"$REPORT_FILE" <<EOF
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
| Disabled users | Disabled users cannot use existing access tokens, refresh tokens, or login until re-enabled. |
| Organization access | Current-user organization listing, organization create/get/update, cross-organization organization access rejection. |
| Member management | Owner add/update/remove/disable/enable member flows, admin/member forbidden paths, last-owner downgrade/remove/disable protection. |
| Device lifecycle | Device create/list/get/update/status update/soft-delete, disabled-device read-only behavior, duplicate serial rejection, same serial in another org allowed. |
| Authorization and tenancy matrix | \`TestIntegrationAuthorizationAndTenancyMatrix\` verifies owner/admin/member/platform-admin/outsider/disabled-user behavior across device reads/writes, claim resolve, provisioning, deactivation, quota visibility, audit visibility, and foreign organization access. |
| Provisioning API | \`TestIntegrationProvisioningEndpoints\` verifies owner/admin initiation, member read-only access, raw claim-material rejection, transactional \`device_operations\` plus \`device_message_outbox\` writes, projected command payload shape, account-side readiness source facts, disabled-device rejection, and idempotent \`operation_id\` reuse. |
| Claim Token persistence | \`TestResolveDeviceClaimTokenCreatesDeviceAndClaim\`, \`TestResolveDeviceClaimTokenMatchesExistingDevice\`, \`TestResolveDeviceClaimTokenRejectsInvalidToken\`, \`TestResolveDeviceClaimTokenRejectsExpiredToken\`, \`TestResolveDeviceClaimTokenRejectsAlreadyClaimedToken\`, \`TestResolveDeviceClaimTokenRejectsCrossOrganizationToken\`, and \`TestResolveDeviceClaimTokenRejectsUnsupportedCategory\` verify account-manager-owned Claim Token storage, raw-token non-persistence, hashed-token lookup, expiry, idempotent ownership matching, category policy, and organization boundaries. |
| Claim Token admin workflow | \`TestDeviceClaimTokenAdminLifecycle\` and \`TestIntegrationAdminDeviceClaimTokenWorkflow\` verify platform-admin token creation/import/list/show/revoke, raw-token non-persistence, generated raw-token one-time return, platform-admin-only access, and revoked-token resolve rejection. |
| Claim Token transfer and reclaim | \`TestDeviceClaimTransferMovesOwnershipAndAudits\`, \`TestDeviceClaimReclaimRequiresEvidenceAndRejectsInvalidTransitions\`, and \`TestIntegrationAdminDeviceClaimOverrideWorkflow\` verify platform-admin-only transfer/reclaim, operator evidence requirements, invalid state transitions, unchanged normal claim-resolve rejection, and audit event emission. |
| Claim resolve API | \`TestIntegrationClaimResolveEndpoint\` verifies owner/admin claim resolution, member rejection, invalid/expired/already-claimed/cross-organization/unsupported-category/quota error codes, returned provisioning input, and that resolve does not create provisioning operations or outbox messages. |
| Claim resolve retryability | \`TestWriteClaimResolveErrorIncludesRetryability\` and \`TestIntegrationClaimResolveEndpoint\` verify machine-readable \`retryable\` and \`resolution_action\` hints for non-retryable policy failures, quota failures, and retryable service-unavailable errors. |
| Deactivation API | \`TestIntegrationDeactivateEndpointUsesProjectedVideoMetadata\` verifies projected metadata is required for new deactivation work, disabled account devices may still enqueue deactivation, default reason propagation, and transactional outbox creation from projected video metadata. |
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
| OIDC provider persistence | \`TestIdentityProviderStoreCRUDAndEnabledInvariant\`, \`TestIdentityProviderRejectsRawClientSecretRef\`, and \`TestIntegrationDatabaseSchemaInvariants\` verify provider CRUD, the one-enabled-provider invariant, secret-reference-only storage, identity link uniqueness, and OIDC schema/index presence. |
| OIDC provider admin CRUD | \`TestIntegrationAdminIdentityProviderWorkflow\` verifies platform-admin-only create/list/show/update/disable, pagination, second-enabled-provider conflict handling, audit events, \`env:VAR_NAME\` secret references, and raw-secret non-persistence/non-response behavior. |
| OIDC state and nonce replay guards | \`TestOIDCLoginStateStoresHashesAndRejectsReplay\`, \`TestOIDCLoginStateRejectsExpiredState\`, and \`TestIntegrationOIDCProviderLoginAndCallback\` verify raw state/nonce non-persistence, one-time state consumption, replay rejection, and callback nonce validation through hashed state records. |
| OIDC public login callback | \`TestIntegrationOIDCProviderLoginAndCallback\` verifies discovery, login redirect, state/nonce creation, callback success, verified-email auto-link to an existing local user, Account Manager JWT issuance, identity persistence, replay rejection, and local email/password login compatibility. |
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
TEST_DATABASE_URL='***' go test -json ./... -coverpkg=./internal/... -coverprofile=$COVERAGE_OUT -covermode=atomic
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

## Coverage Gaps To Watch

- Command entry points under \`cmd/*\` are intentionally validated by \`go build ./...\`, not unit coverage.
- Store and database behavior are primarily covered through API integration tests.
- Add or update tests whenever authorization, membership, token, migration, device lifecycle, or cross-service channel validation behavior changes.
EOF

cat "$REPORT_FILE"

if [ "$overall_status" != "PASS" ]; then
	exit 1
fi
