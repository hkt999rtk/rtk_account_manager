# Private Cloud Deployment Runbook

This runbook defines the account-manager deployment package for Realtek
Connect+ private-cloud evaluation and production-like environments.

It is service-local guidance for `rtk_account_manager`. Product-level component
placement and support boundaries remain owned by the workspace private-cloud
BOM. Do not store deployment secrets in this repository, issue bodies, or
runbook notes.

Source inputs:

- `rtk_cloud_workspace/docs/private-cloud-deployment.md`
- `rtk_cloud_workspace/docs/implementation-gap-backlog.md`
- `rtk_cloud_workspace/docs/core-platform-gap-roadmap.md`
- `docs/SPEC.md`
- `docs/rtk_cloud_contracts_doc/PROVISION.md`
- `docs/rtk_cloud_contracts_doc/CROSS_SERVICE_CHANNEL.md`

## Deployment Profiles

### Single-Node Evaluation

Use this profile for demos, engineering validation, and private-cloud
workshops. It optimizes for fast setup over high availability.

The current staging recommendation is to deploy account manager on
`video-cloud-cd.local`, the same private-cloud host used by `rtk_video_cloud`.
This keeps account/video provisioning and readiness checks in one environment
while preserving service and database isolation.

Recommended placement:

| Component | Evaluation placement |
| --- | --- |
| Account Manager API | `systemd` service on `video-cloud-cd.local`, bound to `127.0.0.1:18081`. |
| Postgres | Shared local PostgreSQL cluster, separate `rtk_account_manager` database and role. |
| Outbox worker | Optional separate process when provisioning lifecycle commands are enabled. |
| Inbox worker | Optional separate process when video-side events are consumed locally. |
| Cleanup tokens | `systemd` timer or scheduled job. |
| Reverse proxy/TLS | Optional for localhost demos; required when exposed outside the host. |
| Broker | `log` adapter for local flows; production broker may be skipped with an explicit `SKIP` note. |

Evaluation acceptance:

- API starts from an immutable binary or pinned container image.
- Migrations complete before API traffic is accepted.
- `GET /v1/health` returns `200`.
- A smoke user can log in, list organizations, and read an existing device.
- Provisioning/readiness evidence is collected when a provisioned device exists.
- Send Mail HTTP configuration is verified and cross-service channel gaps are explicitly marked `SKIP`.

### Production-Like Private Deployment

Use this profile for customer pilots and supportable private deployments.

Required platform services:

| Component | Production-like expectation |
| --- | --- |
| Account Manager API | Dedicated supervised service with restart policy, logs, and pinned artifact version. |
| Postgres | Managed or self-managed Postgres with backup, restore, maintenance, and monitoring. |
| Reverse proxy/TLS | Operator-owned HTTPS termination, access logs, request limits, and CORS policy. |
| Secrets storage | Operator-owned secret manager or root-owned environment file outside git. |
| Broker | Azure Event Hubs or approved equivalent when account/video lifecycle channel is enabled. |
| Observability | `rtk_cloud_logger` zap service logs through journald/forwarder, health checks, worker exit status, DB backup status, and dead-letter evidence. |
| Backup target | Storage independent from the primary runtime host. |

Production-like acceptance:

- Deployment uses immutable build artifacts or pinned container images.
- Database migration and rollback boundaries are reviewed before promotion.
- API, workers, and cleanup jobs have service supervision.
- Backup and restore are rehearsed in a test environment.
- Quota decision notifications are delivered through the Send Mail HTTP outbox worker.
- Platform-admin operations are restricted to approved operator accounts.

## Deployment Package

The service package should contain:

| Path | Purpose |
| --- | --- |
| `rtk-account-manager` | API server binary built from `./cmd/server`. |
| `rtk-account-manager-migrate` | Migration binary built from `./cmd/migrate`. |
| `rtk-account-manager-outbox-worker` | Outbox worker binary built from `./cmd/outbox-worker`. |
| `rtk-account-manager-inbox-worker` | Inbox worker binary built from `./cmd/inbox-worker`. |
| `rtk-account-manager-email-worker` | Send Mail HTTP outbox worker built from `./cmd/email-worker`. |
| `rtk-account-manager-email-outbox-admin` | Safe email queue list/requeue command built from `./cmd/email-outbox-admin`. |
| `rtk-account-manager-cleanup-tokens` | Token cleanup binary built from `./cmd/cleanup-tokens`. |
| `migrations/` | SQL migration files installed beside the binaries' working directory. |
| `deploy/account-manager.env.example` | Non-secret environment key inventory for operator-owned env files. |
| `deploy/systemd/*.service` | Reference native Linux units. |

Recommended build commands:

```sh
mkdir -p dist
go build -trimpath -o dist/rtk-account-manager ./cmd/server
go build -trimpath -o dist/rtk-account-manager-migrate ./cmd/migrate
go build -trimpath -o dist/rtk-account-manager-outbox-worker ./cmd/outbox-worker
go build -trimpath -o dist/rtk-account-manager-inbox-worker ./cmd/inbox-worker
go build -trimpath -o dist/rtk-account-manager-email-worker ./cmd/email-worker
go build -trimpath -o dist/rtk-account-manager-email-outbox-admin ./cmd/email-outbox-admin
go build -trimpath -o dist/rtk-account-manager-cleanup-tokens ./cmd/cleanup-tokens
```

For native installs, place binaries under `/opt/rtk-account-manager/bin/` and
copy the environment file to `/etc/rtk-account-manager/account-manager.env`.
The environment file must be readable only by the service user and root.

Build an immutable release bundle with:

```sh
VERSION=v0.1.0 make release
```

This writes `dist/rtk_account_manager-$VERSION.tar.gz`. The release workflow
also publishes formal release objects to Linode Object Storage using the
workspace artifact governance shape:

```text
releases/rtk_account_manager-$VERSION/$VERSION.tar.gz
releases/rtk_account_manager-$VERSION/$VERSION.tar.gz.sha256
releases/rtk_account_manager-$VERSION/manifest.json
```

GitHub Actions artifacts and GitHub Releases are debug/mirror surfaces. Linode
Object Storage is the durable release store.

Developer self-check with the AWS CLI as an S3-compatible client for Linode
Object Storage:

```sh
aws s3 ls "s3://$LINODE_OBJ_BUCKET/releases/rtk_account_manager-$VERSION/" \
  --endpoint-url "$LINODE_OBJ_ENDPOINT"

mkdir -p ".artifacts/release-download/$VERSION"
aws s3 cp "s3://$LINODE_OBJ_BUCKET/releases/rtk_account_manager-$VERSION/$VERSION.tar.gz" \
  ".artifacts/release-download/$VERSION/$VERSION.tar.gz" \
  --endpoint-url "$LINODE_OBJ_ENDPOINT"
aws s3 cp "s3://$LINODE_OBJ_BUCKET/releases/rtk_account_manager-$VERSION/$VERSION.tar.gz.sha256" \
  ".artifacts/release-download/$VERSION/$VERSION.tar.gz.sha256" \
  --endpoint-url "$LINODE_OBJ_ENDPOINT"
aws s3 cp "s3://$LINODE_OBJ_BUCKET/releases/rtk_account_manager-$VERSION/manifest.json" \
  ".artifacts/release-download/$VERSION/manifest.json" \
  --endpoint-url "$LINODE_OBJ_ENDPOINT"

scripts/verify-linode-release-objects.sh "$VERSION" ".artifacts/release-download/$VERSION"
```

The verifier checks manifest fields, checksum consistency, and the repo-owned
`deploy/check-release.sh` bundle contract.

## Staging CD On `video-cloud-cd.local`

Use `video-cloud-cd.local` as the staging deploy target for account manager.
Do not use it for heavy CI. The deployment runner should only run manual deploy
jobs and short smoke checks.

Runner requirements:

| Setting | Value |
| --- | --- |
| Repository | `hkt999rtk/rtk_account_manager` |
| Runner label | `account-manager-cd` |
| Host | `video-cloud-cd.local` |
| Sudo | Passwordless sudo for installing under `/opt`, `/etc`, `/var/lib`, and restarting account-manager units. |

Staging placement:

| Resource | Value |
| --- | --- |
| Install prefix | `/opt/rtk-account-manager` |
| Config dir | `/etc/rtk-account-manager` |
| State dir | `/var/lib/rtk-account-manager` |
| Service user/group | `rtk-account-manager` |
| API port | `18081` |
| Database | `rtk_account_manager` |
| Database role | `rtk_account_manager` |

Create the database and role outside the deploy workflow:

```sql
CREATE ROLE rtk_account_manager LOGIN PASSWORD '<operator-owned-secret>';
CREATE DATABASE rtk_account_manager OWNER rtk_account_manager;
```

The runtime DSN must point at the account-manager database, never the
`video_cloud` database:

```sh
DATABASE_URL='postgres://rtk_account_manager:<secret>@127.0.0.1:5432/rtk_account_manager?sslmode=disable'
PORT=18081
```

The deploy workflow is `.github/workflows/deploy-local.yml`. It is manual
(`workflow_dispatch`) and deploys an existing release version. Configure
the staging environment with:

| GitHub setting | Purpose |
| --- | --- |
| `ACCOUNT_MANAGER_RUNTIME_ENV` secret | Full `/etc/rtk-account-manager/account-manager.env` content. |
| `ACCOUNT_MANAGER_DEPLOY_PREFIX` variable | Optional override; defaults to `/opt/rtk-account-manager`. |
| `ACCOUNT_MANAGER_DEPLOY_ETC_DIR` variable | Optional override; defaults to `/etc/rtk-account-manager`. |
| `ACCOUNT_MANAGER_DEPLOY_STATE_DIR` variable | Optional override; defaults to `/var/lib/rtk-account-manager`. |
| `ACCOUNT_MANAGER_SMOKE_BASE_URL` variable | Optional smoke base URL; defaults to `http://127.0.0.1:18081`. |
| `ACCOUNT_MANAGER_SMOKE_EMAIL` secret | Existing smoke user email for optional readiness smoke. |
| `ACCOUNT_MANAGER_SMOKE_PASSWORD` secret | Existing smoke user password for optional readiness smoke. |
| `ACCOUNT_MANAGER_SMOKE_ORG_ID` secret | Existing organization ID readable by the smoke user. |
| `ACCOUNT_MANAGER_SMOKE_DEVICE_ID` secret | Existing device ID readable by the smoke user. |

Manual deploy inputs:

| Input | Purpose |
| --- | --- |
| `version` | Existing release version to deploy. |
| `restart_units` | Space-separated account-manager units to restart after migration. |
| `verify` | Run package verification after restart. |
| `run_readiness_smoke` | Run login, organization read, device read, and provisioning/readiness smoke when smoke secrets are configured. |
| `restore_drill_reference` | Operator reference for the latest restore drill, or `SKIP:<reason>`. |
| `email_delivery` | Must be `sendmail_http`. |
| `broker_mode` | `enabled`, `disabled`, or `SKIP`. |

`SKIP:<reason>` values are acceptable only when the omission is deliberate and
reviewable, for example `SKIP:evaluation-no-broker` or
`SKIP:restore-drill-not-required-for-demo`. Do not use `SKIP` to hide a failed
backup, failed smoke check, or missing production-like dependency.

Before running a deployment that can apply migrations, confirm a fresh database
backup and create the deploy gate marker:

```sh
sudo install -d -o rtk-account-manager -g rtk-account-manager /var/lib/rtk-account-manager
printf '%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" | \
  sudo tee /var/lib/rtk-account-manager/last-db-backup-ok >/dev/null
```

Release artifacts are published to Linode Object Storage through its
S3-compatible API. The release workflow may use AWS CLI as an S3-compatible
client, but the durable artifact backend is Linode Object Storage, not AWS
storage.

Expected Object Storage prefix:

```text
releases/rtk_account_manager-<version>/
```

Required objects:

```text
releases/rtk_account_manager-<version>/<version>.tar.gz
releases/rtk_account_manager-<version>/<version>.tar.gz.sha256
releases/rtk_account_manager-<version>/manifest.json
```

Developer self-check:

```sh
aws s3 ls "s3://$LINODE_OBJ_BUCKET/releases/rtk_account_manager-$VERSION/" \
  --endpoint-url "$LINODE_OBJ_ENDPOINT"
aws s3 cp "s3://$LINODE_OBJ_BUCKET/releases/rtk_account_manager-$VERSION/manifest.json" - \
  --endpoint-url "$LINODE_OBJ_ENDPOINT"
```

Deploy sequence:

1. Download the release tarball.
2. Validate the release bundle with `deploy/check-release.sh`.
3. Install binaries, migrations, systemd units, and env examples with
   `deploy/install.sh`.
4. Preserve or install `/etc/rtk-account-manager/account-manager.env`.
5. Require the backup marker before migrations.
6. Run `rtk-account-manager-migrate.service`.
7. Restart selected runtime units.
8. Run `/opt/rtk-account-manager/verify.sh`.
9. Collect deployment evidence:
   - backup marker freshness from `/var/lib/rtk-account-manager/last-db-backup-ok`
   - restore-drill reference from the manual deploy input
   - `/v1/health` smoke result
   - optional login, organization, device, and provisioning/readiness smoke result
   - Send Mail HTTP configuration and cross-service broker mode
   - concise systemd status summaries
   - redacted runtime env-key inventory
10. Upload concise readiness evidence plus raw diagnostics artifacts.

Default restart units are:

```text
rtk-account-manager.service rtk-account-manager-cleanup-tokens.timer
```

Enable `rtk-account-manager-outbox-worker.service`,
`rtk-account-manager-inbox-worker.service`, and
`rtk-account-manager-email-worker.service` in `restart_units` only when the
cross-service lifecycle channel is deliberately enabled for the environment.

## Environment Variables

Use `deploy/account-manager.env.example` as the operator-facing key inventory.
It intentionally contains placeholders only.

### Required

| Variable | Purpose | Secret |
| --- | --- | --- |
| `DATABASE_URL` | Postgres DSN for API, workers, migrations, and cleanup jobs. | Yes |
| `JWT_ACCESS_SECRET` | Access-token signing secret. | Yes |
| `JWT_REFRESH_SECRET` | Refresh-token signing secret. | Yes |

### API Runtime

| Variable | Purpose | Default / notes |
| --- | --- | --- |
| `PORT` | API bind port. | `8080` by code default; staging deploy uses `18081` to avoid `rtk_video_cloud` on `18080`. |
| `ACCESS_TOKEN_TTL` | Access token lifetime. | `15m` |
| `REFRESH_TOKEN_TTL` | Refresh token lifetime. | `720h` |
| `EMAIL_VERIFICATION_TTL` | Email verification OTP lifetime. | `30m` |
| `PASSWORD_RESET_TTL` | Password reset OTP lifetime. | `30m` |
| `OTP_RESEND_INTERVAL` | Minimum resend interval. | `60s` |
| `OTP_MAX_ATTEMPTS` | Max wrong OTP attempts before lockout. | `5` |
| `SIGNUP_DISPOSABLE_DOMAINS` | Comma-separated disposable email domain denylist override. | Built-in denylist when unset. |

### Keycloak/OIDC SSO

OIDC is disabled by default. Enable it only after the external Keycloak/OIDC
client exists and the redirect URL is registered with the provider. Account
manager remains the authorization source of truth; Keycloak proves identity
only.

| Variable | Purpose | Secret |
| --- | --- | --- |
| `OIDC_ENABLED` | Enables public OIDC provider discovery, login, and callback routes. | No |
| `OIDC_PROVIDER_ID` | Stable provider id used in `/v1/auth/oidc/:providerId/...`, for example `keycloak`. | No |
| `OIDC_PROVIDER_NAME` | Display name returned by provider discovery. | No |
| `OIDC_ISSUER_URL` | Expected Keycloak/OIDC issuer URL. | No |
| `OIDC_CLIENT_ID` | OIDC client id registered for account manager. | No |
| `OIDC_CLIENT_SECRET` | OIDC client secret for the env-configured provider and `env:OIDC_CLIENT_SECRET` references. | Yes |
| `OIDC_REDIRECT_URL` | Exact backend callback URL registered with Keycloak. | No |
| `OIDC_SCOPES` | Space-separated scopes. | No; default `openid email profile` |
| `OIDC_AUTO_LINK_EMAIL` | Allows verified OIDC email to link to an existing enabled local user. | No; default `false` |

Platform-admin-managed providers store only `client_secret_ref` values such as
`env:OIDC_CLIENT_SECRET`. Do not put raw client secrets in API payloads,
database rows, issue comments, reports, or deployment evidence.

### Send Mail HTTP And Quota Notifications

Auth-token, owner-transfer, and quota-decision notifications use an encrypted
PostgreSQL outbox. Run `rtk-account-manager-email-worker` alongside the API.
Temporary Send Mail HTTP failure is retried independently and does not roll back the API
mutation.

| Variable | Purpose | Secret |
| --- | --- | --- |
| `SENDMAIL_HTTP_BASE_URL` | Credential-free HTTPS origin for the Send Mail service. | No |
| `SENDMAIL_HTTP_BEARER_TOKEN` | Bearer credential for the Send Mail service. | Yes |
| `SENDMAIL_HTTP_TIMEOUT` | HTTP request timeout. | No; default `15s` |
| `AUTH_TOKEN_BASE_URL` | Browser origin used to build token links. | No |
| `EMAIL_OUTBOX_ENCRYPTION_KEY` | Base64-encoded 32-byte AES-GCM key. | Yes |
| `EMAIL_OUTBOX_POLL_INTERVAL` | Worker polling interval. | No; default `5s` |
| `EMAIL_OUTBOX_BATCH_SIZE` | Maximum rows claimed per poll. | No; default `20` |
| `EMAIL_OUTBOX_MAX_ATTEMPTS` | Delivery attempt limit. | No; default `8` |
| `EMAIL_OUTBOX_RETRY_BASE` | Initial retry delay. | No; default `30s` |
| `EMAIL_OUTBOX_RETRY_MAX` | Retry ceiling. | No; default `30m` |

All deployments must configure Send Mail HTTP and must not reuse or rotate the
encryption key until the queue has been drained.

### Cross-Service Channel

| Variable | Purpose | Default / notes |
| --- | --- | --- |
| `CROSS_SERVICE_BROKER` | Broker adapter. | `log` for local/evaluation; `azure_eventhubs` when enabled. |
| `ACCOUNT_VIDEO_COMMANDS_STREAM` | Account-to-video lifecycle command stream. | `account.video.commands` |
| `VIDEO_ACCOUNT_EVENTS_STREAM` | Video-to-account lifecycle event stream. | `video.account.events` |
| `CROSS_SERVICE_CONSUMER_GROUP` | Inbox worker consumer group. | `rtk_account_manager` |
| `CROSS_SERVICE_MAX_ATTEMPTS` | Retry/dead-letter threshold. | `5` |
| `CROSS_SERVICE_POLL_INTERVAL` | Worker poll/retry interval. | `5s` |
| `AZURE_EVENTHUB_CONNECTION_STRING` | Azure Event Hubs connection string. | Secret; required for Azure adapter. |
| `AZURE_EVENTHUB_CHECKPOINT_FILE` | Durable checkpoint file path. | Persist outside ephemeral runtime dirs. |

When the lifecycle channel is disabled, do not run the outbox/inbox workers and
record the channel as `SKIP` in deployment evidence.

## Secret Handling

Never commit real values for:

- `DATABASE_URL`
- JWT signing secrets
- OIDC client secrets and provider token material
- email delivery credentials
- Azure Event Hubs connection strings
- future cloud-provider credentials

Accepted storage patterns:

- customer/platform secret manager
- root-owned env file outside the repo, mode `0600`
- deployment-system environment secrets

Keep issue comments and sign-off artifacts redacted. Record key names and
storage location categories, not values.

## Migration Sequence

Run migrations as a one-shot step before starting a new API version.

1. Put the new artifact on the host, but keep the previous artifact available.
2. Confirm a recent Postgres backup exists.
3. Stop or drain API traffic if the migration notes require it.
4. Run:

   ```sh
   /opt/rtk-account-manager/bin/rtk-account-manager-migrate
   ```

5. Confirm `schema_migrations` contains all files in `migrations/`.
6. Start or restart the API.
7. Run health and smoke checks.

Reference SQL:

```sql
SELECT version, applied_at
FROM schema_migrations
ORDER BY version;
```

Do not manually edit `schema_migrations` except during a documented restore.

## Start And Health Checks

Native `systemd` profile:

```sh
id -u rtk-account-manager >/dev/null 2>&1 || \
  sudo useradd --system --home-dir /var/lib/rtk-account-manager --shell /usr/sbin/nologin rtk-account-manager
sudo install -d -o rtk-account-manager -g rtk-account-manager /opt/rtk-account-manager/bin
sudo install -d -o rtk-account-manager -g rtk-account-manager /var/lib/rtk-account-manager
sudo install -d -m 0750 /etc/rtk-account-manager
sudo install -m 0755 dist/rtk-account-manager /opt/rtk-account-manager/bin/
sudo install -m 0755 dist/rtk-account-manager-migrate /opt/rtk-account-manager/bin/
sudo install -m 0755 dist/rtk-account-manager-outbox-worker /opt/rtk-account-manager/bin/
sudo install -m 0755 dist/rtk-account-manager-inbox-worker /opt/rtk-account-manager/bin/
sudo install -m 0755 dist/rtk-account-manager-email-worker /opt/rtk-account-manager/bin/
sudo install -m 0755 dist/rtk-account-manager-email-outbox-admin /opt/rtk-account-manager/bin/
sudo install -m 0755 dist/rtk-account-manager-cleanup-tokens /opt/rtk-account-manager/bin/
sudo rsync -a --delete migrations/ /opt/rtk-account-manager/migrations/
sudo install -m 0600 deploy/account-manager.env.example /etc/rtk-account-manager/account-manager.env
sudo install -m 0644 deploy/systemd/*.service /etc/systemd/system/
sudo install -m 0644 deploy/systemd/*.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl start rtk-account-manager-migrate.service
sudo systemctl enable --now rtk-account-manager.service
sudo systemctl enable --now rtk-account-manager-cleanup-tokens.timer
```

Health check:

```sh
curl -fsS http://127.0.0.1:18081/v1/health
```

Expected response:

```json
{"status":"ok"}
```

## Smoke Checks

The deploy workflow always records `/v1/health`. Set
`run_readiness_smoke=true` and configure the smoke secrets to also record
login, organization read, device read, and provisioning/readiness checks.

If `run_readiness_smoke=false`, the readiness report records those checks as
`SKIP:disabled`. If any smoke secret is missing, it records
`SKIP:missing-smoke-secret`. If `jq` is unavailable on the deploy runner, the
workflow records `SKIP:jq-missing` for token-dependent checks. These explicit
SKIP values are intended to make evidence gaps visible without committing raw
responses, JWTs, passwords, or customer payloads.

Use an existing smoke user and existing organization/device in production-like
deployments. Do not create customer data solely for smoke checks unless that is
part of an evaluation environment.

Login:

```sh
LOGIN_RESPONSE=$(curl -fsS -X POST "$ACCOUNT_MANAGER_BASE_URL/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$SMOKE_EMAIL\",\"password\":\"$SMOKE_PASSWORD\"}")
ACCESS_TOKEN=$(printf '%s' "$LOGIN_RESPONSE" | jq -r '.tokens.access_token')
```

Organization read:

```sh
curl -fsS "$ACCOUNT_MANAGER_BASE_URL/v1/orgs/$SMOKE_ORG_ID" \
  -H "Authorization: Bearer $ACCESS_TOKEN" | jq .
```

Device read:

```sh
curl -fsS "$ACCOUNT_MANAGER_BASE_URL/v1/orgs/$SMOKE_ORG_ID/devices/$SMOKE_DEVICE_ID" \
  -H "Authorization: Bearer $ACCESS_TOKEN" | jq .
```

Provisioning/readiness read, when the selected device has lifecycle evidence:

```sh
curl -fsS "$ACCOUNT_MANAGER_BASE_URL/v1/orgs/$SMOKE_ORG_ID/devices/$SMOKE_DEVICE_ID/provisioning" \
  -H "Authorization: Bearer $ACCESS_TOKEN" | jq .
```

If the device is intentionally unprovisioned, record this check as `SKIP` with
the HTTP status and reason.

## Platform-Admin Operations

Platform-admin API access is controlled by `users.platform_admin=true`.

Bootstrap rules:

- Grant the flag only to named operator accounts.
- Prefer deployment bootstrap through
  `ACCOUNT_MANAGER_BOOTSTRAP_PLATFORM_ADMIN_EMAIL` and
  `ACCOUNT_MANAGER_BOOTSTRAP_PLATFORM_ADMIN_PASSWORD` when creating the first
  root account for a private cloud.
- Use an audited SQL migration or controlled DBA procedure only when env-based
  bootstrap is unavailable.
- Record who approved the bootstrap and when.
- Do not share operator passwords or JWTs in tickets.

Example service env bootstrap:

```sh
ACCOUNT_MANAGER_BOOTSTRAP_PLATFORM_ADMIN_EMAIL=admin@realtekconnect.com
ACCOUNT_MANAGER_BOOTSTRAP_PLATFORM_ADMIN_PASSWORD='<secret-from-vault>'
```

Example controlled SQL:

```sql
UPDATE users
SET platform_admin = true, updated_at = now()
WHERE email = '<operator-email>';
```

Brand-cloud backend verification after bootstrap:

```sh
ACCESS_TOKEN="$(
  curl -fsS "$ACCOUNT_MANAGER_BASE_URL/v1/auth/login" \
    -H 'Content-Type: application/json' \
    -d '{"email":"admin@realtekconnect.com","password":"<secret-from-vault>"}' |
  jq -r '.tokens.access_token'
)"

curl -fsS "$ACCOUNT_MANAGER_BASE_URL/v1/admin/brand-clouds" \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Realtek Connect+","metadata":{"public_name":"Realtek Connect+"}}' |
  jq .

curl -fsS "$ACCOUNT_MANAGER_BASE_URL/v1/admin/brand-clouds" \
  -H "Authorization: Bearer $ACCESS_TOKEN" | jq .

curl -fsS "$ACCOUNT_MANAGER_BASE_URL/v1/admin/audit-events?subject_type=brand_cloud" \
  -H "Authorization: Bearer $ACCESS_TOKEN" | jq .
```

Operator endpoints:

| Endpoint | Purpose |
| --- | --- |
| `GET /v1/admin/metrics` | Evaluation signup, verification, quota-raise, and live quota utilization snapshot. |
| `POST /v1/admin/quota-raise-requests/{requestId}/approve` | Approve a pending quota raise. |
| `POST /v1/admin/quota-raise-requests/{requestId}/decline` | Decline a pending quota raise. |

Quota approval/decline triggers requester notification through the Send Mail
HTTP outbox worker.

## User Cache Operations

When `ACCOUNT_MANAGER_USER_CACHE_ENABLED=true`, the API uses Redis-compatible
read-through cache for platform/developer, brand-cloud, and end-user profile
and auth lookups. Postgres remains the source of truth. Redis cache records do
not use TTL; normal account-manager write paths refresh or delete affected cache
entries after successful Postgres writes. Redis miss or Redis outage does not
block user queries because the API falls back to Postgres and refills Redis when
possible.

Required runtime settings:

| Variable | Purpose |
| --- | --- |
| `ACCOUNT_MANAGER_USER_CACHE_ENABLED` | Enables the Redis-compatible user cache. |
| `ACCOUNT_MANAGER_USER_CACHE_ADDR` | Redis/Valkey host and port. |
| `ACCOUNT_MANAGER_USER_CACHE_PREFIX` | Key prefix; keep separate per environment. |

Maintenance commands:

```sh
/app/rtk-account-manager-user-cache rebuild
/app/rtk-account-manager-user-cache inspect --email owner@example.com
/app/rtk-account-manager-user-cache delete --user-id '<user-id>'
```

Run `rebuild` after Redis flush/replacement or after direct Postgres repair for
platform users in the `users` table. Run `delete` when a single platform user
projection should be forced to refill on the next query. Brand-cloud and end-user
cache entries are covered by the API Store decorator read-through behavior, but
this maintenance command does not rebuild those tables; use normal read-through
refill or delete the relevant Redis keys directly if those records were repaired
outside the Account Manager write path.

The key families under `ACCOUNT_MANAGER_USER_CACHE_PREFIX` are:

| Subject | Profile key | Email index | Auth/login key |
| --- | --- | --- | --- |
| Platform/developer user | `:platform:id:{user_id}` | `:platform:email:{email}` | `:platform:auth:{user_id}` |
| Brand-cloud user | `:brand_cloud:id:{brand_cloud_user_id}` | `:brand_cloud:email:{tenant_slug}:{email}` | `:brand_cloud:auth:{brand_cloud_user_id}` |
| End user | `:end_user:id:{end_user_id}` | `:end_user:email:{email}` | `:end_user:auth:{end_user_id}` |

The cache stores auth projections, including password hashes, so the Redis
endpoint must remain private and covered by the same secret-network controls as
the API runtime.

## Upgrade

1. Identify source commit, artifact checksum, workspace contracts repository commit, and
   migration list.
2. Confirm CI passed for the exact artifact source.
3. Confirm backup freshness and restore target availability.
4. Deploy to evaluation/staging first.
5. Run migrations.
6. Restart API and selected workers.
7. Run health and smoke checks.
8. Inspect logs for token delivery, quota notification, outbox, inbox, and
   dead-letter errors.
9. Promote to production-like environment after sign-off.
10. Keep previous artifact and env key inventory available for rollback.

## Rollback

Rollback binary artifacts only when database compatibility is confirmed.

1. Stop or drain API and workers.
2. Deploy the previous known-good artifact.
3. Keep the current database if migrations are backward-compatible.
4. If migrations are not backward-compatible, restore the last known-good
   database backup to a replacement database and repoint `DATABASE_URL`.
5. Start API and workers.
6. Run health and smoke checks.
7. If Redis user cache is enabled, run `rtk-account-manager-user-cache rebuild`
   for platform users or delete affected brand-cloud/end-user keys before
   serving traffic after direct database restore.
8. Record failed version, rollback version, migration state, and residual data
   risk.

Do not delete migration rows to force binary rollback.

## Backup

Minimum backup set:

| Data | Backup guidance |
| --- | --- |
| Account manager Postgres | Managed database snapshot or scheduled `pg_dump` with encrypted storage. |
| Runtime env key inventory | Redacted key list and release manifest; no secret values. |
| Secret material | Operator secret-manager backup or rotation procedure. |
| Worker checkpoint file | Back up `AZURE_EVENTHUB_CHECKPOINT_FILE` when Azure inbox worker is enabled. |
| Logs/evidence | Retain service logs, smoke outputs, and dead-letter inspection output according to customer policy. |

Example logical backup shape:

```sh
pg_dump "$DATABASE_URL" --format=custom --file account-manager-$(date +%Y%m%d%H%M%S).dump
```

Store dumps in operator-owned encrypted backup storage, not in this repository.

## Restore

1. Provision a replacement Postgres database.
2. Restore the selected dump:

   ```sh
   pg_restore --dbname "$RESTORE_DATABASE_URL" --clean --if-exists account-manager.dump
   ```

3. Point a staging account-manager deployment at the restored database.
4. Run migrations only if the target artifact expects newer schema.
5. Run health and smoke checks.
6. Verify platform-admin metrics and representative org/device reads.
7. Promote the restored database only after operator sign-off.

## Operational Evidence

Attach redacted evidence to deployment sign-off:

- artifact version and source commit
- workspace contracts repository commit
- migration list and latest applied migration
- `/v1/health` result
- auth/login smoke result
- organization/device read smoke result
- provisioning/readiness smoke result or explicit `SKIP`
- email delivery: `sendmail_http`
- cross-service channel mode: enabled, disabled, or `SKIP`
- backup timestamp and restore-drill reference
- worker service status when lifecycle channel is enabled

Evidence must not include passwords, JWTs, DSNs, email delivery credentials, Event Hubs
connection strings, or customer payloads beyond intentionally redacted IDs.
