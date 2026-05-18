#!/usr/bin/env bash
set -euo pipefail

version="${VERSION:-${1:-}}"
release_asset="${RELEASE_ASSET:-${2:-}}"
artifact_name="${ARTIFACT_NAME:-rtk_account_manager}"
repo="${GITHUB_REPOSITORY:-hkt999rtk/rtk_account_manager}"
source_commit="${SOURCE_COMMIT:-${GITHUB_SHA:-}}"
output_dir="${OUTPUT_DIR:-dist}"

if [ -z "$version" ]; then
	echo "VERSION or first argument is required" >&2
	exit 2
fi
if [ -z "$release_asset" ] || [ ! -f "$release_asset" ]; then
	echo "RELEASE_ASSET or second argument must point to an existing release tarball" >&2
	exit 2
fi
if [ -z "$source_commit" ]; then
	source_commit="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
fi

case "$version" in
	*[!A-Za-z0-9._-]* | "" | .* | *..*)
		echo "invalid version: use only letters, digits, dot, underscore, and dash" >&2
		exit 2
		;;
esac

mkdir -p "$output_dir"
object_bundle="$output_dir/$version.tar.gz"
checksum_file="$object_bundle.sha256"
manifest_file="$output_dir/manifest.json"
artifact_path="releases/$artifact_name-$version/$version.tar.gz"

cp "$release_asset" "$object_bundle"
if command -v sha256sum >/dev/null 2>&1; then
	sha256="$(sha256sum "$object_bundle" | awk '{print $1}')"
else
	sha256="$(shasum -a 256 "$object_bundle" | awk '{print $1}')"
fi
printf '%s  %s\n' "$sha256" "$(basename "$object_bundle")" >"$checksum_file"

VERSION="$version" \
ARTIFACT_NAME="$artifact_name" \
GITHUB_REPOSITORY="$repo" \
SOURCE_COMMIT="$source_commit" \
OBJECT_SHA256="$sha256" \
python3 - "$manifest_file" <<'PY'
import json
import os
import sys
from datetime import datetime, timezone

manifest_file = sys.argv[1]
version = os.environ["VERSION"]
artifact_name = os.environ.get("ARTIFACT_NAME", "rtk_account_manager")
bundle = f"{version}.tar.gz"
artifact_path = f"releases/{artifact_name}-{version}/{bundle}"

data = {
    "repo": os.environ.get("GITHUB_REPOSITORY", "hkt999rtk/rtk_account_manager"),
    "artifact_name": artifact_name,
    "version": version,
    "source_commit": os.environ.get("SOURCE_COMMIT") or os.environ.get("GITHUB_SHA") or "unknown",
    "bundle": bundle,
    "artifact_path": artifact_path,
    "sha256": os.environ["OBJECT_SHA256"],
    "created_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
}
with open(manifest_file, "w", encoding="utf-8") as f:
    json.dump(data, f, indent=2, sort_keys=True)
    f.write("\n")
PY

cat <<EOF
object_bundle=$object_bundle
checksum_file=$checksum_file
manifest_file=$manifest_file
artifact_path=$artifact_path
sha256=$sha256
EOF
