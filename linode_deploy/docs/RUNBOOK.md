# Account Manager Linode Runbook

This runbook covers the dedicated public VM staging profile for
`rtk_account_manager`.

## Source Of Truth

- Runtime API and service behavior: repo code and `openapi.yaml`.
- Deployment scripts: `linode_deploy/scripts/`.
- Operator secrets: ignored files under `linode_deploy/secrets/` and local shell
  environment.
- Deployment state: ignored files under `linode_deploy/state/`.

## Prerequisites

Operator machine:

- `curl`, `jq`, `openssl`, `go`, `ssh`, `scp`, `tar`
- `LINODE_TOKEN` with Linode instance/firewall permissions
- `GODADDY_KEY` and `GODADDY_SECRET` for `realtekconnect.com` DNS
- an SSH key pair for the VM
- operator public CIDR for SSH allowlisting

Remote VM target:

- Ubuntu 24.04
- public IPv4 only
- inbound `22/tcp` restricted to operator CIDRs
- inbound `80/tcp` and `443/tcp` public for certbot and HTTPS

## Runtime Shape

- service user: `rtk-account-manager`
- install prefix: `/opt/rtk-account-manager`
- env file: `/etc/rtk-account-manager/account-manager.env`
- state dir: `/var/lib/rtk-account-manager`
- API bind: `127.0.0.1:18081` behind nginx
- PostgreSQL: local database `rtk_account_manager`

## Deploy

```sh
set -a
. ~/.env
. linode_deploy/secrets/account-manager-public-staging.env
set +a

linode_deploy/scripts/provision-public-vm.sh
. linode_deploy/state/rtk-account-manager-staging.env
linode_deploy/scripts/set-godaddy-dns.sh
linode_deploy/scripts/deploy-public-vm.sh
linode_deploy/scripts/verify-public-vm.sh
```

`deploy-public-vm.sh` builds a Linux/amd64 release from the checked-out commit,
uploads it to the VM, installs OS dependencies, creates or updates the local
PostgreSQL role/database, writes the environment file, runs migrations, starts
the API service and cleanup timer, configures nginx, and obtains the Let's
Encrypt certificate when enabled.

## Verify

The external verifier checks:

- `GET /v1/health`
- `POST /v1/auth/register`
- `POST /v1/auth/login`
- `GET /v1/me` with the issued bearer token

Verification artifacts are written under `.artifacts/linode-account-manager-verify/`
and must remain untracked.

## Backup

```sh
set -a
. linode_deploy/secrets/account-manager-public-staging.env
. linode_deploy/state/rtk-account-manager-staging.env
set +a

linode_deploy/scripts/backup-public-postgres.sh
```

The backup script creates a PostgreSQL custom-format dump and a checksum under
`.artifacts/linode-account-manager-backups/`. Raw dumps are not committed.

## Staging Limitations

The default public VM profile is appropriate for staging smoke and admin
integration:

- `AUTH_TOKEN_DELIVERY=log`
- `CROSS_SERVICE_BROKER=log`
- `OIDC_ENABLED=false`
- SMTP optional and usually unset

Production-like identity, notification, and cross-service lifecycle testing must
set SMTP/OIDC/broker configuration explicitly before deployment.
