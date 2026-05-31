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
fi

label="${ACCOUNT_MANAGER_LINODE_LABEL:-rtk-account-manager-staging}"
region="${ACCOUNT_MANAGER_LINODE_REGION:-us-sea}"
type="${ACCOUNT_MANAGER_LINODE_TYPE:-g6-standard-2}"
image="${ACCOUNT_MANAGER_LINODE_IMAGE:-linode/ubuntu24.04}"
public_key_path="${ACCOUNT_MANAGER_LINODE_PUBLIC_KEY_PATH:-$HOME/.ssh/id_ed25519_rtkcloud.pub}"
allowed_ssh_cidrs="${ACCOUNT_MANAGER_LINODE_ALLOWED_SSH_CIDRS:-}"
firewall_label="${ACCOUNT_MANAGER_LINODE_FIREWALL_LABEL:-${label}-fw}"
vpc_subnet_id="${ACCOUNT_MANAGER_LINODE_VPC_SUBNET_ID:-}"
private_ipv4="${ACCOUNT_MANAGER_LINODE_PRIVATE_IPV4:-}"
vpc_cidr="${ACCOUNT_MANAGER_LINODE_VPC_CIDR:-10.42.1.0/24}"
default_state_path="${DEPLOY_SECRETS_DIR:+$DEPLOY_SECRETS_DIR/state/${label}.env}"
state_path="${ACCOUNT_MANAGER_LINODE_STATE_PATH:-${default_state_path:-linode_deploy/state/${label}.env}}"
api_base="${LINODE_API_BASE:-https://api.linode.com/v4}"

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }
api() {
  local method="$1" path="$2" data="${3:-}"
  if [ -n "$data" ]; then
    curl -fsS -X "$method" "$api_base$path" \
      -H "Authorization: Bearer $LINODE_TOKEN" \
      -H 'Content-Type: application/json' \
      --data-binary "$data"
  else
    curl -fsS -X "$method" "$api_base$path" \
      -H "Authorization: Bearer $LINODE_TOKEN" \
      -H 'Content-Type: application/json'
  fi
}

need curl
need jq
need openssl
[ -n "${LINODE_TOKEN:-}" ] || die "LINODE_TOKEN is required"
[ -s "$public_key_path" ] || die "public key not found: $public_key_path"
[ -n "$allowed_ssh_cidrs" ] || die "ACCOUNT_MANAGER_LINODE_ALLOWED_SSH_CIDRS must be set"

if api GET "/linode/instances?page_size=500" | jq -e --arg label "$label" '.data[] | select(.label == $label)' >/dev/null; then
  die "Linode label already exists: $label"
fi
if api GET "/networking/firewalls?page_size=500" | jq -e --arg label "$firewall_label" '.data[] | select(.label == $label)' >/dev/null; then
  die "Firewall label already exists: $firewall_label"
fi

root_pass="$(openssl rand -base64 36)"
ssh_key="$(cat "$public_key_path")"
create_payload="$(jq -cn \
  --arg label "$label" \
  --arg region "$region" \
  --arg type "$type" \
  --arg image "$image" \
  --arg root_pass "$root_pass" \
  --arg ssh_key "$ssh_key" \
  --arg subnet_id "$vpc_subnet_id" \
  --arg private_ip "$private_ipv4" \
  '{
    label:$label,
    region:$region,
    type:$type,
    image:$image,
    root_pass:$root_pass,
    authorized_keys:[$ssh_key],
    tags:["rtk-account-manager","account-manager-staging"]
  }
  | if ($subnet_id != "" and $private_ip != "") then
      .interfaces = [
        {purpose:"public", primary:true},
        {purpose:"vpc", subnet_id:($subnet_id|tonumber), ipv4:{vpc:$private_ip}}
      ]
    else . end')"

if [ -n "$vpc_subnet_id" ] && [ -n "$private_ipv4" ]; then
  printf '[account-manager-linode] creating public+vpc Linode %s in %s private_ip=%s\n' "$label" "$region" "$private_ipv4" >&2
else
  printf '[account-manager-linode] creating public-only Linode %s in %s\n' "$label" "$region" >&2
fi
create_json="$(api POST /linode/instances "$create_payload")"
linode_id="$(printf '%s' "$create_json" | jq -r '.id')"
public_ipv4="$(printf '%s' "$create_json" | jq -r '.ipv4[0]')"
[ -n "$linode_id" ] && [ "$linode_id" != null ] || die "failed to read Linode id"
[ -n "$public_ipv4" ] && [ "$public_ipv4" != null ] || die "failed to read Linode public IPv4"

firewall_payload="$(jq -cn --arg label "$firewall_label" --arg cidrs "$allowed_ssh_cidrs" --arg vpc_cidr "$vpc_cidr" '{
  label: $label,
  rules: {
    inbound_policy: "DROP",
    outbound_policy: "ACCEPT",
    inbound: [
      {label:"ssh", action:"ACCEPT", protocol:"TCP", ports:"22", addresses:{ipv4:($cidrs|split(","))}},
      {label:"http", action:"ACCEPT", protocol:"TCP", ports:"80", addresses:{ipv4:["0.0.0.0/0"], ipv6:["::/0"]}},
      {label:"https", action:"ACCEPT", protocol:"TCP", ports:"443", addresses:{ipv4:["0.0.0.0/0"], ipv6:["::/0"]}},
      {label:"vpc-private-tcp", action:"ACCEPT", protocol:"TCP", ports:"18081", addresses:{ipv4:[$vpc_cidr]}}
    ],
    outbound: []
  }
}')"

printf '[account-manager-linode] creating firewall %s\n' "$firewall_label" >&2
firewall_json="$(api POST /networking/firewalls "$firewall_payload")"
firewall_id="$(printf '%s' "$firewall_json" | jq -r '.id')"
[ -n "$firewall_id" ] && [ "$firewall_id" != null ] || die "failed to read firewall id"
api POST "/networking/firewalls/$firewall_id/devices" "$(jq -cn --argjson id "$linode_id" '{id:$id,type:"linode"}')" >/dev/null

mkdir -p "$(dirname "$state_path")"
cat > "$state_path" <<STATE
ACCOUNT_MANAGER_LINODE_ID=$linode_id
ACCOUNT_MANAGER_LINODE_LABEL=$label
ACCOUNT_MANAGER_LINODE_PUBLIC_IPV4=$public_ipv4
ACCOUNT_MANAGER_LINODE_HOST=$public_ipv4
ACCOUNT_MANAGER_LINODE_PRIVATE_IPV4=$private_ipv4
ACCOUNT_MANAGER_LINODE_FIREWALL_ID=$firewall_id
ACCOUNT_MANAGER_LINODE_FIREWALL_LABEL=$firewall_label
STATE
chmod 0600 "$state_path"

cat <<EOF_OUT
Linode created.

State file: $state_path
Public IPv4: $public_ipv4

Next DNS step:
  ACCOUNT_MANAGER_LINODE_DOMAIN=${ACCOUNT_MANAGER_LINODE_DOMAIN:-account-manager.video-cloud-staging.realtekconnect.com} \\
  ACCOUNT_MANAGER_LINODE_PUBLIC_IPV4=$public_ipv4 \\
  linode_deploy/scripts/set-godaddy-dns.sh
EOF_OUT
