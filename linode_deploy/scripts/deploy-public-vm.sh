#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

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

label="${ACCOUNT_MANAGER_LINODE_LABEL:-rtk-account-manager-staging}"
release="${ACCOUNT_MANAGER_LINODE_RELEASE:-$(git -C "$root_dir" rev-parse --short HEAD)}"
domain="${ACCOUNT_MANAGER_LINODE_DOMAIN:-account-manager.video-cloud-staging.realtekconnect.com}"
certbot_email="${ACCOUNT_MANAGER_LINODE_CERTBOT_EMAIL:-}"
ssh_user="${ACCOUNT_MANAGER_LINODE_SSH_USER:-root}"
ssh_key="${ACCOUNT_MANAGER_LINODE_SSH_KEY:-$HOME/.ssh/id_ed25519_rtkcloud}"
host="${ACCOUNT_MANAGER_LINODE_HOST:-${ACCOUNT_MANAGER_LINODE_PUBLIC_IPV4:-}}"
node_exporter_listen_addr="${ACCOUNT_MANAGER_PROMETHEUS_NODE_EXPORTER_LISTEN_ADDR:-}"
remote_bundle="${ACCOUNT_MANAGER_LINODE_REMOTE_BUNDLE:-/tmp/rtk-account-manager-${release}.tar.gz}"
artifact_dir="${ACCOUNT_MANAGER_LINODE_ARTIFACT_DIR:-$root_dir/.artifacts/linode-account-manager-deploy/$release}"
release_bundle="${ACCOUNT_MANAGER_LINODE_RELEASE_BUNDLE:-}"
bundle="${release_bundle:-$artifact_dir/rtk_account_manager-${release}.tar.gz}"
certbot_enable="${ACCOUNT_MANAGER_LINODE_CERTBOT_ENABLE:-1}"
cert_cache_dir="${ACCOUNT_MANAGER_LINODE_CERT_CACHE_DIR:-}"
cert_cache_enabled=0
http_only="${ACCOUNT_MANAGER_LINODE_HTTP_ONLY:-0}"
port="${ACCOUNT_MANAGER_PORT:-18081}"
db_name="${ACCOUNT_MANAGER_POSTGRES_DB:-rtk_account_manager}"
db_user="${ACCOUNT_MANAGER_POSTGRES_USER:-rtk_account_manager}"
db_password="${ACCOUNT_MANAGER_POSTGRES_PASSWORD:-}"
app_cert_issuer_base_url="${APP_CERT_ISSUER_BASE_URL:-}"
app_cert_issuer_client_cert="${APP_CERT_ISSUER_CLIENT_CERT:-/etc/rtk-account-manager/certissuer-client.pem}"
app_cert_issuer_client_key="${APP_CERT_ISSUER_CLIENT_KEY:-/etc/rtk-account-manager/certissuer-client-key.pem}"
app_cert_issuer_ca_file="${APP_CERT_ISSUER_CA_FILE:-/etc/rtk-account-manager/certissuer-ca.pem}"
app_cert_issuer_client_cert_source="${APP_CERT_ISSUER_CLIENT_CERT_SOURCE:-}"
app_cert_issuer_client_key_source="${APP_CERT_ISSUER_CLIENT_KEY_SOURCE:-}"
app_cert_issuer_ca_source="${APP_CERT_ISSUER_CA_SOURCE:-}"

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
if [ -z "$node_exporter_listen_addr" ]; then
  if [ -n "${ACCOUNT_MANAGER_LINODE_PRIVATE_IPV4:-}" ]; then
    node_exporter_listen_addr="$ACCOUNT_MANAGER_LINODE_PRIVATE_IPV4:9100"
  else
    node_exporter_listen_addr="127.0.0.1:9100"
  fi
fi
printf '[account-manager-deploy] node exporter listen address: %s\n' "$node_exporter_listen_addr" >&2
[ -n "$certbot_email" ] || [ "$certbot_enable" = 0 ] || die "ACCOUNT_MANAGER_LINODE_CERTBOT_EMAIL is required when certbot is enabled"
if [ -n "$cert_cache_dir" ]; then
  [ -s "$cert_cache_dir/fullchain.pem" ] || die "cached certificate fullchain not found: $cert_cache_dir/fullchain.pem"
  [ -s "$cert_cache_dir/privkey.pem" ] || die "cached certificate private key not found: $cert_cache_dir/privkey.pem"
  openssl x509 -in "$cert_cache_dir/fullchain.pem" -noout -checkend "${ACCOUNT_MANAGER_LINODE_CERT_CACHE_MIN_VALID_SECONDS:-604800}" >/dev/null \
    || die "cached certificate is expired or too close to expiry: $cert_cache_dir/fullchain.pem"
  cert_cache_enabled=1
fi
[ -n "${JWT_ACCESS_SECRET:-}" ] || die "JWT_ACCESS_SECRET is required"
[ -n "${JWT_REFRESH_SECRET:-}" ] || die "JWT_REFRESH_SECRET is required"
[[ "$db_name" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || die "ACCOUNT_MANAGER_POSTGRES_DB must be a PostgreSQL identifier"
[[ "$db_user" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || die "ACCOUNT_MANAGER_POSTGRES_USER must be a PostgreSQL identifier"
if [ -z "$db_password" ]; then
  db_password="$(openssl rand -hex 32)"
  printf '[account-manager-deploy] generated local PostgreSQL password for this deploy\n' >&2
fi
if [ -n "$app_cert_issuer_base_url" ]; then
  [ -s "$app_cert_issuer_client_cert_source" ] || die "APP_CERT_ISSUER_CLIENT_CERT_SOURCE is required when APP_CERT_ISSUER_BASE_URL is set"
  [ -s "$app_cert_issuer_client_key_source" ] || die "APP_CERT_ISSUER_CLIENT_KEY_SOURCE is required when APP_CERT_ISSUER_BASE_URL is set"
  [ -s "$app_cert_issuer_ca_source" ] || die "APP_CERT_ISSUER_CA_SOURCE is required when APP_CERT_ISSUER_BASE_URL is set"
fi

database_url="postgres://${db_user}:${db_password}@127.0.0.1:5432/${db_name}?sslmode=disable"
mkdir -p "$artifact_dir"

if [ -n "$release_bundle" ]; then
  [ -s "$release_bundle" ] || die "ACCOUNT_MANAGER_LINODE_RELEASE_BUNDLE not found: $release_bundle"
  printf '[account-manager-deploy] using release bundle %s\n' "$release_bundle" >&2
else
  printf '[account-manager-deploy] building Linux release %s\n' "$release" >&2
  (
    cd "$root_dir"
    GOWORK=off GOOS=linux GOARCH=amd64 CGO_ENABLED=0 VERSION="$release" make release >/dev/null
  )
  cp "$root_dir/dist/rtk_account_manager-${release}.tar.gz" "$bundle"
fi

ssh_opts=(-i "$ssh_key" -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/dev/null)
remote="$ssh_user@$host"

tmp_env="$(mktemp)"
cleanup() { rm -f "$tmp_env"; }
trap cleanup EXIT
{
  printf 'DATABASE_URL=%s\n' "$database_url"
  write_env JWT_ACCESS_SECRET
  write_env JWT_REFRESH_SECRET
  printf 'PORT=%s\n' "$port"
  printf 'ACCOUNT_MANAGER_LOG_LEVEL=%s\n' "${ACCOUNT_MANAGER_LOG_LEVEL:-info}"
  printf 'ACCOUNT_MANAGER_LOG_DEVELOPMENT=%s\n' "${ACCOUNT_MANAGER_LOG_DEVELOPMENT:-false}"
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
  printf 'ACCOUNT_MANAGER_INTERNAL_AUTH_TOKEN=%s\n' "${ACCOUNT_MANAGER_INTERNAL_AUTH_TOKEN:-}"
  printf 'CROSS_SERVICE_BROKER=%s\n' "${CROSS_SERVICE_BROKER:-log}"
  printf 'ACCOUNT_VIDEO_COMMANDS_STREAM=%s\n' "${ACCOUNT_VIDEO_COMMANDS_STREAM:-account.video.commands}"
  printf 'VIDEO_ACCOUNT_EVENTS_STREAM=%s\n' "${VIDEO_ACCOUNT_EVENTS_STREAM:-video.account.events}"
  printf 'CROSS_SERVICE_CONSUMER_GROUP=%s\n' "${CROSS_SERVICE_CONSUMER_GROUP:-rtk_account_manager}"
  printf 'CROSS_SERVICE_MAX_ATTEMPTS=%s\n' "${CROSS_SERVICE_MAX_ATTEMPTS:-5}"
  printf 'CROSS_SERVICE_POLL_INTERVAL=%s\n' "${CROSS_SERVICE_POLL_INTERVAL:-5s}"
  printf 'AZURE_EVENTHUB_CONNECTION_STRING=%s\n' "${AZURE_EVENTHUB_CONNECTION_STRING:-}"
  printf 'AZURE_EVENTHUB_CHECKPOINT_FILE=%s\n' "${AZURE_EVENTHUB_CHECKPOINT_FILE:-/var/lib/rtk-account-manager/azure_eventhubs_checkpoint.json}"
  printf 'APP_CERT_ISSUER_BASE_URL=%s\n' "$app_cert_issuer_base_url"
  printf 'APP_CERT_ISSUER_CLIENT_CERT=%s\n' "$app_cert_issuer_client_cert"
  printf 'APP_CERT_ISSUER_CLIENT_KEY=%s\n' "$app_cert_issuer_client_key"
  printf 'APP_CERT_ISSUER_CA_FILE=%s\n' "$app_cert_issuer_ca_file"
  printf 'APP_CERT_ISSUER_TIMEOUT=%s\n' "${APP_CERT_ISSUER_TIMEOUT:-10s}"
} > "$tmp_env"
chmod 0600 "$tmp_env"

printf '[account-manager-deploy] uploading release bundle to %s\n' "$remote" >&2
ssh "${ssh_opts[@]}" "$remote" "mkdir -p /tmp/rtk-account-manager-deploy"
scp "${ssh_opts[@]}" "$bundle" "$remote:$remote_bundle"
scp "${ssh_opts[@]}" "$tmp_env" "$remote:/tmp/rtk-account-manager-deploy/account-manager.env"
if [ -n "$cert_cache_dir" ]; then
  printf '[account-manager-deploy] uploading cached certificate for %s\n' "$domain" >&2
  ssh "${ssh_opts[@]}" "$remote" "mkdir -p /tmp/rtk-account-manager-deploy/cert-cache"
  scp "${ssh_opts[@]}" "$cert_cache_dir/fullchain.pem" "$remote:/tmp/rtk-account-manager-deploy/cert-cache/fullchain.pem"
  scp "${ssh_opts[@]}" "$cert_cache_dir/privkey.pem" "$remote:/tmp/rtk-account-manager-deploy/cert-cache/privkey.pem"
fi
if [ -n "$app_cert_issuer_base_url" ]; then
  printf '[account-manager-deploy] uploading app certissuer client credentials\n' >&2
  ssh "${ssh_opts[@]}" "$remote" "mkdir -p /tmp/rtk-account-manager-deploy/app-cert-issuer"
  scp "${ssh_opts[@]}" "$app_cert_issuer_client_cert_source" "$remote:/tmp/rtk-account-manager-deploy/app-cert-issuer/client.pem"
  scp "${ssh_opts[@]}" "$app_cert_issuer_client_key_source" "$remote:/tmp/rtk-account-manager-deploy/app-cert-issuer/client-key.pem"
  scp "${ssh_opts[@]}" "$app_cert_issuer_ca_source" "$remote:/tmp/rtk-account-manager-deploy/app-cert-issuer/ca.pem"
fi

printf '[account-manager-deploy] installing runtime on %s\n' "$remote" >&2
ssh "${ssh_opts[@]}" "$remote" bash -s -- "$remote_bundle" "$release" "$domain" "$certbot_email" "$certbot_enable" "$http_only" "$port" "$db_name" "$db_user" "$db_password" "$cert_cache_enabled" "$node_exporter_listen_addr" <<'REMOTE'
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
cert_cache_enabled="${11:-0}"
node_exporter_listen_addr="${12:-127.0.0.1:9100}"

export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get install -y ca-certificates curl gnupg2 lsb-release ubuntu-keyring postgresql postgresql-contrib systemd tar prometheus-node-exporter
printf 'ARGS="--web.listen-address=%s"\n' "$node_exporter_listen_addr" > /etc/default/prometheus-node-exporter
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
mkdir -p /etc/nginx/sites-available /etc/nginx/sites-enabled
if [ -d /etc/nginx/sites-enabled ] && ! grep -q 'sites-enabled' /etc/nginx/nginx.conf && [ ! -f /etc/nginx/conf.d/rtk-sites-enabled.conf ]; then
  printf 'include /etc/nginx/sites-enabled/*;\n' > /etc/nginx/conf.d/rtk-sites-enabled.conf
fi
systemctl enable --now postgresql nginx
mkdir -p /var/www/certbot/.well-known/acme-challenge

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
ALTER DATABASE "$db_name" OWNER TO "$db_user";
GRANT CONNECT, TEMPORARY ON DATABASE "$db_name" TO "$db_user";
SQL

sudo -u postgres psql -d "$db_name" -v ON_ERROR_STOP=1 <<SQL
ALTER SCHEMA public OWNER TO "$db_user";
GRANT USAGE, CREATE ON SCHEMA public TO "$db_user";

DO \$\$
DECLARE
  obj record;
BEGIN
  FOR obj IN
    SELECT
      n.nspname AS schema_name,
      c.relname AS object_name,
      CASE c.relkind
        WHEN 'r' THEN 'TABLE'
        WHEN 'p' THEN 'TABLE'
        WHEN 'S' THEN 'SEQUENCE'
        WHEN 'v' THEN 'VIEW'
        WHEN 'm' THEN 'MATERIALIZED VIEW'
        WHEN 'f' THEN 'FOREIGN TABLE'
      END AS object_kind
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public'
      AND c.relkind IN ('r', 'p', 'S', 'v', 'm', 'f')
      AND c.relowner <> (SELECT oid FROM pg_roles WHERE rolname = '$db_user')
  LOOP
    EXECUTE format('ALTER %s %I.%I OWNER TO %I', obj.object_kind, obj.schema_name, obj.object_name, '$db_user');
  END LOOP;

  FOR obj IN
    SELECT p.oid::regprocedure::text AS function_signature
    FROM pg_proc p
    JOIN pg_namespace n ON n.oid = p.pronamespace
    WHERE n.nspname = 'public'
      AND p.prokind = 'f'
      AND p.proowner <> (SELECT oid FROM pg_roles WHERE rolname = '$db_user')
  LOOP
    EXECUTE format('ALTER FUNCTION %s OWNER TO %I', obj.function_signature, '$db_user');
  END LOOP;
END
\$\$;

GRANT SELECT, INSERT, UPDATE, DELETE, REFERENCES, TRIGGER ON ALL TABLES IN SCHEMA public TO "$db_user";
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO "$db_user";
ALTER DEFAULT PRIVILEGES FOR ROLE "$db_user" IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE, REFERENCES, TRIGGER ON TABLES TO "$db_user";
ALTER DEFAULT PRIVILEGES FOR ROLE "$db_user" IN SCHEMA public
  GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO "$db_user";
SQL

rm -rf /tmp/rtk-account-manager-release
mkdir -p /tmp/rtk-account-manager-release
tar --warning=no-unknown-keyword -xzf "$remote_bundle" -C /tmp/rtk-account-manager-release --strip-components=1
cp /tmp/rtk-account-manager-deploy/account-manager.env /tmp/rtk-account-manager-release/deploy/account-manager.env.example
(cd /tmp/rtk-account-manager-release && ./deploy/install.sh)
install -m 0600 -o root -g root /tmp/rtk-account-manager-deploy/account-manager.env /etc/rtk-account-manager/account-manager.env
if [ -d /tmp/rtk-account-manager-deploy/app-cert-issuer ]; then
  chgrp rtk-account-manager /etc/rtk-account-manager
  chmod 0750 /etc/rtk-account-manager
  install -m 0644 -o root -g root /tmp/rtk-account-manager-deploy/app-cert-issuer/client.pem /etc/rtk-account-manager/certissuer-client.pem
  install -m 0640 -o root -g rtk-account-manager /tmp/rtk-account-manager-deploy/app-cert-issuer/client-key.pem /etc/rtk-account-manager/certissuer-client-key.pem
  install -m 0644 -o root -g root /tmp/rtk-account-manager-deploy/app-cert-issuer/ca.pem /etc/rtk-account-manager/certissuer-ca.pem
fi

systemctl daemon-reload
systemctl enable --now prometheus-node-exporter
systemctl restart prometheus-node-exporter
systemctl is-active prometheus-node-exporter
for _ in $(seq 1 10); do
  if ss -lnt | grep -F "$node_exporter_listen_addr" >/dev/null; then
    node_exporter_ready=1
    break
  fi
  sleep 1
done
if [ "${node_exporter_ready:-0}" != "1" ]; then
  ss -lnt >&2 || true
  exit 1
fi
systemctl start rtk-account-manager-migrate.service
systemctl enable --now rtk-account-manager-cleanup-tokens.timer
systemctl enable --now rtk-account-manager.service
systemctl restart rtk-account-manager.service

cat > /etc/nginx/sites-available/rtk-account-manager.conf <<NGINX
server {
    listen 80;
    server_name $domain;

    client_max_body_size 10m;

    location ^~ /.well-known/acme-challenge/ {
        alias /var/www/certbot/.well-known/acme-challenge/;
        default_type text/plain;
    }

    location = /metrics/prometheus {
        return 404;
    }

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
rm -f /etc/nginx/conf.d/rtk-account-manager.conf /etc/nginx/sites-enabled/default /etc/nginx/conf.d/default.conf
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

if [ "$cert_cache_enabled" = "1" ] && [ "$http_only" != "1" ]; then
  archive_dir="/etc/letsencrypt/archive/$domain"
  live_dir="/etc/letsencrypt/live/$domain"
  renewal_conf="/etc/letsencrypt/renewal/$domain.conf"
  mkdir -p "$archive_dir" "$live_dir" /etc/letsencrypt/renewal /var/www/certbot/.well-known/acme-challenge
  install -m 0644 /tmp/rtk-account-manager-deploy/cert-cache/fullchain.pem "$archive_dir/fullchain1.pem"
  install -m 0600 /tmp/rtk-account-manager-deploy/cert-cache/privkey.pem "$archive_dir/privkey1.pem"
  awk 'BEGIN{n=0} /-----BEGIN CERTIFICATE-----/{n++} n==1{print > cert} n>1{print > chain}' \
    cert="$archive_dir/cert1.pem" chain="$archive_dir/chain1.pem" "$archive_dir/fullchain1.pem"
  if [ ! -s "$archive_dir/chain1.pem" ]; then
    cp "$archive_dir/fullchain1.pem" "$archive_dir/chain1.pem"
  fi
  ln -sfn "../../archive/$domain/cert1.pem" "$live_dir/cert.pem"
  ln -sfn "../../archive/$domain/chain1.pem" "$live_dir/chain.pem"
  ln -sfn "../../archive/$domain/fullchain1.pem" "$live_dir/fullchain.pem"
  ln -sfn "../../archive/$domain/privkey1.pem" "$live_dir/privkey.pem"
  cat > "$renewal_conf" <<RENEWAL
version = 2.9.0
archive_dir = /etc/letsencrypt/archive/$domain
cert = /etc/letsencrypt/live/$domain/cert.pem
privkey = /etc/letsencrypt/live/$domain/privkey.pem
chain = /etc/letsencrypt/live/$domain/chain.pem
fullchain = /etc/letsencrypt/live/$domain/fullchain.pem

[renewalparams]
account =
authenticator = webroot
webroot_path = /var/www/certbot
server = https://acme-v02.api.letsencrypt.org/directory
key_type = rsa
deploy_hook = systemctl reload nginx
RENEWAL
  certbot register --non-interactive --agree-tos --email "$certbot_email" >/dev/null 2>&1 || true
  account="$(find /etc/letsencrypt/accounts/acme-v02.api.letsencrypt.org/directory -mindepth 1 -maxdepth 1 -type d 2>/dev/null | head -n1 | xargs basename || true)"
  if [ -n "$account" ]; then
    sed -i "s/^account =.*/account = $account/" "$renewal_conf"
  fi
  cat > /etc/nginx/sites-available/rtk-account-manager.conf <<NGINX
server {
    listen 80;
    server_name $domain;

    location ^~ /.well-known/acme-challenge/ {
        alias /var/www/certbot/.well-known/acme-challenge/;
        default_type text/plain;
    }

    location / {
        return 301 https://\$host\$request_uri;
    }
}

server {
    listen 443 ssl;
    server_name $domain;

    ssl_certificate /etc/letsencrypt/live/$domain/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/$domain/privkey.pem;
    client_max_body_size 10m;

    location ^~ /.well-known/acme-challenge/ {
        alias /var/www/certbot/.well-known/acme-challenge/;
        default_type text/plain;
    }

    location = /metrics/prometheus {
        return 404;
    }

    location / {
        proxy_pass http://127.0.0.1:$port;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
}
NGINX
  nginx -t
  systemctl reload nginx
  systemctl enable --now certbot.timer
  systemctl is-enabled certbot.timer >/dev/null
  printf 'installed cached certificate lineage for %s\n' "$domain"
elif [ "$certbot_enable" = "1" ] && [ "$http_only" != "1" ]; then
  certbot_log="$(mktemp)"
  set +e
  certbot --nginx --non-interactive --agree-tos --email "$certbot_email" -d "$domain" --redirect 2>&1 | tee "$certbot_log"
  certbot_status="${PIPESTATUS[0]}"
  set -e
  if [ "$certbot_status" -ne 0 ]; then
    if grep -Eqi 'too many certificates|rate-limits|rate limit' "$certbot_log"; then
      retry_after="$(sed -nE 's/.*retry after ([^:]+:[^:]+:[^ ]+ UTC).*/\1/p' "$certbot_log" | tail -n 1)"
      printf 'error: Let'\''s Encrypt rate limit hit for %s; Account Manager deploy stopped before verify.' "$domain" >&2
      if [ -n "$retry_after" ]; then
        printf ' Retry after %s.' "$retry_after" >&2
      fi
      printf ' See Certbot output above and %s.\n' "$certbot_log" >&2
    else
      printf 'error: Certbot failed for %s; Account Manager deploy stopped before verify. See output above and %s.\n' "$domain" "$certbot_log" >&2
    fi
    exit "$certbot_status"
  fi
  rm -f "$certbot_log"
  systemctl enable --now certbot.timer
  systemctl is-enabled certbot.timer >/dev/null
fi

systemctl is-active rtk-account-manager.service
systemctl is-active nginx
systemctl enable --now rtk-account-manager-outbox-worker.service rtk-account-manager-inbox-worker.service
systemctl restart rtk-account-manager-outbox-worker.service rtk-account-manager-inbox-worker.service
systemctl is-active rtk-account-manager-outbox-worker.service
systemctl is-active rtk-account-manager-inbox-worker.service
printf 'rtk_account_manager %s deployed for %s\n' "$release" "$domain"
REMOTE

printf '[account-manager-deploy] deployed %s to %s\n' "$release" "$domain" >&2
