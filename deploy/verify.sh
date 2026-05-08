#!/usr/bin/env bash
set -euo pipefail

prefix=${PREFIX:-/opt/rtk-account-manager}
env_file=${ENV_FILE:-/etc/rtk-account-manager/account-manager.env}
base_url=${ACCOUNT_MANAGER_BASE_URL:-http://127.0.0.1:${PORT:-18081}}

if [ -f "$env_file" ]; then
  env_base_url=$(grep -E '^ACCOUNT_MANAGER_BASE_URL=' "$env_file" | tail -1 | cut -d= -f2- || true)
  env_port=$(grep -E '^PORT=' "$env_file" | tail -1 | cut -d= -f2- || true)
  if [ -n "$env_base_url" ]; then
    base_url=$env_base_url
  else
    base_url=${ACCOUNT_MANAGER_BASE_URL:-http://127.0.0.1:${env_port:-${PORT:-18081}}}
  fi
fi

echo "Verifying RTK Account Manager at $base_url"

for binary in \
  rtk-account-manager \
  rtk-account-manager-migrate \
  rtk-account-manager-outbox-worker \
  rtk-account-manager-inbox-worker \
  rtk-account-manager-cleanup-tokens; do
  test -x "$prefix/bin/$binary"
done

curl -fsS "$base_url/v1/health" >/dev/null

if command -v systemctl >/dev/null 2>&1; then
  systemctl is-active --quiet rtk-account-manager.service
  systemctl is-enabled --quiet rtk-account-manager-cleanup-tokens.timer
fi

echo "RTK Account Manager verification passed"
