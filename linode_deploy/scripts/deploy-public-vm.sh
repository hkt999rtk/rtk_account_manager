#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
label="${ACCOUNT_MANAGER_LINODE_LABEL:-rtk-account-manager-staging}"
release="${ACCOUNT_MANAGER_LINODE_RELEASE:-$(git -C "$root_dir" rev-parse --short HEAD)}"
domain="${ACCOUNT_MANAGER_LINODE_DOMAIN:-account-manager.video-cloud-staging.realtekconnect.com}"
certbot_email="${ACCOUNT_MANAGER_LINODE_CERTBOT_EMAIL:-}"
ssh_user="${ACCOUNT_MANAGER_LINODE_SSH_USER:-root}"
ssh_key="${ACCOUNT_MANAGER_LINODE_SSH_KEY:-$HOME/.ssh/id_ed25519_rtkcloud}"
host="${ACCOUNT_MANAGER_LINODE_HOST:-${ACCOUNT_MANAGER_LINODE_PUBLIC_IPV4:-}}"
remote_bundle="${ACCOUNT_MANAGER_LINODE_REMOTE_BUNDLE:-/tmp/rtk-account-manager-${release}.tar.gz}"
artifact_dir="${ACCOUNT_MANAGER_LINODE_ARTIFACT_DIR:-$root_dir/.artifacts/linode-account-manager-deploy/$release}"
bundle="$artifact_dir/rtk_account_manager-${release}.tar.gz"
certbot_enable="${ACCOUNT_MANAGER_LINODE_CERTBOT_ENABLE:-1}"
http_only="${ACCOUNT_MANAGER_LINODE_HTTP_ONLY:-0}"
port="${ACCOUNT_MANAGER_PORT:-18081}"
db_name="${ACCOUNT_MANAGER_POSTGRES_DB:-rtk_account_manager}"
db_user="${ACCOUNT_MANAGER_POSTGRES_USER:-rtk_account_manager}"
db_password="${ACCOUNT_MANAGER_POSTGRES_PASSWORD:-}"

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }
write_env() {
  local key="$1"
  local value="${!key:-}"
  if printf '%s' "$value" | grep -q '[[:cntrl:]]'; then die "$key contains control characters"; fi
  printf '%s=%s\n' "$key" "$value"
}

need ssh
need scp
need go
need tar
need openssl
[ -n "$host" ] || die "ACCOUNT_MANAGER_LINODE_HOST or ACCOUNT_MANAGER_LINODE_PUBLIC_IPV4 is required"
[ -s "$ssh_key" ] || die "SSH key not found: $ssh_key"
[ -n "$certbot_email" ] || [ "$certbot_enable" = 0 ] || die "ACCOUNT_MANAGER_LINODE_CERTBOT_EMAIL is required when certbot is enabled"
[ -n "${JWT_ACCESS_SECRET:-}" ] || die "JWT_ACCESS_SECRET is required"
[ -n "${JWT_REFRESH_SECRET:-}" ] || die "JWT_REFRESH_SECRET is required"
if [ -z "$db_password" ]; then
  db_password="$(openssl rand -hex 32)"
  printf '[account-manager-deploy] generated local PostgreSQL password for this deploy\n' >&2
fi

database_url="postgres://${db_user}:${db_password}@127.0.0.1:5432/${db_name}?sslmode=disable"
mkdir -p "$artifact_dir"

printf '[account-manager-deploy] building Linux release %s\n' "$release" >&2
(
  cd "$root_dir"
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 VERSION="$release" make release >/dev/null
)
cp "$root_dir/dist/rtk_account_manager-${release}.tar.gz" "$bundle"

ssh_opts=(-i "$ssh_key" -o BatchMode=yes -o StrictHostKeyChecking=accept-new)
remote="$ssh_user@$host"

tmp_env="$(mktemp)"
cleanup() { rm -f "$tmp_env"; }
trap cleanup EXIT
{
  printf 'DATABASE_URL=%s\n' "$database_url"
  write_env JWT_ACCESS_SECRET
  write_env JWT_REFRESH_SECRET
  printf 'PORT=%s\n' "$port"
  printf 'ACCESS_TOKEN_TTL=%s\n' "${ACCESS_TOKEN_TTL:-15m}"
  printf 'REFRESH_TOKEN_TTL=%s\n' "${REFRESH_TOKEN_TTL:-720h}"
  printf 'AUTH_TOKEN_DELIVERY=%s\n' "${AUTH_TOKEN_DELIVERY:-log}"
  printf 'EMAIL_VERIFICATION_TTL=%s\n' "${EMAIL_VERIFICATION_TTL:-30m}"
  printf 'PASSWORD_RESET_TTL=%s\n' "${PASSWORD_RESET_TTL:-30m}"
  printf 'OTP_RESEND_INTERVAL=%s\n' "${OTP_RESEND_INTERVAL:-60s}"
  printf 'OTP_MAX_ATTEMPTS=%s\n' "${OTP_MAX_ATTEMPTS:-5}"
  printf 'SIGNUP_CAPTCHA_REQUIRED=%s\n' "${SIGNUP_CAPTCHA_REQUIRED:-false}"
  printf 'SIGNUP_DISPOSABLE_DOMAINS=%s\n' "${SIGNUP_DISPOSABLE_DOMAINS:-}"
  printf 'SMTP_HOST=%s\n' "${SMTP_HOST:-}"
  printf 'SMTP_PORT=%s\n' "${SMTP_PORT:-587}"
  printf 'SMTP_USERNAME=%s\n' "${SMTP_USERNAME:-}"
  printf 'SMTP_PASSWORD=%s\n' "${SMTP_PASSWORD:-}"
  printf 'SMTP_FROM=%s\n' "${SMTP_FROM:-}"
  printf 'OIDC_ENABLED=%s\n' "${OIDC_ENABLED:-false}"
  printf 'OIDC_PROVIDER_ID=%s\n' "${OIDC_PROVIDER_ID:-keycloak}"
  printf 'OIDC_PROVIDER_NAME=%s\n' "${OIDC_PROVIDER_NAME:-Keycloak}"
  printf 'OIDC_ISSUER_URL=%s\n' "${OIDC_ISSUER_URL:-}"
  printf 'OIDC_CLIENT_ID=%s\n' "${OIDC_CLIENT_ID:-}"
  printf 'OIDC_CLIENT_SECRET=%s\n' "${OIDC_CLIENT_SECRET:-}"
  printf 'OIDC_REDIRECT_URL=%s\n' "${OIDC_REDIRECT_URL:-}"
  printf 'OIDC_SCOPES=%s\n' "${OIDC_SCOPES:-openid email profile}"
  printf 'OIDC_AUTO_LINK_EMAIL=%s\n' "${OIDC_AUTO_LINK_EMAIL:-false}"
  printf 'CROSS_SERVICE_BROKER=%s\n' "${CROSS_SERVICE_BROKER:-log}"
  printf 'ACCOUNT_VIDEO_COMMANDS_STREAM=%s\n' "${ACCOUNT_VIDEO_COMMANDS_STREAM:-account.video.commands}"
  printf 'VIDEO_ACCOUNT_EVENTS_STREAM=%s\n' "${VIDEO_ACCOUNT_EVENTS_STREAM:-video.account.events}"
  printf 'CROSS_SERVICE_CONSUMER_GROUP=%s\n' "${CROSS_SERVICE_CONSUMER_GROUP:-rtk_account_manager}"
  printf 'CROSS_SERVICE_MAX_ATTEMPTS=%s\n' "${CROSS_SERVICE_MAX_ATTEMPTS:-5}"
  printf 'CROSS_SERVICE_POLL_INTERVAL=%s\n' "${CROSS_SERVICE_POLL_INTERVAL:-5s}"
  printf 'AZURE_EVENTHUB_CONNECTION_STRING=%s\n' "${AZURE_EVENTHUB_CONNECTION_STRING:-}"
  printf 'AZURE_EVENTHUB_CHECKPOINT_FILE=%s\n' "${AZURE_EVENTHUB_CHECKPOINT_FILE:-/var/lib/rtk-account-manager/azure_eventhubs_checkpoint.json}"
} > "$tmp_env"
chmod 0600 "$tmp_env"

printf '[account-manager-deploy] uploading release bundle to %s\n' "$remote" >&2
ssh "${ssh_opts[@]}" "$remote" "mkdir -p /tmp/rtk-account-manager-deploy"
scp "${ssh_opts[@]}" "$bundle" "$remote:$remote_bundle"
scp "${ssh_opts[@]}" "$tmp_env" "$remote:/tmp/rtk-account-manager-deploy/account-manager.env"

printf '[account-manager-deploy] installing runtime on %s\n' "$remote" >&2
ssh "${ssh_opts[@]}" "$remote" bash -s -- "$remote_bundle" "$release" "$domain" "$certbot_email" "$certbot_enable" "$http_only" "$port" "$db_name" "$db_user" "$db_password" <<'REMOTE'
set -euo pipefail
remote_bundle="$1"
release="$2"
domain="$3"
certbot_email="$4"
certbot_enable="$5"
http_only="$6"
port="$7"
db_name="$8"
db_user="$9"
db_password="${10}"

export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get install -y ca-certificates curl gnupg2 lsb-release ubuntu-keyring postgresql postgresql-contrib systemd tar
curl -fsS https://nginx.org/keys/nginx_signing.key | gpg --dearmor -o /usr/share/keyrings/nginx-archive-keyring.gpg.tmp
mv /usr/share/keyrings/nginx-archive-keyring.gpg.tmp /usr/share/keyrings/nginx-archive-keyring.gpg
printf 'deb [signed-by=/usr/share/keyrings/nginx-archive-keyring.gpg] http://nginx.org/packages/ubuntu %s nginx\n' "$(lsb_release -cs)" > /etc/apt/sources.list.d/nginx-org.list
cat > /etc/apt/preferences.d/99nginx <<'PREF'
Package: *
Pin: origin nginx.org
Pin-Priority: 900
PREF
apt-get update -y
nginx_candidate="$(apt-cache policy nginx | awk '/Candidate:/ {print $2}')"
dpkg --compare-versions "$nginx_candidate" ge 1.30.0
apt-get install -y -o Dpkg::Options::=--force-confold nginx certbot python3-certbot-nginx
if ! grep -q 'server_names_hash_bucket_size' /etc/nginx/nginx.conf; then
  sed -i '/http {/a\    server_names_hash_bucket_size 128;' /etc/nginx/nginx.conf
fi
if [ -d /etc/nginx/sites-enabled ] && ! grep -q 'sites-enabled' /etc/nginx/nginx.conf && [ ! -f /etc/nginx/conf.d/rtk-sites-enabled.conf ]; then
  printf 'include /etc/nginx/sites-enabled/*;\n' > /etc/nginx/conf.d/rtk-sites-enabled.conf
fi
systemctl enable --now postgresql nginx

sudo -u postgres psql -v ON_ERROR_STOP=1 <<SQL
DO \$\$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = '$db_user') THEN
    CREATE ROLE "$db_user" LOGIN PASSWORD '$db_password';
  ELSE
    ALTER ROLE "$db_user" WITH PASSWORD '$db_password';
  END IF;
END
\$\$;
SELECT 'CREATE DATABASE "$db_name" OWNER "$db_user"'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '$db_name')\gexec
SQL

rm -rf /tmp/rtk-account-manager-release
mkdir -p /tmp/rtk-account-manager-release
tar -xzf "$remote_bundle" -C /tmp/rtk-account-manager-release --strip-components=1
cp /tmp/rtk-account-manager-deploy/account-manager.env /tmp/rtk-account-manager-release/deploy/account-manager.env.example
(cd /tmp/rtk-account-manager-release && ./deploy/install.sh)
install -m 0600 -o root -g root /tmp/rtk-account-manager-deploy/account-manager.env /etc/rtk-account-manager/account-manager.env

systemctl daemon-reload
systemctl start rtk-account-manager-migrate.service
systemctl enable --now rtk-account-manager-cleanup-tokens.timer
systemctl enable --now rtk-account-manager.service
systemctl restart rtk-account-manager.service

cat > /etc/nginx/sites-available/rtk-account-manager.conf <<NGINX
server {
    listen 80;
    server_name $domain;

    client_max_body_size 10m;

    location / {
        proxy_pass http://127.0.0.1:$port;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
    }
}
NGINX
ln -sf /etc/nginx/sites-available/rtk-account-manager.conf /etc/nginx/sites-enabled/rtk-account-manager.conf
rm -f /etc/nginx/sites-enabled/default
nginx -t
systemctl reload nginx

for _ in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:$port/v1/health" >/dev/null; then
    ready=1
    break
  fi
  sleep 1
done
if [ "${ready:-0}" != "1" ]; then
  journalctl -u rtk-account-manager -n 160 --no-pager >&2 || true
  exit 1
fi

if [ "$certbot_enable" = "1" ] && [ "$http_only" != "1" ]; then
  certbot --nginx --non-interactive --agree-tos --email "$certbot_email" -d "$domain" --redirect
fi

systemctl is-active rtk-account-manager.service
systemctl is-active nginx
printf 'rtk_account_manager %s deployed for %s\n' "$release" "$domain"
REMOTE

printf '[account-manager-deploy] deployed %s to %s\n' "$release" "$domain" >&2
