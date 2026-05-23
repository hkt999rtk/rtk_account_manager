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
root_domain="${GODADDY_ROOT_DOMAIN:-realtekconnect.com}"
ipv4="${ACCOUNT_MANAGER_LINODE_PUBLIC_IPV4:-${ACCOUNT_MANAGER_LINODE_HOST:-}}"
ttl="${GODADDY_RECORD_TTL:-600}"
api_base="${GODADDY_API_BASE:-https://api.godaddy.com/v1}"

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }
need curl
need jq
[ -n "${GODADDY_KEY:-}" ] || die "GODADDY_KEY is required"
[ -n "${GODADDY_SECRET:-}" ] || die "GODADDY_SECRET is required"
[ -n "$ipv4" ] || die "ACCOUNT_MANAGER_LINODE_PUBLIC_IPV4 or ACCOUNT_MANAGER_LINODE_HOST is required"

if [ "$domain" = "$root_domain" ]; then
  record_name="@"
else
  record_name="${domain%.$root_domain}"
  [ "$record_name" != "$domain" ] || die "domain $domain is not under root domain $root_domain"
fi
payload="$(jq -cn --arg data "$ipv4" --argjson ttl "$ttl" '[{data:$data, ttl:$ttl}]')"

curl -fsS -X PUT "$api_base/domains/$root_domain/records/A/$record_name" \
  -H "Authorization: sso-key $GODADDY_KEY:$GODADDY_SECRET" \
  -H 'Content-Type: application/json' \
  --data-binary "$payload" >/dev/null

printf 'Updated DNS: %s A %s\n' "$domain" "$ipv4"
