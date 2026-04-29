# Testing Guide

## Test Layers

The backend uses three test layers:

| Layer | Command | Purpose |
| --- | --- | --- |
| Unit tests | `make test` | Fast package-level checks that do not require Postgres unless `TEST_DATABASE_URL` is set. |
| Integration tests | `make integration-test` | Runs the API, store, migrations, auth, authorization, and device lifecycle tests against Postgres. |
| Full report | `make test-report` | Runs formatting, integration-aware tests, coverage, build validation, and writes `docs/TEST_REPORT.md`. |

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

## Correctness Versus Coverage

Coverage only proves that statements executed. Correctness comes from assertions that check externally visible behavior and database state. The maintained report includes a correctness validation section that maps tests back to behavior groups from `docs/SPEC.md`.

When adding a feature, do not rely on line coverage alone. Add assertions for:

- HTTP status codes and response bodies.
- Database side effects and constraints.
- Role and organization boundary behavior.
- Token lifecycle behavior.
- Outbox/inbox idempotency and retry/dead-letter behavior.
- OpenAPI response compatibility when API payloads change.

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
| Broker adapters | Local broker tests are deterministic; Azure Event Hubs tests do not run unless explicitly configured. |

The v2 test report must distinguish coverage from correctness by naming representative tests for each group above.

## CI

GitHub Actions runs `make test-report` on pushes to `main` and on pull requests. The workflow starts a Postgres service, runs the report, builds all packages, and uploads the report artifacts.
