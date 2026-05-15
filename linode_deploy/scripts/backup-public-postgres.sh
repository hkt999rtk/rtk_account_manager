#!/usr/bin/env bash
set -euo pipefail

host="${ACCOUNT_MANAGER_LINODE_HOST:-${ACCOUNT_MANAGER_LINODE_PUBLIC_IPV4:-}}"
ssh_user="${ACCOUNT_MANAGER_LINODE_SSH_USER:-root}"
ssh_key="${ACCOUNT_MANAGER_LINODE_SSH_KEY:-$HOME/.ssh/id_ed25519_rtkcloud}"
db_name="${ACCOUNT_MANAGER_POSTGRES_DB:-rtk_account_manager}"
backup_dir="${ACCOUNT_MANAGER_LINODE_BACKUP_DIR:-.artifacts/linode-account-manager-backups}"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
remote_archive="/tmp/rtk-account-manager-db-$stamp.dump"
local_archive="$backup_dir/rtk-account-manager-db-$stamp.dump"

[ -n "$host" ] || { echo "ACCOUNT_MANAGER_LINODE_HOST or ACCOUNT_MANAGER_LINODE_PUBLIC_IPV4 is required" >&2; exit 1; }
[ -s "$ssh_key" ] || { echo "SSH key not found: $ssh_key" >&2; exit 1; }
mkdir -p "$backup_dir"
ssh_opts=(-i "$ssh_key" -o BatchMode=yes -o StrictHostKeyChecking=accept-new)
remote="$ssh_user@$host"

ssh "${ssh_opts[@]}" "$remote" "sudo -u postgres pg_dump -Fc '$db_name' > '$remote_archive'"
scp "${ssh_opts[@]}" "$remote:$remote_archive" "$local_archive"
ssh "${ssh_opts[@]}" "$remote" "rm -f '$remote_archive'"
sha256sum "$local_archive" > "$local_archive.sha256"
printf 'Backup written: %s\n' "$local_archive"
