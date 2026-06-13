#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

fake_bin="$tmpdir/bin"
capture="$tmpdir/capture"
mkdir -p "$fake_bin" "$capture"

cat > "$fake_bin/openssl" <<'FAKE_OPENSSL'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = "rand" ]; then
  printf 'abcdef1234567890abcdef12\n'
  exit 0
fi
exec /usr/bin/openssl "$@"
FAKE_OPENSSL
chmod +x "$fake_bin/openssl"

cat > "$fake_bin/curl" <<'FAKE_CURL'
#!/usr/bin/env bash
set -euo pipefail
method="GET"
data=""
url=""
output=""
write_out=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -X)
      method="$2"
      shift 2
      ;;
    -H)
      shift 2
      ;;
    --data-binary|-d|--data)
      data="$2"
      shift 2
      ;;
    -o)
      output="$2"
      shift 2
      ;;
    -w)
      write_out="$2"
      shift 2
      ;;
    -*)
      shift
      ;;
    *)
      url="$1"
      shift
      ;;
  esac
done

mkdir -p "$RTK_TEST_CAPTURE_DIR"
body=""
status="200"
case "$url" in
  */linode/instances?page_size=500)
    body='{"data":[]}'
    ;;
  */networking/firewalls?page_size=500)
    body='{"data":[]}'
    ;;
  */linode/instances)
    printf '%s' "$data" > "$RTK_TEST_CAPTURE_DIR/linode-create.json"
    body='{"id":12345,"ipv4":["203.0.113.20"]}'
    ;;
  */networking/firewalls)
    printf '%s' "$data" > "$RTK_TEST_CAPTURE_DIR/firewall-create.json"
    body='{"id":67890}'
    ;;
  */networking/firewalls/67890/devices)
    printf '%s' "$data" > "$RTK_TEST_CAPTURE_DIR/firewall-device.json"
    body='{"id":67890}'
    ;;
  *)
    printf 'unexpected curl url: %s\n' "$url" >&2
    exit 22
    ;;
esac
if [ -n "$output" ]; then
  printf '%s' "$body" > "$output"
else
  printf '%s' "$body"
fi
if [ -n "$write_out" ]; then
  printf '%s' "${write_out//\%\{http_code\}/$status}"
fi
FAKE_CURL
chmod +x "$fake_bin/curl"

state="$tmpdir/account-manager.env"
PATH="$fake_bin:$PATH" \
RTK_TEST_CAPTURE_DIR="$capture" \
LINODE_TOKEN="token-for-test" \
ACCOUNT_MANAGER_LINODE_LABEL="rtk-account-manager-test" \
ACCOUNT_MANAGER_LINODE_FIREWALL_LABEL="rtk-account-manager-test-fw" \
ACCOUNT_MANAGER_LINODE_PUBLIC_KEY_PATH="$tmpdir/id_ed25519.pub" \
ACCOUNT_MANAGER_LINODE_ALLOWED_SSH_CIDRS="203.0.113.10/32" \
ACCOUNT_MANAGER_LINODE_STATE_PATH="$state" \
ACCOUNT_MANAGER_LINODE_VPC_SUBNET_ID="710365" \
ACCOUNT_MANAGER_LINODE_PRIVATE_IPV4="10.42.1.50" \
  bash -c 'printf "ssh-ed25519 test-key\n" > "$ACCOUNT_MANAGER_LINODE_PUBLIC_KEY_PATH"; "$0"' \
  "$repo_root/linode_deploy/scripts/provision-public-vm.sh" >/tmp/provision-public-vm-test.out

jq -e '
  .interfaces[0].purpose == "public" and
  .interfaces[0].primary == true and
  .interfaces[1].purpose == "vpc" and
  .interfaces[1].subnet_id == 710365 and
  .interfaces[1].ipv4.vpc == "10.42.1.50"
' "$capture/linode-create.json" >/dev/null

grep -q '^ACCOUNT_MANAGER_LINODE_PRIVATE_IPV4=10.42.1.50$' "$state"

jq -e '
  ([.rules.inbound[] | select((.ports | split(",")) | index("80"))] | length) == 0 and
  ([.rules.inbound[] | select(.label == "https" and .protocol == "TCP" and .ports == "443")] | length) == 1
' "$capture/firewall-create.json" >/dev/null

printf 'provision-public-vm VPC test passed\n'
