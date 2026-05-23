#!/usr/bin/env bash
set -euo pipefail

load_secret_env() {
  local file="$1"
  if [ -f "$file" ]; then
    set -a
    # shellcheck disable=SC1090
    . "$file"
    set +a
  fi
}

if [ -n "${DEPLOY_SECRETS_DIR:-}" ]; then
  [ -d "$DEPLOY_SECRETS_DIR" ] || { printf 'error: DEPLOY_SECRETS_DIR not found: %s\n' "$DEPLOY_SECRETS_DIR" >&2; exit 1; }
  load_secret_env "$DEPLOY_SECRETS_DIR/env/account-manager-public-staging.env"
  load_secret_env "$DEPLOY_SECRETS_DIR/state/${ACCOUNT_MANAGER_LINODE_LABEL:-rtk-account-manager-staging}.env"
fi

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
verify_brand_cloud="${ACCOUNT_MANAGER_VERIFY_BRAND_CLOUD:-0}"
platform_admin_email="${ACCOUNT_MANAGER_VERIFY_PLATFORM_ADMIN_EMAIL:-${ACCOUNT_MANAGER_BOOTSTRAP_PLATFORM_ADMIN_EMAIL:-}}"
platform_admin_password="${ACCOUNT_MANAGER_VERIFY_PLATFORM_ADMIN_PASSWORD:-${ACCOUNT_MANAGER_BOOTSTRAP_PLATFORM_ADMIN_PASSWORD:-}}"
brand_cloud_name="${ACCOUNT_MANAGER_VERIFY_BRAND_CLOUD_NAME:-Realtek Connect+ Verify $(date -u +%Y%m%d%H%M%S)}"
command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }
command -v openssl >/dev/null 2>&1 || { echo "openssl is required" >&2; exit 1; }
mkdir -p "$artifacts_dir"

record() { printf '%s\n' "$1" | tee -a "$artifacts_dir/checks.txt" >/dev/null; }
cleanup_files=()
cleanup() {
  if [ "${#cleanup_files[@]}" -gt 0 ]; then
    rm -f "${cleanup_files[@]}"
  fi
}
trap cleanup EXIT

curl -fsS "$base_url/v1/health" > "$artifacts_dir/health.json"
record "PASS health"

register_body="$(mktemp)"
cleanup_files+=("$register_body")
jq -cn --arg email "$email" --arg password "$password" --arg org "$org" '{email:$email,password:$password,organization_name:$org}' > "$register_body"
status="$(curl -sS -o "$artifacts_dir/register.json" -w '%{http_code}' -H 'content-type: application/json' --data-binary "@$register_body" "$base_url/v1/auth/register")"
if [ "$status" != 201 ] && [ "$status" != 409 ]; then
  printf 'register returned HTTP %s\n' "$status" >&2
  cat "$artifacts_dir/register.json" >&2
  exit 1
fi
record "PASS register endpoint http_$status"

login_body="$(mktemp)"
cleanup_files+=("$login_body")
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

if [ "$verify_brand_cloud" = 1 ]; then
  if [ -z "$platform_admin_email" ] || [ -z "$platform_admin_password" ]; then
    echo "ACCOUNT_MANAGER_VERIFY_PLATFORM_ADMIN_EMAIL/PASSWORD or bootstrap platform-admin credentials are required for brand-cloud verification" >&2
    exit 1
  fi

  platform_login_body="$(mktemp)"
  platform_login_response="$(mktemp)"
  cleanup_files+=("$platform_login_body" "$platform_login_response")
  jq -cn --arg email "$platform_admin_email" --arg password "$platform_admin_password" '{email:$email,password:$password}' > "$platform_login_body"
  status="$(curl -sS -o "$platform_login_response" -w '%{http_code}' -H 'content-type: application/json' --data-binary "@$platform_login_body" "$base_url/v1/auth/login")"
  if [ "$status" != 200 ]; then
    printf 'platform-admin login returned HTTP %s\n' "$status" >&2
    jq '{error}' "$platform_login_response" >&2 || true
    exit 1
  fi
  platform_token="$(jq -r '.tokens.access_token // .access_token // .accessToken // empty' "$platform_login_response")"
  [ -n "$platform_token" ] || { echo "platform-admin login response did not include an access token" >&2; exit 1; }

  brand_body="$(mktemp)"
  cleanup_files+=("$brand_body")
  jq -cn --arg name "$brand_cloud_name" '{name:$name,metadata:{verification:"linode-public-vm"}}' > "$brand_body"
  status="$(curl -sS -o "$artifacts_dir/create-brand-cloud.json" -w '%{http_code}' -H "authorization: Bearer $platform_token" -H 'content-type: application/json' --data-binary "@$brand_body" "$base_url/v1/admin/brand-clouds")"
  if [ "$status" != 201 ]; then
    printf 'brand-cloud create returned HTTP %s\n' "$status" >&2
    cat "$artifacts_dir/create-brand-cloud.json" >&2
    exit 1
  fi
  record "PASS brand-cloud create http_$status"

  brand_cloud_id="$(jq -r '.brand_cloud.id // empty' "$artifacts_dir/create-brand-cloud.json")"
  [ -n "$brand_cloud_id" ] || { echo "brand-cloud create response did not include an id" >&2; exit 1; }
  curl -fsS -H "authorization: Bearer $platform_token" "$base_url/v1/admin/brand-clouds" > "$artifacts_dir/list-brand-clouds.json"
  record "PASS brand-cloud list"
  curl -fsS -H "authorization: Bearer $platform_token" "$base_url/v1/admin/brand-clouds/$brand_cloud_id" > "$artifacts_dir/get-brand-cloud.json"
  record "PASS brand-cloud get"
  curl -fsS -H "authorization: Bearer $platform_token" "$base_url/v1/admin/audit-events?subject_type=brand_cloud" > "$artifacts_dir/audit-brand-cloud.json"
  record "PASS brand-cloud audit"
fi

printf 'Account Manager verify passed: %s\nArtifacts: %s\n' "$base_url" "$artifacts_dir"
