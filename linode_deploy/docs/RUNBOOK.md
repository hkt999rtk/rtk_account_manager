# Account Manager Linode Deploy Runbook

This runbook defines the operator-local Linode deployment flow for
`rtk_account_manager`. GitHub CI remains artifact-only. It must not SSH into
Linode hosts or run deployment.

## Topology

Account Manager runs on a dedicated VPC-only VM in the existing video-cloud
Linode staging stack:

| Role | Placement |
| --- | --- |
| `edge` | Existing public/VPC edge VM, terminates HTTPS. |
| `infra` | Existing VPC-only infra VM, hosts PostgreSQL. |
| `account-manager` | New VPC-only VM at `10.42.1.20`, API on `18081`. |

Public route:

```text
account-manager-staging.realtekconnect.com
  -> edge nginx
  -> 10.42.1.20:18081
```

Account Manager uses a separate Postgres database and role:

```text
database: rtk_account_manager
role:     rtk_account_manager
```

Do not reuse the `video_cloud` database or database role.

## Operator Secrets

Create a local ignored secrets file:

```sh
mkdir -p linode_deploy/secrets
install -m 0600 /dev/null linode_deploy/secrets/account-manager-staging.env
```

Minimum keys:

```sh
ACCOUNT_MANAGER_DB_PASSWORD='<db-role-password>'
JWT_ACCESS_SECRET='<access-token-secret>'
JWT_REFRESH_SECRET='<refresh-token-secret>'
```

OIDC keys when SSO smoke is enabled:

```sh
OIDC_ISSUER_URL='https://<keycloak>/realms/<realm>'
OIDC_CLIENT_ID='rtk-account-manager'
OIDC_CLIENT_SECRET='<oidc-client-secret>'
```

Optional keys:

```sh
SMTP_HOST=
SMTP_USERNAME=
SMTP_PASSWORD=
SMTP_FROM=
AZURE_EVENTHUB_CONNECTION_STRING=
```

Secrets must stay out of git, issue comments, generated reports, and shell
history where possible.

## Plan

Review the manifest:

```sh
go run ./linode_deploy/cmd/linode-deploy plan \
  --config linode_deploy/configs/account-manager-staging.yaml
```

The plan must show:

- Account Manager VM `account-manager-staging-account-manager` at `10.42.1.20`
- edge route for `account-manager-staging.realtekconnect.com`
- edge -> account-manager `18081/tcp`
- account-manager -> infra `5432/tcp`
- DB migration gate before runtime restart
- workers disabled by default

## Deploy Script

Run a dry-run first:

```sh
linode_deploy/scripts/deploy-staging.sh \
  --release vX.Y.Z \
  --stack account-manager-staging \
  --config linode_deploy/configs/account-manager-staging.yaml \
  --secrets-file linode_deploy/secrets/account-manager-staging.env \
  --report .artifacts/account-manager-linode-deploy.md
```

Use a local bundle only for debug:

```sh
linode_deploy/scripts/deploy-staging.sh \
  --release vX.Y.Z \
  --release-bundle dist/rtk_account_manager-vX.Y.Z.tar.gz
```

The script refuses `latest`. Operators must choose an explicit release.

## Live Execution Notes

The current deploy artifact validates and records the ordered Linode deployment
plan. The operator performs live Linode/API/SSH mutation using the generated
report until a dedicated Account Manager Linode executor is wired.

Live steps from the report:

1. Confirm `10.42.1.20` is unused.
2. Create or update the `account-manager` VPC-only VM.
3. Update Linode firewall rules for edge, infra, and account-manager.
4. Add the edge nginx vhost for `account-manager-staging.realtekconnect.com`.
5. Create/verify `rtk_account_manager` Postgres role and database on infra.
6. Install the release under `/opt/rtk-account-manager`.
7. Write `/etc/rtk-account-manager/account-manager.env` with mode `0600`.
8. Confirm DB backup marker at `/var/lib/rtk-account-manager/last-db-backup-ok`.
9. Run `rtk-account-manager-migrate.service`.
10. Restart API and cleanup timer.
11. Start outbox/inbox workers only when explicitly enabled.
12. Run health and smoke checks.

## Runtime Defaults

```sh
DATABASE_URL='postgres://rtk_account_manager:<secret>@10.42.1.30:5432/rtk_account_manager?sslmode=disable'
PORT=18081
AUTH_TOKEN_DELIVERY=log
CROSS_SERVICE_BROKER=log
OIDC_REDIRECT_URL='https://account-manager-staging.realtekconnect.com/v1/auth/oidc/keycloak/callback'
```

## Verification

Private VM health:

```sh
curl -fsS http://127.0.0.1:18081/v1/health
```

Edge/public health:

```sh
curl -fsS https://account-manager-staging.realtekconnect.com/v1/health
```

Optional smoke checks:

- login with existing smoke user
- read smoke organization
- read smoke device
- read provisioning state
- OIDC provider discovery when OIDC is enabled

## Evidence

The report must include release, manifest digest, planned service changes,
health/smoke status, and redacted env inventory. It must not include DSNs,
JWT secrets, OIDC secrets, SMTP passwords, broker credentials, or raw tokens.
