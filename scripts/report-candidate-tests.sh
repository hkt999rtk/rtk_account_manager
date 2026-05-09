#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

assert_contains() {
	local file=$1
	local pattern=$2
	if ! grep -F -- "$pattern" "$file" >/dev/null; then
		echo "expected $file to contain: $pattern" >&2
		exit 1
	fi
}

assert_rejects() {
	local description=$1
	shift
	if "$@" >/dev/null 2>&1; then
		echo "expected rejection: $description" >&2
		exit 1
	fi
}

valid_report="$tmp_dir/docs/TEST_REPORT.md"
mkdir -p "$(dirname "$valid_report")"
cat >"$valid_report" <<'EOF'
# Test Report

Generated: test
EOF
"$repo_root/scripts/validate-report-candidate.sh" docs/TEST_REPORT.md "$valid_report" >/dev/null

assert_rejects "invalid target path" "$repo_root/scripts/validate-report-candidate.sh" docs/UNKNOWN.md "$valid_report"

dsn_report="$tmp_dir/dsn.md"
cat >"$dsn_report" <<'EOF'
# Test Report

DATABASE_URL=postgres://user:password@localhost/db
EOF
assert_rejects "raw DSN with credentials" "$repo_root/scripts/validate-report-candidate.sh" docs/TEST_REPORT.md "$dsn_report"

secret_report="$tmp_dir/secret.md"
cat >"$secret_report" <<'EOF'
# Test Report

JWT_ACCESS_SECRET=not-redacted
EOF
assert_rejects "secret env assignment" "$repo_root/scripts/validate-report-candidate.sh" docs/TEST_REPORT.md "$secret_report"

jwt_report="$tmp_dir/jwt.md"
cat >"$jwt_report" <<'EOF'
# Test Report

token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.signature
EOF
assert_rejects "JWT-looking value" "$repo_root/scripts/validate-report-candidate.sh" docs/TEST_REPORT.md "$jwt_report"

key_report="$tmp_dir/key.md"
cat >"$key_report" <<'EOF'
# Test Report

-----BEGIN PRIVATE KEY-----
EOF
assert_rejects "private key material" "$repo_root/scripts/validate-report-candidate.sh" docs/TEST_REPORT.md "$key_report"

asset="$tmp_dir/rtk_account_manager-vtest.tar.gz"
printf 'release-bundle' >"$asset"
release_output="$tmp_dir/candidates/docs/RELEASE_TEST_REPORT.md"
OUTPUT="$release_output" \
	VERSION="vtest" \
	SOURCE_COMMIT="abc123" \
	RELEASE_ASSET="$asset" \
	CONTRACTS_COMMIT="def456" \
	RUN_URL="https://github.example/run/1" \
	REPORT_GENERATED_AT="test" \
	"$repo_root/scripts/generate-release-report-candidate.sh" >/dev/null
assert_contains "$release_output" "# Release Test Report"
assert_contains "$release_output" "| Release version | \`vtest\` |"
assert_contains "$release_output" "| Source commit | \`abc123\` |"
assert_contains "$release_output" "| Contracts commit | \`def456\` |"
assert_contains "$release_output" "| Release asset SHA256 |"

evidence="$tmp_dir/deployment-evidence"
mkdir -p "$evidence"
cat >"$evidence/summary.txt" <<'EOF'
version=vready
EOF
cat >"$evidence/backup-marker-status.txt" <<'EOF'
present
EOF
cat >"$evidence/api-status.txt" <<'EOF'
     Active: active (running) since today
EOF
cat >"$evidence/cleanup-timer-status.txt" <<'EOF'
     Active: active (waiting) since today
EOF
cat >"$evidence/migrate-status.txt" <<'EOF'
     Active: inactive (dead) since today
EOF
cat >"$evidence/redacted-env-keys.txt" <<'EOF'
DATABASE_URL=<redacted>
JWT_ACCESS_SECRET=<redacted>
EOF
readiness_output="$tmp_dir/candidates/docs/READINESS_TEST_REPORT.md"
OUTPUT="$readiness_output" \
	EVIDENCE_DIR="$evidence" \
	VERIFY_RESULT="success" \
	RUN_URL="https://github.example/run/2" \
	REPORT_GENERATED_AT="test" \
	"$repo_root/scripts/generate-readiness-report-candidate.sh" >/dev/null
assert_contains "$readiness_output" "# Readiness Test Report"
assert_contains "$readiness_output" "| Deployed version | \`vready\` |"
assert_contains "$readiness_output" "| Verify result | \`success\` |"
assert_contains "$readiness_output" "- \`JWT_ACCESS_SECRET=<redacted>\`"

import_root="$tmp_dir/imported/artifact/docs"
mkdir -p "$import_root"
cp "$release_output" "$import_root/RELEASE_TEST_REPORT.md"
(
	cd "$tmp_dir"
	assert_rejects "invalid import target" "$repo_root/scripts/import-report-candidate.sh" docs/NOT_ALLOWED.md
	TARGET_REPORT=docs/RELEASE_TEST_REPORT.md IMPORT_DIR="$tmp_dir/imported" "$repo_root/scripts/import-report-candidate.sh" >/dev/null
	test -f docs/RELEASE_TEST_REPORT.md
)

echo "report candidate script tests passed"
