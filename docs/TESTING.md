# Testing Guide

## Test Layers

The backend uses three test layers:

| Layer | Command | Purpose |
| --- | --- | --- |
| Unit tests | `make test` | Fast package-level checks that do not require Postgres unless `TEST_DATABASE_URL` is set. |
| Integration tests | `make integration-test` | Runs the API, store, migrations, auth, authorization, and device lifecycle tests against Postgres. |
| Full report | `make test-report` | Runs formatting, integration-aware tests, coverage, build validation, and writes `docs/TEST_REPORT.md`. |
| Readiness smoke | `make readiness-smoke` | Emits a redacted private-cloud readiness artifact against a running deployment. |

`make integration-test` and `make test-report` expect Postgres to be reachable at `TEST_DATABASE_URL`.
For the default local setup, start it first with `make db-up`.
`make test-report` now fails fast with that prerequisite message instead of overwriting `docs/TEST_REPORT.md` with a misleading low-coverage report.

## Coverage Policy

`make test-report` measures statement coverage with:

```sh
go test -json ./... -coverpkg=./internal/... -coverprofile=reports/coverage.out -covermode=atomic
```

The default minimum total coverage is `80.0%`. Override it only when intentionally changing the project baseline:

```sh
COVERAGE_THRESHOLD=82.0 make test-report
```

Coverage scope is `./internal/...` because command entry points under `cmd/*` are validated by `go build ./...`.

## Required Test Updates

Update tests whenever changing:

- Authentication, JWT, refresh-token, logout, or disabled-user behavior.
- Organization membership, owner/admin/member authorization, or last-owner rules.
- Device lifecycle behavior, status changes, soft-delete behavior, or organization scoping.
- Provisioning, deactivation, outbox, inbox, broker adapter, or cross-service projection behavior.
- SQL migrations, database constraints, or timestamp triggers.
- OpenAPI request/response contracts.
- Configuration loading or local development commands.

## Report Artifacts

`make test-report` creates:

| Path | Purpose |
| --- | --- |
| `docs/TEST_REPORT.md` | Human-readable current test report. |
| `reports/test-events.json` | Machine-readable Go test event stream. |
| `reports/coverage.out` | Go coverage profile. |
| `reports/coverage.txt` | Function-level coverage summary. |
| `reports/coverage.html` | HTML coverage report. |
| `reports/gofmt.txt` | Files that need formatting, empty when formatting passes. |
| `reports/build.txt` | Build output, empty when build passes. |
| `reports/test-cases.md` | Passing test case list extracted from Go JSON events. |

`reports/` is ignored by git. Commit `docs/TEST_REPORT.md` when intentionally refreshing the maintained report.

## Readiness Smoke Artifact

`go run ./cmd/readiness-smoke` produces a JSON artifact that is safe to attach to
private-cloud deployment sign-off. It redacts credentials and token material and
does not create, update, or delete account-manager data.

Configure it with existing deployment resources:

```sh
ACCOUNT_MANAGER_BASE_URL='https://account-manager.example.internal' \
ACCOUNT_MANAGER_VERSION='2026.05.07+build' \
DATABASE_URL='postgres://...' \
READINESS_SMOKE_EMAIL='owner@example.com' \
READINESS_SMOKE_PASSWORD='...' \
READINESS_SMOKE_ORG_ID='...' \
READINESS_SMOKE_DEVICE_ID='...' \
go run ./cmd/readiness-smoke -output reports/readiness-smoke.json
```

The checks cover service version input, `/v1/health`, applied SQL migrations,
login, organization/device readability, and provisioning/readiness evidence for
the selected device. Disabled optional features such as SMTP or the
cross-service channel are emitted as explicit `SKIP` checks rather than hidden.
Use `go run ./cmd/readiness-smoke -dry-run` for configuration-only validation.

## Correctness Versus Coverage

Coverage only proves that statements executed. Correctness comes from assertions that check externally visible behavior and database state. The maintained report includes a correctness validation section that maps tests back to behavior groups from `docs/SPEC.md`.

When adding a feature, do not rely on line coverage alone. Add assertions for:

- HTTP status codes and response bodies.
- Database side effects and constraints.
- Role and organization boundary behavior.
- Token lifecycle behavior.
- Outbox/inbox idempotency and retry/dead-letter behavior.
- OpenAPI response compatibility when API payloads change.

For the current Claim Token and readiness scope, the maintained report must
name representative tests for Claim Token persistence, Claim Token resolve API
policy, registry-only readiness behavior, and OpenAPI response validation. These
groups are required because they close contract gaps that coverage percentage
alone cannot prove correct.

## V2 Provisioning And Event Channel Test Matrix

When implementing the v2 provisioning/event-channel milestone, the maintained report must add evidence for these behavior groups:

| Group | Required evidence |
| --- | --- |
| Provisioning API | Creating a provision operation writes `device_operations` and `device_message_outbox` in one transaction. |
| Deactivation API | Creating a deactivation operation writes the correct operation and outbox command. |
| Authorization | `owner` and `admin` may initiate lifecycle operations; `member` may only read provisioning state. |
| Device scoping | Cross-organization and missing devices are rejected without leaking resource existence. |
| Disabled devices | Disabled devices cannot be provisioned. |
| Operation idempotency | Reusing `operation_id` with the same payload returns the existing operation. |
| Operation conflicts | Reusing `operation_id` with a different payload returns `409 Conflict`. |
| Message validation | Required envelope fields, supported `schema_version`, message type, stream, and partition key are validated. |
| Outbox worker | Publish success marks rows `published`; transient failure retries; exhausted failure becomes `dead_lettered`. |
| Inbox worker | Duplicate `message_id` is ignored safely and does not repeat side effects. |
| Projection idempotency | Replayed events with the same `operation_id` do not corrupt final state. |
| Metadata merge | `video_cloud_*` projections preserve unrelated device metadata. |
| Activation projection | `DeviceProvisionSucceeded` updates video metadata but does not set account-manager `status=online`. |
| Online projection | `DeviceOnlineChanged` updates account-manager `status` and `last_seen_at`. |
| Failure projection | Provision/deactivation failures record stable error metadata and operation failure state. |
| Claim Token persistence | Claim Tokens are stored as hashes, resolve once, reject expired/already-claimed/cross-organization tokens, and only support categories accepted by the product policy. |
| Claim Token admin workflow | Platform-admin generated/imported/revoked Claim Tokens never persist raw token values; generated raw tokens are returned once; revoked tokens cannot resolve. |
| Claim resolve API | `POST /v1/orgs/:orgId/devices/claim/resolve` returns provisioning input, enforces owner/admin authorization, rejects member access, emits machine-readable errors, and does not publish lifecycle outbox work. |
| Claim resolve retryability | Claim resolve errors include stable `retryable` and `resolution_action` hints for invalid, expired, already-claimed, cross-organization, unsupported, quota, and service-unavailable failures. |
| Registry-only readiness | `GET /provisioning` returns account-side readiness with nullable `operation` for enabled and disabled registry-only devices while preserving `404` for missing devices. |
| Readiness failure attribution | Failed/dead-lettered provisioning and deactivation readiness responses include `readiness.failure` with layer, source state, retryability, error fields, operation id, and occurrence time. |
| Admin visibility | Platform-admin quota request list/show and audit event list endpoints enforce admin-only access, pagination, and filters. |
| OpenAPI contract validation | Claim resolve, registry-only provisioning-state, provisioned provisioning-state, provisioning, and deactivation responses validate against `openapi.yaml`. |
| Broker adapters | Local broker tests are deterministic; Azure Event Hubs tests do not run unless explicitly configured. |

The v2 test report must distinguish coverage from correctness by naming representative tests for each group above.

## CI

GitHub Actions runs `make test-report` on pushes to `main` and on pull requests. The workflow starts a Postgres service, runs the report, builds all packages, and uploads the report artifacts.
