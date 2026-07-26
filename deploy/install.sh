#!/usr/bin/env bash
set -euo pipefail

prefix=${PREFIX:-/opt/rtk-account-manager}
etc_dir=${ETC_DIR:-/etc/rtk-account-manager}
systemd_dir=${SYSTEMD_DIR:-/etc/systemd/system}
state_dir=${STATE_DIR:-/var/lib/rtk-account-manager}
service_user=${SERVICE_USER:-rtk-account-manager}
service_group=${SERVICE_GROUP:-$service_user}

release_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)

if [ "$(id -u)" -ne 0 ]; then
  echo "install.sh must run as root" >&2
  exit 1
fi

if ! getent group "$service_group" >/dev/null; then
  groupadd --system "$service_group"
fi

if ! id -u "$service_user" >/dev/null 2>&1; then
  useradd --system \
    --gid "$service_group" \
    --home-dir "$state_dir" \
    --shell /usr/sbin/nologin \
    "$service_user"
fi

install -d -o root -g root -m 0755 "$prefix" "$prefix/bin" "$systemd_dir"
install -d -o "$service_user" -g "$service_group" -m 0755 "$state_dir"
install -d -o root -g root -m 0750 "$etc_dir"

install -m 0755 "$release_dir/bin/rtk-account-manager" "$prefix/bin/rtk-account-manager"
install -m 0755 "$release_dir/bin/rtk-account-manager-migrate" "$prefix/bin/rtk-account-manager-migrate"
install -m 0755 "$release_dir/bin/rtk-account-manager-outbox-worker" "$prefix/bin/rtk-account-manager-outbox-worker"
install -m 0755 "$release_dir/bin/rtk-account-manager-inbox-worker" "$prefix/bin/rtk-account-manager-inbox-worker"
install -m 0755 "$release_dir/bin/rtk-account-manager-email-worker" "$prefix/bin/rtk-account-manager-email-worker"
install -m 0755 "$release_dir/bin/rtk-account-manager-email-outbox-admin" "$prefix/bin/rtk-account-manager-email-outbox-admin"
install -m 0755 "$release_dir/bin/rtk-account-manager-cleanup-tokens" "$prefix/bin/rtk-account-manager-cleanup-tokens"
install -m 0755 "$release_dir/deploy/verify.sh" "$prefix/verify.sh"

rm -rf "$prefix/migrations"
mkdir -p "$prefix/migrations"
cp -R "$release_dir/migrations/." "$prefix/migrations/"
chown -R root:root "$prefix/migrations"
chmod -R go-w "$prefix/migrations"

install -m 0644 "$release_dir"/deploy/systemd/*.service "$systemd_dir/"
install -m 0644 "$release_dir"/deploy/systemd/*.timer "$systemd_dir/"
install -m 0644 "$release_dir/deploy/account-manager.env.example" "$etc_dir/account-manager.env.example"

install -m 0644 "$release_dir/release-manifest.txt" "$prefix/release-manifest.txt"

echo "installed RTK Account Manager release into $prefix"
