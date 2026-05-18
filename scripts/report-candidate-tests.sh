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

release_dir="$tmp_dir/rtk_account_manager-vtest"
mkdir -p "$release_dir/bin" "$release_dir/deploy/systemd" "$release_dir/migrations"
for executable in \
	bin/rtk-account-manager \
	bin/rtk-account-manager-migrate \
	bin/rtk-account-manager-outbox-worker \
	bin/rtk-account-manager-inbox-worker \
	bin/rtk-account-manager-cleanup-tokens \
	deploy/install.sh \
	deploy/verify.sh; do
	printf '#!/usr/bin/env sh\nexit 0\n' >"$release_dir/$executable"
	chmod +x "$release_dir/$executable"
done
for file in \
	deploy/account-manager.env.example \
	deploy/systemd/rtk-account-manager.service \
	deploy/systemd/rtk-account-manager-migrate.service \
	deploy/systemd/rtk-account-manager-outbox-worker.service \
	deploy/systemd/rtk-account-manager-inbox-worker.service \
	deploy/systemd/rtk-account-manager-cleanup-tokens.service \
	deploy/systemd/rtk-account-manager-cleanup-tokens.timer \
	release-manifest.txt \
	migrations/001_test.sql; do
	printf 'test\n' >"$release_dir/$file"
done
asset="$tmp_dir/rtk_account_manager-vtest.tar.gz"
tar -C "$tmp_dir" -czf "$asset" "$(basename "$release_dir")"
object_output="$tmp_dir/object-output"
mkdir -p "$object_output"
VERSION="vtest" \
	SOURCE_COMMIT="abc123" \
	RELEASE_ASSET="$asset" \
	OUTPUT_DIR="$object_output" \
	"$repo_root/scripts/prepare-linode-release-objects.sh" >/dev/null
"$repo_root/scripts/verify-linode-release-objects.sh" vtest "$object_output" >/dev/null
release_output="$tmp_dir/candidates/docs/RELEASE_TEST_REPORT.md"
OUTPUT="$release_output" \
	VERSION="vtest" \
	SOURCE_COMMIT="abc123" \
	RELEASE_ASSET="$asset" \
	CONTRACTS_COMMIT="def456" \
	OBJECT_STORAGE_BUNDLE_KEY="releases/rtk_account_manager-vtest/vtest.tar.gz" \
	OBJECT_STORAGE_CHECKSUM_KEY="releases/rtk_account_manager-vtest/vtest.tar.gz.sha256" \
	OBJECT_STORAGE_MANIFEST_KEY="releases/rtk_account_manager-vtest/manifest.json" \
	RUN_URL="https://github.example/run/1" \
	REPORT_GENERATED_AT="test" \
	"$repo_root/scripts/generate-release-report-candidate.sh" >/dev/null
assert_contains "$release_output" "# Release Test Report"
assert_contains "$release_output" "| Release version | \`vtest\` |"
assert_contains "$release_output" "| Source commit | \`abc123\` |"
assert_contains "$release_output" "| Contracts commit | \`def456\` |"
assert_contains "$release_output" "| Release asset SHA256 |"
assert_contains "$release_output" "| Object Storage SHA256 |"
assert_contains "$release_output" "| Object Storage bundle key | \`releases/rtk_account_manager-vtest/vtest.tar.gz\` |"
assert_contains "$release_output" "| Object Storage manifest key | \`releases/rtk_account_manager-vtest/manifest.json\` |"

evidence="$tmp_dir/deployment-evidence"
mkdir -p "$evidence"
cat >"$evidence/summary.txt" <<'EOF'
version=vready
EOF
cat >"$evidence/backup-marker-status.txt" <<'EOF'
present 2026-05-13T00:00:00Z
EOF
cat >"$evidence/production-evidence.txt" <<'EOF'
restore_drill_reference=runbook-restore-2026-05-13
smtp_mode=log-only
broker_mode=disabled
EOF
cat >"$evidence/smoke-results.txt" <<'EOF'
health=PASS
login=PASS
organization_read=PASS
device_read=PASS
provisioning_readiness=SKIP:not-readable-or-not-selected
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
assert_contains "$readiness_output" "| Backup marker status | \`present 2026-05-13T00:00:00Z\` |"
assert_contains "$readiness_output" "| Restore drill reference | \`runbook-restore-2026-05-13\` |"
assert_contains "$readiness_output" "| SMTP mode | \`log-only\` |"
assert_contains "$readiness_output" "| Cross-service broker mode | \`disabled\` |"
assert_contains "$readiness_output" "| \`health\` | \`PASS\` |"
assert_contains "$readiness_output" "| \`login\` | \`PASS\` |"
assert_contains "$readiness_output" "| \`provisioning_readiness\` | \`SKIP:not-readable-or-not-selected\` |"
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
