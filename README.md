# RTK Account Manager

Backend account and device manager for organization-scoped users and registry-only IoT devices.

## Local Development

1. Copy environment defaults:

   ```sh
   cp .env.example .env
   ```

   The server and maintenance commands load `.env` automatically when it is present.

2. Start Postgres:

   ```sh
   make db-up
   ```

3. Run migrations:

   ```sh
   make migrate
   ```

4. Start the API:

   ```sh
   make run
   ```

5. Clean expired or revoked refresh tokens when needed:

   ```sh
   make cleanup-tokens
   ```

6. Inspect or requeue lifecycle outbox/inbox rows when worker failures need investigation:

   ```sh
   go run ./cmd/lifecycle-admin outbox list
   go run ./cmd/lifecycle-admin inbox list
   ```

7. Run tests:

   ```sh
   make test
   ```

8. Run Postgres-backed integration tests:

   ```sh
   make integration-test
   ```

   These tests require the Docker Compose Postgres service to be running.

9. Generate the maintained test report:

   ```sh
   make test-report
   ```

   The report is written to `docs/TEST_REPORT.md`. Report artifacts are written under `reports/`.
   This command also requires the Postgres service from `make db-up`.

10. Generate a private-cloud readiness evidence artifact from an already running API:

   ```sh
   READINESS_SMOKE_EMAIL='owner@example.com' \
   READINESS_SMOKE_PASSWORD='password123' \
   READINESS_SMOKE_ORG_ID='<existing-org-id>' \
   READINESS_SMOKE_DEVICE_ID='<existing-device-id>' \
   go run ./cmd/readiness-smoke -output reports/readiness-smoke.json
   ```

   The readiness smoke is read-only. It records service version input, health,
   migration status, login, organization/device reads, and provisioning/readiness
   evidence when the referenced resources already exist. Missing optional SMTP
   or cross-service channel settings are recorded as explicit `SKIP` checks.
   Use `go run ./cmd/readiness-smoke -dry-run` to validate configuration without
   network or database calls.

11. Stop local services:

   ```sh
   make db-down
   ```

The API listens on `http://localhost:8080` by default. The OpenAPI contract is in `openapi.yaml`.
List endpoints accept `limit` and `offset` query parameters and return pagination metadata.
Testing policy and report maintenance are documented in `docs/TESTING.md`.
The current v2 provisioning and account/video event-channel surface is documented in `docs/SPEC.md`; rollout tracking and dependency history live in `docs/PROVISIONING_AND_EVENT_CHANNEL_PLAN.md`.
The proposed provider-neutral commercial account, balance ledger, payment
intent, and automatic top-up architecture is documented in
`docs/PAYMENT_ABSTRACTION_AND_AUTO_TOPUP.md`. It is documentation-only and does
not describe an enabled production payment route yet.
The implementation stays aligned with the `docs/rtk_cloud_contracts_doc/` submodule for provisioning and cross-service channel boundaries.
The local provisioning and worker flow, including the `log` broker adapter runbook, is documented in `docs/PROVISIONING_EVENT_WORKERS_RUNBOOK.md`.
The optional local Keycloak/OIDC login flow is documented in `docs/KEYCLOAK_LOCAL_RUNBOOK.md`; normal local development does not require Keycloak.
Private-cloud deployment packaging, systemd templates, migration/upgrade/rollback, and backup/restore operations are documented in `docs/PRIVATE_CLOUD_DEPLOYMENT_RUNBOOK.md`; reference deploy assets live under `deploy/`.
The service logging migration to `rtk_cloud_logger` zap and central journald forwarding is documented in `docs/SERVICE_LOGGING_MIGRATION.md`.
Linode staging runtime is K8s-only and is operated from the workspace; see `docs/linode-staging-k8s.md`.
Auth verification, email sign-in, and password reset tokens use `AUTH_TOKEN_DELIVERY`.
Set `AUTH_TOKEN_DELIVERY=log` for dev/test to write generated one-time tokens to
the API server log. Set `AUTH_TOKEN_DELIVERY=smtp` plus `SMTP_HOST`,
`SMTP_PORT`, `SMTP_USERNAME`, `SMTP_PASSWORD`, `SMTP_FROM`,
`SMTP_FROM_NAME`, `SMTP_ENCRYPTION=starttls`,
`EMAIL_OUTBOX_ENCRYPTION_KEY`, and `AUTH_TOKEN_BASE_URL` to enqueue
verification, login activation, password reset, owner-transfer, and quota
decision email. Run `rtk-account-manager-email-worker` to deliver the durable
PostgreSQL outbox. `AUTH_TOKEN_BASE_URL` must point at the Admin Console
browser origin so messages can link to `/signup/verify`, `/login/activate`,
and `/reset-password`.

Alternatively, set `AUTH_TOKEN_DELIVERY=sendmail_http`,
`SENDMAIL_HTTP_BASE_URL`, `SENDMAIL_HTTP_BEARER_TOKEN`, and
`SENDMAIL_HTTP_TIMEOUT` to deliver the same outbox messages through the
Send Mail HTTP API. The worker sends `to`, `subject`, `text`, and `html` to
`POST /send`; the service owns the SMTP sender identity. Production rejects
log delivery, incomplete transport configuration, non-STARTTLS SMTP, and
non-HTTPS Send Mail origins.

### Local SMTP + IMAP signup E2E

The real-mail signup test is an explicit local-only check. It starts an
ephemeral PostgreSQL container, Account Manager API and email worker, and the
Admin Console; signs up through Chromium; reads the newly delivered message
through IMAP without changing its read state; opens `/signup/verify`; and checks
the one-time token, verified account state, session, and password login.

Install the Admin Console web dependencies and Chromium once, then run:

```sh
cd ../rtk_cloud_admin/web
npm ci
npm run e2e:install
cd ../../rtk_account_manager
RUN_LIVE_EMAIL_E2E=1 make test-email-signup-e2e
```

The command reads SMTP and IMAP test credentials from `~/.env`. IMAP requires
`IMAP_SERVER`, `IMAP_EMAIL_ADDR`, `IMAP_EMAIL_PASSWORD`, `IMAP_EMAIL_PORT`,
`IMAP_EMAIL_SECURITY`, and `IMAP_EMAIL_FOLDER`. It also uses the test-only SMTP
aliases `SMTP_SERVER`, `SMTP_PORT`, `SMTP_EMAIL_ADDR`, `SMTP_EMAIL_PASSWORD`,
and `SMTP_ENCRYPTION`, mapping them to the service's canonical variables only
inside the test process.

The live test is not part of normal CI or `make test`. It must not be run
against a shared deployment. It does not mark or delete mailbox messages and
does not print credentials, tokens, complete verification URLs, or complete
mailbox addresses. A timeout normally indicates SMTP delivery delay, an IMAP
folder mismatch, or TLS/authentication failure. Run
`make test-email-signup-helper` for the offline MIME and URL parser tests.
Set `CROSS_SERVICE_BROKER=azure_eventhubs` plus `AZURE_EVENTHUB_CONNECTION_STRING` to run the workers against Azure Event Hubs instead of the local `log` adapter. The inbox worker persists Azure consumer checkpoints at `.state/azure_eventhubs/<stream>__<consumer-group>.json` by default; set `AZURE_EVENTHUB_CHECKPOINT_FILE` to override that path.

Set `ACCOUNT_MANAGER_USER_CACHE_ENABLED=true` to enable the Redis-compatible
read-through user cache. The cache keeps Postgres as the source of truth, uses
no TTL, and falls back to Postgres when Redis misses or is unavailable. Configure
`ACCOUNT_MANAGER_USER_CACHE_ADDR` and `ACCOUNT_MANAGER_USER_CACHE_PREFIX` to
select the Redis/Valkey endpoint and key namespace. The cache stores auth
projections, including password hashes, so the Redis endpoint must remain
private to the service network.

The API Store decorator currently covers platform/developer users, brand-cloud
users, and end users for profile and login/auth read paths. Platform user keys
use `:platform:id:`, `:platform:email:`, and `:platform:auth:` under the prefix;
brand-cloud users use `:brand_cloud:id:`, `:brand_cloud:email:`, and
`:brand_cloud:auth:`; end users use `:end_user:id:`, `:end_user:email:`, and
`:end_user:auth:`.

Use the maintenance command when Redis is flushed or a direct platform-user
database repair must be reflected in cache. This command operates on platform
users in the `users` table; brand-cloud and end-user caches are repaired by
normal read-through refill or by deleting their Redis keys directly:

```sh
go run ./cmd/user-cache rebuild
go run ./cmd/user-cache inspect --email owner@example.com
go run ./cmd/user-cache delete --user-id '<user-id>'
```

## Smoke Test

After starting Postgres and the API, create a user, organization, and device:

```sh
REGISTER_RESPONSE=$(curl -sS -X POST http://localhost:8080/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{
    "email": "owner@example.com",
    "password": "password123",
    "display_name": "Owner",
    "organization_name": "Owner Org"
  }')

ACCESS_TOKEN=$(echo "$REGISTER_RESPONSE" | jq -r '.tokens.access_token')
ORG_ID=$(echo "$REGISTER_RESPONSE" | jq -r '.organization.id')

curl -sS -X POST "http://localhost:8080/v1/orgs/$ORG_ID/devices" \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "Lab Camera",
    "category": "ip_camera",
    "serial_number": "CAM-001",
    "metadata": {"location": "lab"}
  }'

curl -sS "http://localhost:8080/v1/orgs/$ORG_ID/devices" \
  -H "Authorization: Bearer $ACCESS_TOKEN"
```

Run migrations manually with explicit environment variables:

```sh
DATABASE_URL='postgres://rtk:rtk_password@localhost:5432/rtk_account_manager?sslmode=disable' \
JWT_ACCESS_SECRET='dev-access-secret' \
JWT_REFRESH_SECRET='dev-refresh-secret' \
go run ./cmd/migrate
```
