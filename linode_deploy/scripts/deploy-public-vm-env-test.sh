#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

fake_bin="$tmpdir/bin"
capture="$tmpdir/capture"
mkdir -p "$fake_bin" "$capture" "$tmpdir/release"

cat > "$fake_bin/ssh" <<'FAKE_SSH'
#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$RTK_TEST_CAPTURE_DIR"
if [ "$#" -gt 0 ] && [ "${*: -1}" = "mkdir -p /tmp/rtk-account-manager-deploy" ]; then
  exit 0
fi
if printf '%s\n' "$*" | grep -q 'bash -s'; then
  cat > "$RTK_TEST_CAPTURE_DIR/remote-install.sh"
  printf '%s\n' "$*" > "$RTK_TEST_CAPTURE_DIR/remote-install-args.txt"
  exit 0
fi
exit 0
FAKE_SSH
chmod +x "$fake_bin/ssh"

cat > "$fake_bin/scp" <<'FAKE_SCP'
#!/usr/bin/env bash
set -euo pipefail
exit 0
FAKE_SCP
chmod +x "$fake_bin/scp"

cat > "$fake_bin/go" <<'FAKE_GO'
#!/usr/bin/env bash
set -euo pipefail
exit 0
FAKE_GO
chmod +x "$fake_bin/go"

release_bundle="$tmpdir/release/test.tar.gz"
printf 'fake release\n' > "$release_bundle"
ssh_key="$tmpdir/id_ed25519"
printf 'fake key\n' > "$ssh_key"

PATH="$fake_bin:$PATH" \
RTK_TEST_CAPTURE_DIR="$capture" \
ACCOUNT_MANAGER_LINODE_DOMAIN="account.example.test" \
ACCOUNT_MANAGER_LINODE_CERTBOT_EMAIL="admin@example.test" \
ACCOUNT_MANAGER_LINODE_HOST="203.0.113.20" \
ACCOUNT_MANAGER_LINODE_PRIVATE_IPV4="10.42.1.50" \
ACCOUNT_MANAGER_LINODE_SSH_KEY="$ssh_key" \
ACCOUNT_MANAGER_LINODE_RELEASE="test" \
ACCOUNT_MANAGER_LINODE_RELEASE_BUNDLE="$release_bundle" \
JWT_ACCESS_SECRET="access-secret" \
JWT_REFRESH_SECRET="refresh-secret" \
GODADDY_KEY="fake-godaddy-key" \
GODADDY_SECRET="fake-godaddy-secret" \
CLOUD_DNS_ROOT_DOMAIN="example.test" \
  "$repo_root/linode_deploy/scripts/deploy-public-vm.sh" >/tmp/deploy-public-vm-env-test.out

grep -q -- '--manual-auth-hook /usr/local/libexec/rtk-cloud-certbot-dns-auth' "$capture/remote-install.sh"
! grep -q 'certbot --nginx' "$capture/remote-install.sh"
! grep -q 'listen 80' "$capture/remote-install.sh"
! grep -q '/.well-known/acme-challenge' "$capture/remote-install.sh"

printf 'deploy-public-vm env test passed\n'
