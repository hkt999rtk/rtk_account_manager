#!/usr/bin/env bash
set -euo pipefail

OUTPUT="${OUTPUT:-.artifacts/report-candidates/docs/READINESS_TEST_REPORT.md}"
EVIDENCE_DIR="${EVIDENCE_DIR:-deployment-evidence}"
VERSION="${VERSION:-}"
VERIFY_RESULT="${VERIFY_RESULT:-unknown}"
RUN_URL="${RUN_URL:-}"
GENERATED_AT="${REPORT_GENERATED_AT:-$(date -u +"%Y-%m-%dT%H:%M:%SZ")}"

if [ -z "$RUN_URL" ] && [ -n "${GITHUB_SERVER_URL:-}" ] && [ -n "${GITHUB_REPOSITORY:-}" ] && [ -n "${GITHUB_RUN_ID:-}" ]; then
	RUN_URL="${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}"
fi
if [ -z "$RUN_URL" ]; then
	RUN_URL="not available"
fi

if [ -z "$VERSION" ] && [ -f "$EVIDENCE_DIR/summary.txt" ]; then
	VERSION="$(awk -F= '$1 == "version" { print $2; exit }' "$EVIDENCE_DIR/summary.txt")"
fi
if [ -z "$VERSION" ]; then
	VERSION="unknown"
fi

extract_active_line() {
	local file=$1
	if [ -f "$file" ]; then
		grep -m1 'Active:' "$file" | sed -E 's/^[[:space:]]+//' || true
	fi
}

backup_marker_status="unknown"
if [ -f "$EVIDENCE_DIR/backup-marker-status.txt" ]; then
	backup_marker_status="$(tr '\n' ' ' <"$EVIDENCE_DIR/backup-marker-status.txt" | sed -E 's/[[:space:]]+/ /g; s/^ //; s/ $//')"
fi

api_status="$(extract_active_line "$EVIDENCE_DIR/api-status.txt")"
cleanup_status="$(extract_active_line "$EVIDENCE_DIR/cleanup-timer-status.txt")"
migrate_status="$(extract_active_line "$EVIDENCE_DIR/migrate-status.txt")"
api_status="${api_status:-not collected}"
cleanup_status="${cleanup_status:-not collected}"
migrate_status="${migrate_status:-not collected}"

mkdir -p "$(dirname "$OUTPUT")"
cat >"$OUTPUT" <<EOF
# Readiness Test Report

Generated: $GENERATED_AT

## Summary

| Field | Value |
| --- | --- |
| Deployed version | \`$VERSION\` |
| Verify result | \`$VERIFY_RESULT\` |
| Backup marker status | \`$backup_marker_status\` |
| Workflow run | $RUN_URL |

## Service Status Summary

| Unit | Summary |
| --- | --- |
| rtk-account-manager.service | \`$api_status\` |
| rtk-account-manager-cleanup-tokens.timer | \`$cleanup_status\` |
| rtk-account-manager-migrate.service | \`$migrate_status\` |

## Redacted Environment Keys

EOF

if [ -s "$EVIDENCE_DIR/redacted-env-keys.txt" ]; then
	sed -E 's/=.*/=<redacted>/' "$EVIDENCE_DIR/redacted-env-keys.txt" \
		| awk '/^[A-Za-z_][A-Za-z0-9_]*=<redacted>$/ { printf "- `%s`\n", $0 }' >>"$OUTPUT"
else
	echo "No redacted environment key inventory was collected." >>"$OUTPUT"
fi

cat >>"$OUTPUT" <<'EOF'

## Candidate Policy

This sanitized readiness report contains only concise deployment evidence. Full systemd output, readiness JSON, logs, and diagnostics remain artifact-only.
EOF

"$(dirname "$0")/validate-report-candidate.sh" docs/READINESS_TEST_REPORT.md "$OUTPUT" >/dev/null
echo "generated readiness report candidate: $OUTPUT"
