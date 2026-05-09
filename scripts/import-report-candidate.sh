#!/usr/bin/env bash
set -euo pipefail

TARGET_REPORT="${TARGET_REPORT:-${1:-}}"
IMPORT_DIR="${IMPORT_DIR:-.artifacts/imported}"

case "$TARGET_REPORT" in
	docs/TEST_REPORT.md | docs/RELEASE_TEST_REPORT.md | docs/READINESS_TEST_REPORT.md)
		;;
	*)
		echo "unsupported target report: $TARGET_REPORT" >&2
		exit 2
		;;
esac

if [ ! -d "$IMPORT_DIR" ]; then
	echo "import directory not found: $IMPORT_DIR" >&2
	exit 2
fi

candidate="$(find "$IMPORT_DIR" -type f -path "*/$TARGET_REPORT" | head -n 1)"
if [ -z "$candidate" ]; then
	echo "candidate for $TARGET_REPORT not found under $IMPORT_DIR" >&2
	exit 2
fi

"$(dirname "$0")/validate-report-candidate.sh" "$TARGET_REPORT" "$candidate"
mkdir -p "$(dirname "$TARGET_REPORT")"
cp "$candidate" "$TARGET_REPORT"
echo "imported $candidate to $TARGET_REPORT"
