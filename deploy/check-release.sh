#!/usr/bin/env bash
set -euo pipefail

release_dir=${1:-}
if [ -z "$release_dir" ]; then
  echo "usage: $0 <release-dir>" >&2
  exit 2
fi

if [ ! -d "$release_dir" ]; then
  echo "release directory not found: $release_dir" >&2
  exit 1
fi

required_executables=(
  bin/rtk-account-manager
  bin/rtk-account-manager-migrate
  bin/rtk-account-manager-outbox-worker
  bin/rtk-account-manager-inbox-worker
  bin/rtk-account-manager-cleanup-tokens
  deploy/install.sh
  deploy/verify.sh
)

for path in "${required_executables[@]}"; do
  if [ ! -x "$release_dir/$path" ]; then
    echo "missing executable: $path" >&2
    exit 1
  fi
done

required_files=(
  deploy/account-manager.env.example
  deploy/systemd/rtk-account-manager.service
  deploy/systemd/rtk-account-manager-migrate.service
  deploy/systemd/rtk-account-manager-outbox-worker.service
  deploy/systemd/rtk-account-manager-inbox-worker.service
  deploy/systemd/rtk-account-manager-cleanup-tokens.service
  deploy/systemd/rtk-account-manager-cleanup-tokens.timer
  release-manifest.txt
)

for path in "${required_files[@]}"; do
  if [ ! -f "$release_dir/$path" ]; then
    echo "missing release file: $path" >&2
    exit 1
  fi
done

if ! find "$release_dir/migrations" -maxdepth 1 -type f -name '*.sql' | grep -q .; then
  echo "release has no SQL migrations" >&2
  exit 1
fi

if grep -R '<secret-from-secret-manager>\|<user>:<password>' "$release_dir" \
  --exclude='account-manager.env.example' >/dev/null; then
  echo "placeholder secret text leaked outside env example" >&2
  exit 1
fi

echo "release bundle is valid: $release_dir"
