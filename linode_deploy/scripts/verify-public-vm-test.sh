#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

fake_bin="$tmpdir/bin"
artifacts="$tmpdir/artifacts"
mkdir -p "$fake_bin" "$artifacts"

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
out=""
write_code=""
method="GET"
data_seen=0
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o)
      out="$2"
      shift 2
      ;;
    -w)
      write_code="$2"
      shift 2
      ;;
    -H)
      shift 2
      ;;
    --data-binary|-d|--data)
      data_seen=1
      method="POST"
      shift 2
      ;;
    -X)
      method="$2"
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

body='{}'
code=200
case "$url" in
  */v1/health)
    body='{"status":"ok"}'
    ;;
  */v1/auth/register)
    code=201
    body='{"user":{"id":"user-1"}}'
    ;;
  */v1/auth/login)
    body='{"tokens":{"access_token":"token-for-test"}}'
    ;;
  */v1/me)
    body='{"user":{"id":"user-1"}}'
    ;;
  */v1/admin/brand-clouds)
    if [ "$method" = "POST" ] || [ "$data_seen" = 1 ]; then
      body='{"brand_cloud":{"id":"brand-1","name":"Realtek Connect+ Verify","organization_kind":"brand_cloud","status":"active"}}'
      code=201
    else
      body='{"brand_clouds":[{"id":"brand-1","name":"Realtek Connect+ Verify","organization_kind":"brand_cloud"}],"pagination":{"total":1}}'
    fi
    ;;
  */v1/admin/brand-clouds/brand-1)
    body='{"brand_cloud":{"id":"brand-1","name":"Realtek Connect+ Verify","organization_kind":"brand_cloud","status":"active"}}'
    ;;
  */v1/admin/audit-events\?subject_type=brand_cloud)
    body='{"audit_events":[{"event_type":"brand_cloud_created","subject_type":"brand_cloud"}]}'
    ;;
  *)
    printf 'unexpected curl url: %s\n' "$url" >&2
    code=404
    body='{"error":"unexpected"}'
    ;;
esac

if [ -n "$out" ]; then
  printf '%s' "$body" > "$out"
else
  printf '%s' "$body"
fi
if [ -n "$write_code" ]; then
  printf '%s' "$write_code" | sed "s/%{http_code}/$code/g"
fi
exit 0
FAKE_CURL
chmod +x "$fake_bin/curl"

PATH="$fake_bin:$PATH" \
ACCOUNT_MANAGER_BASE_URL="https://account-manager.example.test" \
ACCOUNT_MANAGER_LINODE_VERIFY_ARTIFACTS="$artifacts" \
ACCOUNT_MANAGER_VERIFY_PLATFORM_ADMIN_EMAIL="root@example.test" \
ACCOUNT_MANAGER_VERIFY_PLATFORM_ADMIN_PASSWORD="secret-password" \
ACCOUNT_MANAGER_VERIFY_BRAND_CLOUD=1 \
ACCOUNT_MANAGER_VERIFY_BRAND_CLOUD_NAME="Realtek Connect+ Verify" \
  "$repo_root/linode_deploy/scripts/verify-public-vm.sh" >/tmp/verify-public-vm-test.out

grep -q 'PASS brand-cloud create http_201' "$artifacts/checks.txt"
grep -q 'PASS brand-cloud list' "$artifacts/checks.txt"
grep -q 'PASS brand-cloud audit' "$artifacts/checks.txt"
test -f "$artifacts/create-brand-cloud.json"
test -f "$artifacts/list-brand-clouds.json"
test -f "$artifacts/audit-brand-cloud.json"
if find "$artifacts" -type f -name '*platform*login*' -print -quit | grep -q .; then
  echo 'platform-admin login artifacts must not be persisted' >&2
  exit 1
fi

printf 'verify-public-vm brand-cloud test passed\n'
