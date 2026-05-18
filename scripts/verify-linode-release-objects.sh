#!/usr/bin/env bash
set -euo pipefail

version="${1:-}"
download_dir="${2:-}"
artifact_name="${ARTIFACT_NAME:-rtk_account_manager}"

if [ -z "$version" ] || [ -z "$download_dir" ]; then
	echo "usage: $0 <version> <download-dir>" >&2
	exit 2
fi

bundle="$download_dir/$version.tar.gz"
checksum="$download_dir/$version.tar.gz.sha256"
manifest="$download_dir/manifest.json"

for path in "$bundle" "$checksum" "$manifest"; do
	if [ ! -f "$path" ]; then
		echo "missing release object: $path" >&2
		exit 1
	fi
done

python3 - "$version" "$artifact_name" "$bundle" "$checksum" "$manifest" <<'PY'
import hashlib
import json
import pathlib
import sys

version, artifact_name, bundle_path, checksum_path, manifest_path = sys.argv[1:]
bundle = pathlib.Path(bundle_path)
checksum_text = pathlib.Path(checksum_path).read_text(encoding="utf-8").strip()
manifest = json.loads(pathlib.Path(manifest_path).read_text(encoding="utf-8"))

expected_bundle = f"{version}.tar.gz"
expected_path = f"releases/{artifact_name}-{version}/{expected_bundle}"
actual_sha = hashlib.sha256(bundle.read_bytes()).hexdigest()
published_sha = checksum_text.split()[0]

checks = {
    "repo": manifest.get("repo") == "hkt999rtk/rtk_account_manager",
    "artifact_name": manifest.get("artifact_name") == artifact_name,
    "version": manifest.get("version") == version,
    "bundle": manifest.get("bundle") == expected_bundle,
    "artifact_path": manifest.get("artifact_path") == expected_path,
    "source_commit": bool(manifest.get("source_commit")),
    "sha256": manifest.get("sha256") == actual_sha == published_sha,
    "created_at": bool(manifest.get("created_at")),
}
failed = [name for name, ok in checks.items() if not ok]
if failed:
    raise SystemExit("release object verification failed: " + ", ".join(failed))
PY

extract_dir="$(mktemp -d)"
trap 'rm -rf "$extract_dir"' EXIT
tar -xzf "$bundle" -C "$extract_dir"
release_dir="$(find "$extract_dir" -mindepth 1 -maxdepth 1 -type d | head -1)"
if [ -z "$release_dir" ]; then
	echo "release bundle did not contain a top-level release directory" >&2
	exit 1
fi
"$(dirname "$0")/../deploy/check-release.sh" "$release_dir" >/dev/null

echo "release objects verified: $version"
