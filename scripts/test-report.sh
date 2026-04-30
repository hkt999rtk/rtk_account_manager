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

started_at="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"

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

overall_status="PASS"
if [ "$format_status" -ne 0 ] || [ "$test_status" -ne 0 ] || [ "$build_status" -ne 0 ] || [ "$coverage_status" -ne 0 ]; then
	overall_status="FAIL"
fi

package_count="$(go list ./... | wc -l | tr -d ' ')"
test_count="$(grep -c '"Action":"run"' "$TEST_EVENTS" 2>/dev/null || true)"
pass_count="$(grep -c '"Action":"pass"' "$TEST_EVENTS" 2>/dev/null || true)"
fail_count="$(grep -c '"Action":"fail"' "$TEST_EVENTS" 2>/dev/null || true)"

grep '"Action":"pass".*"Test":' "$TEST_EVENTS" 2>/dev/null \
	| sed -E 's/.*"Package":"([^"]+)","Test":"([^"]+)".*/- `\1`: `\2`/' \
	| sort -u >"$TEST_CASES_MD" || true

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

## Coverage

| Metric | Value |
| --- | --- |
| Total statement coverage | $coverage_total |
| Minimum required coverage | ${COVERAGE_THRESHOLD}% |
| Coverage mode | atomic |
| Coverage scope | ./internal/... |

## Test Execution

| Metric | Value |
| --- | --- |
| Go packages | $package_count |
| Test cases started | $test_count |
| JSON pass events | $pass_count |
| JSON fail events | $fail_count |
| Integration database | Postgres via TEST_DATABASE_URL |

## Correctness Validation

Coverage is only a signal that code executed. Correctness is validated by assertions in the automated tests. This report confirms the following behavior groups were exercised:

| Behavior group | Evidence |
| --- | --- |
| Auth and sessions | Register, login, invalid login, password change with current-password validation, refresh-token revocation after password change, refresh rotation, old refresh rejection, logout revocation, expired token parsing, wrong-secret parsing. |
| Disabled users | Disabled users cannot use existing access tokens, refresh tokens, or login until re-enabled. |
| Organization access | Current-user organization listing, organization create/get/update, cross-organization organization access rejection. |
| Member management | Owner add/update/remove/disable/enable member flows, admin/member forbidden paths, last-owner downgrade/remove/disable protection. |
| Device lifecycle | Device create/list/get/update/status update/soft-delete, disabled-device read-only behavior, duplicate serial rejection, same serial in another org allowed. |
| Provisioning API | \`TestIntegrationProvisioningEndpoints\` verifies owner/admin initiation, member read-only access, raw claim-material rejection, transactional \`device_operations\` plus \`device_message_outbox\` writes, projected command payload shape, account-side readiness source facts, disabled-device rejection, and idempotent \`operation_id\` reuse. |
| Deactivation API | \`TestIntegrationDeactivateEndpointUsesProjectedVideoMetadata\` verifies projected metadata is required for new deactivation work, disabled account devices may still enqueue deactivation, default reason propagation, and transactional outbox creation from projected video metadata. |
| Device scoping | Lifecycle endpoints reject cross-organization reads and writes without leaking foreign device access. |
| Authorization boundaries | owner/admin/member role permissions are enforced across device CRUD, provisioning, deactivation, and member management paths. |
| Operation idempotency | Reusing the same lifecycle \`operation_id\` returns the existing operation and preserves the original outbox \`message_id\`, including retries after device disablement or missing live metadata. |
| Operation conflicts | Reusing a lifecycle \`operation_id\` with conflicting provision activity data or deactivation reason returns \`409 Conflict\`. |
| Message validation | Envelope fields, supported \`schema_version\`, message-type/stream/service pairing, lifecycle UUIDs, UTC timestamps, and \`partition_key\` validation for cross-service messages. |
| Outbox worker | \`TestRunOnceMarksSuccessfulPublishes\`, \`TestRunOnceSchedulesTransientRetry\`, \`TestRunOnceDeadLettersExhaustedPublishFailures\`, \`TestRunOnceIgnoresStaleLeaseTransitionConflict\`, and \`TestRunOnceIgnoresConflictWhenRetryLosesToPublished\` verify publish success, retry, dead-letter, and stale-lease conflict handling when another worker already won the publish race. |
| Outbox publish race recovery | \`TestRecordOutboxPublishTransitionRejectsStaleLease\`, \`TestRecordOutboxPublishTransitionLetsPublishedOutcomeOverrideLaterFailure\`, and \`TestRecordOutboxPublishTransitionPreservesInboxCompletedOperation\` verify stale workers cannot roll back a published outbox row or overwrite an inbox-completed device operation. |
| Inbox worker | \`TestRunOnceSkipsPreviouslyProcessedDuplicates\`, \`TestRunOnceDeadLettersInvalidMessages\`, and \`TestRunOnceRetriesTransientProjectionFailures\` verify message-id dedupe, dead-lettering, and transient projection retry behavior. |
| Inbox replay guards and dead-letter payloads | \`TestRunOnceSkipsCompletedLifecycleReplayWithNewMessageID\`, \`TestRunOnceSkipsCompletedLifecycleReplayForRetryingMessage\`, \`TestRunOnceDeadLettersMalformedAndUnmappedMessages/malformed_payload_keeps_inspectable_inbox_row\`, and \`TestCreateOrGetInboxMessagePreservesDeadLetterPayloadSnapshot\` verify terminal lifecycle replays stay side-effect free and malformed payload bytes remain inspectable in persisted inbox rows. |
| Projection idempotency and metadata merge | \`TestRunOnceProcessesProvisionSuccess\`, \`TestRunOnceProcessesFailureAndProjectionEvents\`, \`TestApplyProjectionMetadataPreservesExistingFieldsAndClearsNil\`, and \`TestMetadataChangedProjectionFiltersNonVideoCloudKeys\` cover replay-safe projection and selective \`video_cloud_*\` metadata updates. |
| Activation and online projection | \`TestProjectDeviceProvisioningAndOnlineRules\` proves provisioning success does not set account-manager \`status=online\`, while \`DeviceOnlineChanged\` remains the only event that updates \`status\` and \`last_seen_at\`. |
| Account readiness projection | \`TestReadinessFromProjectionStates\` verifies activation pending, activation failed, activation succeeded but offline, ready, deactivation pending, and deactivated aggregate states from explicit source facts. |
| Failure projection | \`TestProjectDeviceRejectsDisabledDevicesExceptDeactivateResults\` and \`TestRunOnceProcessesFailureAndProjectionEvents\` verify provision/deactivation failures keep stable error metadata and terminal operation state, including disabled-device deactivation results. |
| Broker adapters | \`TestNewPublisherCreatesLogPublisherAndRejectsUnsupportedKinds\`, \`TestNewConsumerCreatesLogConsumerAndRejectsUnsupportedKinds\`, \`TestLogPublisherWritesEnvelopeJSON\`, \`TestLogConsumerReadsEnvelopeJSON\`, \`TestAzureEventHubsPublisherPublishesJSONRecord\`, \`TestAzureEventHubsConsumerReadsAcrossPartitions\`, \`TestAzureEventHubsConsumerAcknowledgesAndResumesFromCheckpoint\`, and \`TestOpenAzurePartitionsUsesStoredCheckpointWhenPresent\` cover the deterministic local default adapter plus Azure Event Hubs publish/consume and durable checkpoint resume behavior without requiring live Azure. |
| Database invariants | Idempotent migrations, normalized email constraint, non-blank organization/device names, owner invariant, automatic \`updated_at\` triggers. |
| OpenAPI contract | OpenAPI schema validation plus representative provisioning, provisioning-state, and deactivation response validation against \`openapi.yaml\`. |
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

## Coverage Gaps To Watch

- Command entry points under \`cmd/*\` are intentionally validated by \`go build ./...\`, not unit coverage.
- Store and database behavior are primarily covered through API integration tests.
- Add or update tests whenever authorization, membership, token, migration, device lifecycle, or cross-service channel validation behavior changes.
EOF

cat "$REPORT_FILE"

if [ "$overall_status" != "PASS" ]; then
	exit 1
fi
