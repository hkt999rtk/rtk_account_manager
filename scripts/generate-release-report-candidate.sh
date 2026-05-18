#!/usr/bin/env bash
set -euo pipefail

OUTPUT="${OUTPUT:-.artifacts/report-candidates/docs/RELEASE_TEST_REPORT.md}"
VERSION="${VERSION:-}"
SOURCE_COMMIT="${SOURCE_COMMIT:-${GITHUB_SHA:-}}"
RELEASE_ASSET="${RELEASE_ASSET:-}"
RUN_URL="${RUN_URL:-}"
CONTRACTS_COMMIT="${CONTRACTS_COMMIT:-}"
OBJECT_STORAGE_BUNDLE_KEY="${OBJECT_STORAGE_BUNDLE_KEY:-}"
OBJECT_STORAGE_CHECKSUM_KEY="${OBJECT_STORAGE_CHECKSUM_KEY:-}"
OBJECT_STORAGE_MANIFEST_KEY="${OBJECT_STORAGE_MANIFEST_KEY:-}"
GENERATED_AT="${REPORT_GENERATED_AT:-$(date -u +"%Y-%m-%dT%H:%M:%SZ")}"

if [ -z "$VERSION" ]; then
	echo "VERSION is required" >&2
	exit 2
fi

if [ -z "$SOURCE_COMMIT" ]; then
	SOURCE_COMMIT="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
fi

if [ -z "$RELEASE_ASSET" ] || [ ! -f "$RELEASE_ASSET" ]; then
	echo "RELEASE_ASSET must point to an existing release tarball" >&2
	exit 2
fi

if [ -z "$RUN_URL" ] && [ -n "${GITHUB_SERVER_URL:-}" ] && [ -n "${GITHUB_REPOSITORY:-}" ] && [ -n "${GITHUB_RUN_ID:-}" ]; then
	RUN_URL="${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}"
fi
if [ -z "$RUN_URL" ]; then
	RUN_URL="not available"
fi

if [ -z "$CONTRACTS_COMMIT" ] && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	CONTRACTS_COMMIT="$(git ls-tree HEAD contracts 2>/dev/null | awk '{print $3}' || true)"
fi
if [ -z "$CONTRACTS_COMMIT" ]; then
	CONTRACTS_COMMIT="not present"
fi

asset_name="$(basename "$RELEASE_ASSET")"
if command -v sha256sum >/dev/null 2>&1; then
	asset_sha256="$(sha256sum "$RELEASE_ASSET" | awk '{print $1}')"
else
	asset_sha256="$(shasum -a 256 "$RELEASE_ASSET" | awk '{print $1}')"
fi

mkdir -p "$(dirname "$OUTPUT")"
cat >"$OUTPUT" <<EOF
# Release Test Report

Generated: $GENERATED_AT

## Summary

| Field | Value |
| --- | --- |
| Release version | \`$VERSION\` |
| Source commit | \`$SOURCE_COMMIT\` |
| Release asset | \`$asset_name\` |
| Release asset SHA256 | \`$asset_sha256\` |
| Object Storage SHA256 | \`$asset_sha256\` |
| Object Storage bundle key | \`${OBJECT_STORAGE_BUNDLE_KEY:-not published}\` |
| Object Storage checksum key | \`${OBJECT_STORAGE_CHECKSUM_KEY:-not published}\` |
| Object Storage manifest key | \`${OBJECT_STORAGE_MANIFEST_KEY:-not published}\` |
| Contracts commit | \`$CONTRACTS_COMMIT\` |
| Workflow run | $RUN_URL |

## Candidate Policy

This sanitized release report is generated as a workflow artifact. It is not committed automatically. Import it into a pull request branch only through the report import workflow after reviewing the artifact.
EOF

"$(dirname "$0")/validate-report-candidate.sh" docs/RELEASE_TEST_REPORT.md "$OUTPUT" >/dev/null
echo "generated release report candidate: $OUTPUT"
