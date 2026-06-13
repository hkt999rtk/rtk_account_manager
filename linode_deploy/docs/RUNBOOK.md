# Account Manager Linode Runbook

This runbook covers the dedicated public VM staging profile for
`rtk_account_manager`. It is retained as a legacy Linode VM migration reference
and rollback path. The production migration target is Linode Kubernetes Engine
(LKE), gated by the workspace `docs/lke-migration-inventory.md`.

## Source Of Truth

- Runtime API and service behavior: repo code and `openapi.yaml`.
- Deployment scripts: `linode_deploy/scripts/`.
- Operator secrets: workspace `.secrets/<environment>/<provider>/account-manager/`
  through `DEPLOY_SECRETS_DIR`, with `linode_deploy/secrets/` kept as a legacy
  fallback.
- Deployment state: `DEPLOY_SECRETS_DIR/state/` when set, with
  `linode_deploy/state/` kept as a legacy fallback.

## LKE Target Profile

The LKE target should preserve Account Manager API behavior while replacing the
public VM, nginx, local env file, and systemd service with Kubernetes
primitives:

- Run the API as a Deployment in an `account-manager` namespace with a
  ClusterIP Service and readiness/liveness probes.
- Expose public HTTPS through Linode NodeBalancer plus Ingress or Gateway API.
  cert-manager should own TLS automation.
- Keep `/v1/health` as the external smoke endpoint and keep
  `/metrics/prometheus` private to the observability namespace.
- Source runtime secrets from OpenBao or an approved secret manager. Kubernetes
  Secrets may hold injected runtime material only; do not commit env files,
  DSNs, tokens, or signing material.
- Do not assume the current local PostgreSQL VM database moves into Kubernetes.
  Compare temporary VM/external database retention, a PostgreSQL operator, a
  StatefulSet, or a managed/external PostgreSQL option before cutover.
- Database migration and rollback must be release-controlled and restore-tested
  before production traffic moves to LKE.

TODO: confirm LKE namespace naming, database target, migration job shape,
cert-manager issuer, OpenBao auth role, NetworkPolicy, and backup target before
writing production manifests.

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
- inbound `443/tcp` public for HTTPS; Let's Encrypt uses GoDaddy DNS-01, so
  public `80/tcp` is not required

## Runtime Shape

- service user: `rtk-account-manager`
- install prefix: `/opt/rtk-account-manager`
- env file: `/etc/rtk-account-manager/account-manager.env`
- state dir: `/var/lib/rtk-account-manager`
- API bind: `:18081`; public nginx proxies through loopback and central
  Prometheus scrapes `/metrics/prometheus` over the private VPC IP
- PostgreSQL: local database `rtk_account_manager`

## Deploy

```sh
export WORKSPACE=/path/to/rtk_cloud_workspace
export DEPLOY_SECRETS_DIR="$WORKSPACE/.secrets/staging/linode/account-manager"

linode_deploy/scripts/provision-public-vm.sh
linode_deploy/scripts/set-godaddy-dns.sh
linode_deploy/scripts/deploy-public-vm.sh
linode_deploy/scripts/verify-public-vm.sh
```

The deploy scripts source
`$DEPLOY_SECRETS_DIR/env/account-manager-public-staging.env` and
`$DEPLOY_SECRETS_DIR/state/rtk-account-manager-staging.env` when present. For
standalone repo usage, the legacy `linode_deploy/secrets/` and
`linode_deploy/state/` paths remain supported.

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

When `ACCOUNT_MANAGER_VERIFY_BRAND_CLOUD=1` is set, the verifier also checks
the platform-admin brand-cloud management path:

- `POST /v1/auth/login` with Account Manager platform-admin credentials
- `POST /v1/admin/brand-clouds`
- `GET /v1/admin/brand-clouds`
- `GET /v1/admin/brand-clouds/{id}`
- `GET /v1/admin/audit-events?subject_type=brand_cloud`

Provide the platform-admin credentials from ignored operator secrets. The
verifier reads `ACCOUNT_MANAGER_VERIFY_PLATFORM_ADMIN_EMAIL` and
`ACCOUNT_MANAGER_VERIFY_PLATFORM_ADMIN_PASSWORD`, or falls back to
`ACCOUNT_MANAGER_BOOTSTRAP_PLATFORM_ADMIN_EMAIL` and
`ACCOUNT_MANAGER_BOOTSTRAP_PLATFORM_ADMIN_PASSWORD` when those are already
loaded for deployment bootstrap.

## Observability

The API serves Prometheus text metrics at `GET /metrics/prometheus`. This
endpoint is intended for private Prometheus scraping by the central Video Cloud
observability stack on the private VPC IP and app port. Public nginx returns
404 for this path; public verification should continue to use `GET /v1/health`
and authenticated API smoke checks.

```sh
set -a
. linode_deploy/secrets/account-manager-public-staging.env
. linode_deploy/state/rtk-account-manager-staging.env
. linode_deploy/secrets/account-manager-platform-admin.env
set +a

ACCOUNT_MANAGER_VERIFY_BRAND_CLOUD=1 \
ACCOUNT_MANAGER_VERIFY_BRAND_CLOUD_NAME="Realtek Connect+ Verify $(date -u +%Y%m%d%H%M%S)" \
  linode_deploy/scripts/verify-public-vm.sh
```

The platform-admin login request/response is held in temporary files only. Do
not persist platform-admin bearer tokens in `.artifacts/`.

Verification artifacts are written under `.artifacts/linode-account-manager-verify/`
and must remain untracked.

## Backup

```sh
export WORKSPACE=/path/to/rtk_cloud_workspace
export DEPLOY_SECRETS_DIR="$WORKSPACE/.secrets/staging/linode/account-manager"

linode_deploy/scripts/backup-public-postgres.sh
```

The backup script creates a PostgreSQL custom-format dump and a checksum under
`.artifacts/linode-account-manager-backups/`. Raw dumps are not committed.

## Staging Limitations

The default public VM profile is appropriate for staging smoke and admin
integration:

- `AUTH_TOKEN_DELIVERY=log` for log-only smoke, or `smtp` when staging should
  send verification, sign-in, and password-reset email.
- `AUTH_TOKEN_BASE_URL=<admin-console-origin>` when `AUTH_TOKEN_DELIVERY=smtp`.
- `CROSS_SERVICE_BROKER=log`
- `OIDC_ENABLED=false`
- SMTP optional for log-only smoke; required for `AUTH_TOKEN_DELIVERY=smtp`.

Production-like identity, notification, and cross-service lifecycle testing must
set SMTP/OIDC/broker configuration explicitly before deployment.
