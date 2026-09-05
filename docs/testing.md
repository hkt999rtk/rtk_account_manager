# Testing Guide

## Test Layers

The backend uses these test layers:

| Layer | Command | Purpose |
| --- | --- | --- |
| Unit tests | `make test` | Fast package-level checks that do not require Postgres unless `TEST_DATABASE_URL` is set. |
| Integration tests | `make integration-test` | Runs the API, store, migrations, auth, authorization, and device lifecycle tests against Postgres. |
| Full report | `make test-report` | Runs formatting, integration-aware tests, coverage, build validation, and writes `docs/test_report.md`. |
| Race tests | `make test-race` | Runs Go's race detector against packages that do not need the shared integration database. |
| Repeatability smoke | `make test-repeat` | Runs selected unit packages with `-shuffle=on -count=3` to catch order coupling and flakes. |
| Fuzz smoke | `make fuzz-smoke` | Runs short seeded fuzz checks for strict JSON and contract parser behavior. |
| Readiness smoke | `make readiness-smoke` | Emits a redacted private-cloud readiness artifact against a running deployment. |
| Email signup helper | `make test-email-signup-helper` | Offline IMAP MIME, verification URL, and transport-policy helper tests. |
| Staging email signup E2E | Workspace `scripts/staging_email_signup_e2e.py` | Send Mail HTTP → IMAP → activation verification; never part of required CI. |

`make integration-test` and `make test-report` expect Postgres to be reachable at `TEST_DATABASE_URL`.
For the default local setup, start it first with `make db-up`.
`make test-report` now fails fast with that prerequisite message instead of overwriting `docs/test_report.md` with a misleading low-coverage report.

Integration tests share one Postgres database and use an advisory lock through
`internal/testutil.LockIntegrationDatabase`. Do not make these tests parallel
unless each test receives an isolated database/schema or the truncation strategy
is changed first.

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

The project intentionally keeps the minimum at `80.0%` while adding stronger
correctness gates. Raising coverage should be a separate baseline change, not a
side effect of feature work.

## Required Test Updates

Update tests whenever changing:

- Authentication, JWT, refresh-token, logout, or disabled-user behavior.
- Keycloak/OIDC provider resolution, login callback, state/nonce handling, identity linking, or external identity management.
- Organization membership, ACL permission checks, owner/admin/member
  compatibility, or last-owner rules.
- Device lifecycle behavior, status changes, soft-delete behavior, or organization scoping.
- Provisioning, deactivation, outbox, inbox, broker adapter, or cross-service projection behavior.
- User unprovision, Claim Token reuse policy, device binding release, or platform-admin support override behavior.
- SQL migrations, database constraints, or timestamp triggers.
- OpenAPI request/response contracts.
- Configuration loading or local development commands.

## Report Artifacts

`make test-report` creates:

| Path | Purpose |
| --- | --- |
| `docs/test_report.md` | Human-readable current test report. |
| `reports/test-events.json` | Machine-readable Go test event stream. |
| `reports/coverage.out` | Go coverage profile. |
| `reports/coverage.txt` | Function-level coverage summary. |
| `reports/coverage.html` | HTML coverage report. |
| `reports/gofmt.txt` | Files that need formatting, empty when formatting passes. |
| `reports/build.txt` | Build output, empty when build passes. |
| `reports/test-cases.md` | Passing test case list extracted from Go JSON events. |
| `reports/correctness-gates.md` | Required behavior gates and representative passing tests. |
| `reports/test-repeat.txt` | `make test-repeat` output when that target runs. |
| `reports/test-race.txt` | `make test-race` output when that target runs. |
| `reports/fuzz-smoke-*.txt` | `make fuzz-smoke` output when that target runs. |

`reports/` is ignored by git. Commit `docs/test_report.md` when intentionally refreshing the maintained report.

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
the selected device. Disabled optional features such as the
cross-service channel are emitted as explicit `SKIP` checks rather than hidden.
Use `go run ./cmd/readiness-smoke -dry-run` for configuration-only validation.

The live email signup E2E uses a disposable local PostgreSQL container and
local Account Manager/Admin Console processes while connecting to the Send Mail HTTP and
IMAP endpoints configured in `~/.env`. It captures IMAP `UIDNEXT` before signup,
reads later messages with `BODY.PEEK[]`, and leaves mailbox state unchanged.
The runner redacts mailbox addresses and never emits credentials, tokens,
complete verification links, or message bodies. `RUN_LIVE_EMAIL_E2E=1` is
required to prevent accidental external mail traffic; do not enable this test
in normal PR CI or point it at a shared deployment.

## Correctness Versus Coverage

Coverage only proves that statements executed. Correctness comes from assertions that check externally visible behavior and database state. The maintained report includes correctness gates and a correctness validation section that map tests back to behavior groups from `docs/spec.md`.

When adding a feature, do not rely on line coverage alone. Add assertions for:

- HTTP status codes and response bodies.
- Database side effects and constraints.
- Role and organization boundary behavior.
- Token lifecycle behavior.
- Outbox/inbox idempotency and retry/dead-letter behavior.
- OpenAPI response compatibility when API payloads change.

`make test-report` fails when a required correctness gate is missing a passing
representative test. When renaming or replacing a representative test, update
`scripts/test-report.sh` in the same change so the executable gate and the
human-readable report stay aligned.

For the current Claim Token and readiness scope, the maintained report must
name representative tests for Claim Token persistence, Claim Token resolve API
policy, registry-only readiness behavior, and OpenAPI response validation. These
groups are required because they close contract gaps that coverage percentage
alone cannot prove correct.

For the Keycloak/OIDC SSO scope, the maintained report must name representative
tests for provider CRUD, state/nonce replay protection, token validation,
unknown/disabled/unverified user rejection, auto-link policy, local login
compatibility, current-user identity management, and secret redaction. These
groups are required because OIDC correctness depends on security policy and
contract behavior, not just endpoint line coverage.

## Keycloak/OIDC SSO Test Matrix

The Keycloak/OIDC implementation uses fake OIDC/JWKS servers in automated tests.
Do not make CI depend on a live Keycloak container; the Docker Compose Keycloak
profile is only for local manual integration checks.

| Group | Required evidence |
| --- | --- |
| Provider persistence | `identity_providers`, `user_identities`, and `oidc_login_states` persistence supports CRUD, uniqueness, multiple enabled providers, hashed state/nonce, and replay rejection. |
| Provider admin CRUD | Platform-admin-only create/list/show/update/disable enforces `env:VAR_NAME` secret references, rejects non-admin access, supports multiple enabled providers, and emits audit events. |
| Provider discovery and login | Disabled OIDC returns no providers and rejects login; enabled OIDC redirects with state and nonce. |
| Callback success | Successful callback validates token claims, links to an existing local user under policy, returns the existing Account Manager token response shape, and keeps local email/password login working. |
| Callback rejection policy | Unknown users, disabled linked users, replayed state, invalid nonce, invalid issuer/audience/signature/expiry, unexpected signing method, and unverified email are rejected with typed errors. |
| Identity management | Current users can list/unlink their own identities, cannot access another user's identity, and disabled users cannot manage identities. |
| Secret redaction | Raw client secrets, Keycloak access tokens, refresh tokens, state, and nonce are not persisted, returned, logged in typed errors, or written to the maintained report. |
| OpenAPI contract validation | Public OIDC endpoints, current-user identities, and platform-admin identity-provider endpoints validate representative responses against `openapi.yaml`. |

## V2 Provisioning And Event Channel Test Matrix

When implementing the v2 provisioning/event-channel milestone, the maintained report must add evidence for these behavior groups:

| Group | Required evidence |
| --- | --- |
| Provisioning API | Creating a provision operation writes `device_operations` and `device_message_outbox` in one transaction. |
| Deactivation API | Creating a deactivation operation writes the correct operation and outbox command. |
| Authorization | `owner`, `admin`, and `member` may resolve claims and initiate organization-scoped lifecycle operations; outsiders and disabled users are rejected. |
| Device scoping | Cross-organization and missing devices are rejected without leaking resource existence. |
| Authorization matrix | Owner/admin/member/platform-admin/outsider/disabled-user behavior is checked across device, claim, lifecycle, quota, and audit endpoints. |
| ACL persistence and admin workflow | ACL schema invariants, permission catalog seed, system roles, scoped role assignments, read-only observer write denial, platform-admin ACL management APIs, external group mappings, and ACL audit events are checked. |
| Disabled devices | Disabled devices cannot be provisioned. |
| Operation idempotency | Reusing `operation_id` with the same payload returns the existing operation. |
| Operation conflicts | Reusing `operation_id` with a different payload returns `409 Conflict`. |
| Message validation | Required envelope fields, supported `schema_version`, message type, stream, and partition key are validated. |
| Parser fuzz seeds | Strict JSON, envelope, and API bind parser seeds cover malformed JSON, unknown fields, wrong stream/message type, wrong partition key, and multiple JSON values. |
| Outbox worker | Publish success marks rows `published`; transient failure retries; exhausted failure becomes `dead_lettered`. |
| Inbox worker | Duplicate `message_id` is ignored safely and does not repeat side effects. |
| Projection idempotency | Replayed events with the same `operation_id` do not corrupt final state. |
| Metadata merge | `video_cloud_*` projections preserve unrelated device metadata. |
| Activation projection | `DeviceProvisionSucceeded` updates video metadata but does not set account-manager `status=online`. |
| Online projection | `DeviceOnlineChanged` updates account-manager `status` and `last_seen_at`. |
| Failure projection | Provision/deactivation failures record stable error metadata and operation failure state. |
| Claim Token persistence | Claim Tokens are stored as hashes, resolve once, reject expired/already-claimed/cross-organization tokens, and only support categories accepted by the product policy. |
| Claim Token admin workflow | Platform-admin generated/imported/revoked Claim Tokens never persist raw token values; generated raw tokens are returned once; revoked tokens cannot resolve. |
| Claim Token transfer/reclaim | Platform-admin-only override endpoints require operator reason/evidence, preserve normal claim-resolve rejection, reject repeated override transitions, and emit audit events. |
| Device user unprovision | Member/admin/owner can release a normal device binding for resale, old users can no longer operate the released device, the original Claim Token remains one-time, and a fresh Claim Token can onboard the same factory identity. |
| Device unprovision override | Platform-admin override requires reason/evidence, rejects non-platform users, and writes audit evidence. |
| Claim resolve API | `POST /v1/orgs/:orgId/devices/claim/resolve` returns provisioning input with `service_options`, allows owner/admin/member organization members, rejects outsiders, emits machine-readable errors, and does not publish lifecycle outbox work. |
| Claim resolve retryability | Claim resolve errors include stable `retryable` and `resolution_action` hints for invalid, expired, already-claimed, cross-organization, unsupported, quota, and service-unavailable failures. |
| Registry-only readiness | `GET /provisioning` returns account-side readiness with nullable `operation` for enabled and disabled registry-only devices while preserving `404` for missing devices. |
| Readiness failure attribution | Failed/dead-lettered provisioning and deactivation readiness responses include `readiness.failure` with layer, source state, retryability, error fields, operation id, and occurrence time. |
| Admin visibility | Platform-admin quota request list/show and audit event list endpoints enforce admin-only access, pagination, and filters. |
| Lifecycle observability | Platform-admin-only metrics expose outbox/inbox status counts, dead-letter breakdowns, operation status/type counts, and active-operation age. |
| Database schema invariants | Catalog tests verify critical tables, columns, constraints, indexes, token hashing columns, message dedupe keys, and audit/query filter indexes. |
| OpenAPI contract validation | Claim resolve, registry-only provisioning-state, provisioned provisioning-state, provisioning, and deactivation responses validate against `openapi.yaml`. |
| Broker adapters | Local broker tests are deterministic; Azure Event Hubs tests do not run unless explicitly configured. |

The v2 test report must distinguish coverage from correctness by naming representative tests for each group above.

## CI

GitHub Actions runs `make test-report` and `make test-repeat` on pushes to
`main` and on pull requests. The workflow starts a Postgres service, runs the
report, builds all packages, and uploads the report artifacts.

Scheduled/manual CI also runs the heavier `make test-race` and
`make fuzz-smoke` targets. These are kept out of the normal PR critical path so
developer feedback stays fast while still giving the project a recurring deeper
signal.

When CI fails:

- Postgres unreachable: confirm the service health check and `TEST_DATABASE_URL`.
- Migration or schema failure: inspect the first failing integration test and
  compare the migration catalog checks with the intended schema.
- OpenAPI drift: update `openapi.yaml` and the response contract test together.
- Correctness gate failure: restore the missing representative test or update
  the gate if the behavior moved to a renamed test.
- Coverage regression: add meaningful assertions for the changed behavior
  before considering a threshold override.
- Race/repeat/fuzz failure: treat it as test isolation or parser correctness
  evidence; do not paper over it by removing the target.
