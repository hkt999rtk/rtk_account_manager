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
- `contracts/PROVISION.md`
- `contracts/CROSS_SERVICE_CHANNEL.md`

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
- Optional SMTP and cross-service channel gaps are explicitly marked `SKIP`.

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
| Observability | Service logs, health checks, worker exit status, DB backup status, and dead-letter evidence. |
| Backup target | Storage independent from the primary runtime host. |

Production-like acceptance:

- Deployment uses immutable build artifacts or pinned container images.
- Database migration and rollback boundaries are reviewed before promotion.
- API, workers, and cleanup jobs have service supervision.
- Backup and restore are rehearsed in a test environment.
- Quota decision notifications are configured through SMTP or explicitly run in
  log-only mode for evaluation.
- Platform-admin operations are restricted to approved operator accounts.

## Deployment Package

The service package should contain:

| Path | Purpose |
| --- | --- |
| `rtk-account-manager` | API server binary built from `./cmd/server`. |
| `rtk-account-manager-migrate` | Migration binary built from `./cmd/migrate`. |
| `rtk-account-manager-outbox-worker` | Outbox worker binary built from `./cmd/outbox-worker`. |
| `rtk-account-manager-inbox-worker` | Inbox worker binary built from `./cmd/inbox-worker`. |
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
go build -trimpath -o dist/rtk-account-manager-cleanup-tokens ./cmd/cleanup-tokens
```

For native installs, place binaries under `/opt/rtk-account-manager/bin/` and
copy the environment file to `/etc/rtk-account-manager/account-manager.env`.
The environment file must be readable only by the service user and root.

Build an immutable release bundle with:

```sh
VERSION=v0.1.0 make release
```

This writes `dist/rtk_account_manager-$VERSION.tar.gz`. GitHub Actions
publishes the same tarball through `.github/workflows/release.yml`.

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
(`workflow_dispatch`) and deploys an existing GitHub Release version. Configure
the staging environment with:

| GitHub setting | Purpose |
| --- | --- |
| `ACCOUNT_MANAGER_RUNTIME_ENV` secret | Full `/etc/rtk-account-manager/account-manager.env` content. |
| `ACCOUNT_MANAGER_DEPLOY_PREFIX` variable | Optional override; defaults to `/opt/rtk-account-manager`. |
| `ACCOUNT_MANAGER_DEPLOY_ETC_DIR` variable | Optional override; defaults to `/etc/rtk-account-manager`. |
| `ACCOUNT_MANAGER_DEPLOY_STATE_DIR` variable | Optional override; defaults to `/var/lib/rtk-account-manager`. |

Before running a deployment that can apply migrations, confirm a fresh database
backup and create the deploy gate marker:

```sh
sudo install -d -o rtk-account-manager -g rtk-account-manager /var/lib/rtk-account-manager
printf '%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" | \
  sudo tee /var/lib/rtk-account-manager/last-db-backup-ok >/dev/null
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
9. Upload deployment evidence with service status and redacted env keys.

Default restart units are:

```text
rtk-account-manager.service rtk-account-manager-cleanup-tokens.timer
```

Enable `rtk-account-manager-outbox-worker.service` and
`rtk-account-manager-inbox-worker.service` in `restart_units` only when the
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
| `AUTH_TOKEN_DELIVERY` | Verification/reset token delivery adapter. | `log` is currently supported by the server entrypoint. |
| `EMAIL_VERIFICATION_TTL` | Email verification OTP lifetime. | `30m` |
| `PASSWORD_RESET_TTL` | Password reset OTP lifetime. | `30m` |
| `OTP_RESEND_INTERVAL` | Minimum resend interval. | `60s` |
| `OTP_MAX_ATTEMPTS` | Max wrong OTP attempts before lockout. | `5` |
| `SIGNUP_CAPTCHA_REQUIRED` | Require a captcha token in signup payload. | `false` unless enabled. |
| `SIGNUP_DISPOSABLE_DOMAINS` | Comma-separated disposable email domain denylist override. | Built-in denylist when unset. |

### SMTP And Quota Notifications

Quota-raise approval and decline notifications use SMTP when `SMTP_HOST` and
`SMTP_FROM` are configured. Otherwise the service falls back to a log adapter.

| Variable | Purpose | Secret |
| --- | --- | --- |
| `SMTP_HOST` | SMTP host or host:port. | No |
| `SMTP_PORT` | SMTP port when not embedded in `SMTP_HOST`. | No |
| `SMTP_USERNAME` | SMTP username. | Usually yes |
| `SMTP_PASSWORD` | SMTP password. | Yes |
| `SMTP_FROM` | Sender address. | No |

Evaluation deployments may use log-only notifications if that is recorded in
the readiness evidence. Production-like deployments should configure SMTP or an
operator-approved mail relay.

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
- SMTP password or relay credentials
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
- Use an audited SQL migration or controlled DBA procedure.
- Record who approved the bootstrap and when.
- Do not share operator passwords or JWTs in tickets.

Example controlled SQL:

```sql
UPDATE users
SET platform_admin = true, updated_at = now()
WHERE email = '<operator-email>';
```

Operator endpoints:

| Endpoint | Purpose |
| --- | --- |
| `GET /v1/admin/metrics` | Evaluation signup, verification, quota-raise, and live quota utilization snapshot. |
| `POST /v1/admin/quota-raise-requests/{requestId}/approve` | Approve a pending quota raise. |
| `POST /v1/admin/quota-raise-requests/{requestId}/decline` | Decline a pending quota raise. |

Quota approval/decline should trigger requester notification through SMTP when
configured, or through logs in evaluation mode.

## Upgrade

1. Identify source commit, artifact checksum, contracts submodule commit, and
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
7. Record failed version, rollback version, migration state, and residual data
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
- contracts submodule commit
- migration list and latest applied migration
- `/v1/health` result
- auth/login smoke result
- organization/device read smoke result
- provisioning/readiness smoke result or explicit `SKIP`
- SMTP mode: configured or log-only `SKIP`
- cross-service channel mode: enabled, disabled, or `SKIP`
- backup timestamp and restore-drill reference
- worker service status when lifecycle channel is enabled

Evidence must not include passwords, JWTs, DSNs, SMTP credentials, Event Hubs
connection strings, or customer payloads beyond intentionally redacted IDs.
