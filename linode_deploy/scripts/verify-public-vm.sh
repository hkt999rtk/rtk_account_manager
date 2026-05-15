#!/usr/bin/env bash
set -euo pipefail

domain="${ACCOUNT_MANAGER_LINODE_DOMAIN:-account-manager.video-cloud-staging.realtekconnect.com}"
base_url="${ACCOUNT_MANAGER_BASE_URL:-}"
if [ -z "$base_url" ]; then
  if [ "${ACCOUNT_MANAGER_LINODE_HTTP_ONLY:-0}" = 1 ]; then
    base_url="http://$domain"
  else
    base_url="https://$domain"
  fi
fi
artifacts_dir="${ACCOUNT_MANAGER_LINODE_VERIFY_ARTIFACTS:-.artifacts/linode-account-manager-verify}"
email="${ACCOUNT_MANAGER_VERIFY_EMAIL:-verify+$(date -u +%Y%m%d%H%M%S)@example.invalid}"
password="${ACCOUNT_MANAGER_VERIFY_PASSWORD:-Verify-$(openssl rand -hex 12)aA1!}"
org="${ACCOUNT_MANAGER_VERIFY_ORG:-RTK Account Manager Linode Verify}"
command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }
command -v openssl >/dev/null 2>&1 || { echo "openssl is required" >&2; exit 1; }
mkdir -p "$artifacts_dir"

record() { printf '%s\n' "$1" | tee -a "$artifacts_dir/checks.txt" >/dev/null; }

curl -fsS "$base_url/v1/health" > "$artifacts_dir/health.json"
record "PASS health"

register_body="$(mktemp)"
trap 'rm -f "$register_body"' EXIT
jq -cn --arg email "$email" --arg password "$password" --arg org "$org" '{email:$email,password:$password,organization_name:$org}' > "$register_body"
status="$(curl -sS -o "$artifacts_dir/register.json" -w '%{http_code}' -H 'content-type: application/json' --data-binary "@$register_body" "$base_url/v1/auth/register")"
if [ "$status" != 201 ] && [ "$status" != 409 ]; then
  printf 'register returned HTTP %s\n' "$status" >&2
  cat "$artifacts_dir/register.json" >&2
  exit 1
fi
record "PASS register endpoint http_$status"

login_body="$(mktemp)"
trap 'rm -f "$register_body" "$login_body"' EXIT
jq -cn --arg email "$email" --arg password "$password" '{email:$email,password:$password}' > "$login_body"
status="$(curl -sS -o "$artifacts_dir/login.json" -w '%{http_code}' -H 'content-type: application/json' --data-binary "@$login_body" "$base_url/v1/auth/login")"
if [ "$status" = 200 ]; then
  token="$(jq -r '.tokens.access_token // .access_token // .accessToken // empty' "$artifacts_dir/login.json")"
  [ -n "$token" ] || { echo "login response did not include an access token" >&2; exit 1; }
  curl -fsS -H "authorization: Bearer $token" "$base_url/v1/me" > "$artifacts_dir/me.json"
  record "PASS login and me"
else
  printf 'login returned HTTP %s\n' "$status" >&2
  cat "$artifacts_dir/login.json" >&2
  exit 1
fi

printf 'Account Manager verify passed: %s\nArtifacts: %s\n' "$base_url" "$artifacts_dir"
