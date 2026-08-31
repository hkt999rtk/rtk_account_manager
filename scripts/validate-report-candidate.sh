#!/usr/bin/env bash
set -euo pipefail

target=${1:-}
candidate=${2:-}

case "$target" in
  docs/test_report.md)
    expected_heading="# Test Report"
    ;;
  docs/release_test_report.md)
    expected_heading="# Release Test Report"
    ;;
  docs/readiness_test_report.md)
    expected_heading="# Readiness Test Report"
    ;;
  *)
    echo "unsupported target report: $target" >&2
    exit 2
    ;;
esac

if [ -z "$candidate" ] || [ ! -f "$candidate" ]; then
  echo "candidate report not found: $candidate" >&2
  exit 2
fi

first_line=$(sed -n '1p' "$candidate")
if [ "$first_line" != "$expected_heading" ]; then
  echo "invalid report heading: expected '$expected_heading', got '$first_line'" >&2
  exit 1
fi

if grep -E -- '-----BEGIN [A-Z ]*PRIVATE KEY-----' "$candidate" >/dev/null; then
  echo "candidate contains private key material" >&2
  exit 1
fi

if grep -E 'eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+' "$candidate" >/dev/null; then
  echo "candidate contains JWT-looking token material" >&2
  exit 1
fi

if grep -E '[a-z][a-z0-9+.-]*://[^[:space:]/:@]+:[^[:space:]@]+@' "$candidate" \
  | grep -Ev '<redacted>|%5Bredacted%5D|\\[redacted\\]' >/dev/null; then
  echo "candidate contains unredacted credential-bearing URL" >&2
  exit 1
fi

if awk '
  BEGIN { bad = 0 }
  /^[A-Za-z_][A-Za-z0-9_]*(SECRET|TOKEN|PASSWORD)[A-Za-z0-9_]*=/ {
    if ($0 !~ /=<redacted>$/ && $0 !~ /=$/) {
      print $0
      bad = 1
    }
  }
  END { exit bad }
' "$candidate" >/dev/null; then
  :
else
  echo "candidate contains unredacted secret-like environment assignment" >&2
  exit 1
fi

echo "report candidate is valid for $target: $candidate"
