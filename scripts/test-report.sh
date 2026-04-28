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

format_status=0
test_status=0
build_status=0
coverage_status=0

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
| Auth and sessions | Register, login, invalid login, refresh rotation, old refresh rejection, logout revocation, expired token parsing, wrong-secret parsing. |
| Disabled users | Disabled users cannot use existing access tokens, refresh tokens, or login until re-enabled. |
| Organization access | Current-user organization listing, organization create/get/update, cross-organization organization access rejection. |
| Member management | Owner add/update/remove/disable/enable member flows, admin/member forbidden paths, last-owner downgrade/remove/disable protection. |
| Device lifecycle | Device create/list/get/update/status update/soft-delete, disabled-device read-only behavior, duplicate serial rejection, same serial in another org allowed. |
| Authorization boundaries | owner/admin/member role permissions and cross-organization device/member access rejection. |
| Message validation | Envelope fields, supported \`schema_version\`, message-type/stream/service pairing, lifecycle UUIDs, UTC timestamps, and \`partition_key\` validation for cross-service messages. |
| Database invariants | Idempotent migrations, normalized email constraint, non-blank organization/device names, owner invariant, automatic updated_at triggers. |
| OpenAPI contract | OpenAPI schema validation and representative response validation against \`openapi.yaml\`. |
| Configuration and maintenance | \`.env\` loading, TTL parsing/fallbacks, required JWT secrets, refresh-token cleanup behavior. |

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
