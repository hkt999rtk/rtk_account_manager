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
The implementation stays aligned with the `contracts/` submodule for provisioning and cross-service channel boundaries.
The local provisioning and worker flow, including the `log` broker adapter runbook, is documented in `docs/PROVISIONING_EVENT_WORKERS_RUNBOOK.md`.
The optional local Keycloak/OIDC login flow is documented in `docs/KEYCLOAK_LOCAL_RUNBOOK.md`; normal local development does not require Keycloak.
Private-cloud deployment packaging, systemd templates, migration/upgrade/rollback, and backup/restore operations are documented in `docs/PRIVATE_CLOUD_DEPLOYMENT_RUNBOOK.md`; reference deploy assets live under `deploy/`.
The local auth verification and password reset delivery adapter is `AUTH_TOKEN_DELIVERY=log`; generated one-time tokens are written to the API server log for dev/test use until a production mail or SMS adapter replaces it. Quota-raise approval and decline notifications use the SMTP mail path when `SMTP_HOST` and `SMTP_FROM` are configured, and otherwise fall back to the local log adapter for dev/test.
Set `CROSS_SERVICE_BROKER=azure_eventhubs` plus `AZURE_EVENTHUB_CONNECTION_STRING` to run the workers against Azure Event Hubs instead of the local `log` adapter. The inbox worker persists Azure consumer checkpoints at `.state/azure_eventhubs/<stream>__<consumer-group>.json` by default; set `AZURE_EVENTHUB_CHECKPOINT_FILE` to override that path.

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
