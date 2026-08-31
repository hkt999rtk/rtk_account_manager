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
   evidence when the referenced resources already exist. Send Mail HTTP
   configuration is checked explicitly; missing cross-service channel settings
   are recorded as `SKIP`.
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
The implementation stays aligned with the canonical workspace contracts repo through the `docs/rtk_cloud_contracts_doc/` symlink for provisioning and cross-service channel boundaries.
The local/staging app-certificate smoke-test authorization, idempotency, audit, and production boundary are documented in `docs/developer-pki-test-bundles.md`.
The local provisioning and worker flow, including the `log` broker adapter runbook, is documented in `docs/PROVISIONING_EVENT_WORKERS_RUNBOOK.md`.
The optional local Keycloak/OIDC login flow is documented in `docs/KEYCLOAK_LOCAL_RUNBOOK.md`; normal local development does not require Keycloak.
Private-cloud deployment packaging, systemd templates, migration/upgrade/rollback, and backup/restore operations are documented in `docs/PRIVATE_CLOUD_DEPLOYMENT_RUNBOOK.md`; reference deploy assets live under `deploy/`.
The service logging migration to `rtk_cloud_logger` zap and central journald forwarding is documented in `docs/SERVICE_LOGGING_MIGRATION.md`.
Linode staging runtime is K8s-only and is operated from the workspace; see `docs/linode-staging-k8s.md`.
Auth verification, email sign-in, password reset, invitations, owner transfer,
and quota-decision notifications are transactionally written to the encrypted
PostgreSQL outbox. Run `rtk-account-manager-email-worker` to deliver every
message through the Send Mail HTTP API. Configure `AUTH_TOKEN_BASE_URL`,
`SENDMAIL_HTTP_BASE_URL`, `SENDMAIL_HTTP_BEARER_TOKEN`,
`SENDMAIL_HTTP_TIMEOUT`, and `EMAIL_OUTBOX_ENCRYPTION_KEY`. The worker sends
`to`, `subject`, `text`, and `html` to `POST /send`; production requires HTTPS.
The workspace-level staging email E2E uses the same HTTP delivery path and IMAP
only to verify the received message.

The in-progress ownership-handoff preview/confirmation adapter uses paired
`BILLING_HANDOFF_BASE_URL` and a dedicated `BILLING_HANDOFF_TOKEN` (at least 32
characters, not reused from other service credentials). The origin must use HTTPS;
literal loopback HTTP is permitted for isolated tests only. Leave both unset to
keep unavailable financial evidence fail-closed. Configuring this adapter does
**not** enable complete transfers: trusted initial eligibility, producer hold/drain
workers and automatic delivery of the implemented owner commit/finalization
protocol are still required. See
`docs/MULTICLOUD_IMPLEMENTATION_PROGRESS.md` before enabling any runtime rollout.

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
