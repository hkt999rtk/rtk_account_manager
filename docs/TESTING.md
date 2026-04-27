# Testing Guide

## Test Layers

The backend uses three test layers:

| Layer | Command | Purpose |
| --- | --- | --- |
| Unit tests | `make test` | Fast package-level checks that do not require Postgres unless `TEST_DATABASE_URL` is set. |
| Integration tests | `make integration-test` | Runs the API, store, migrations, auth, authorization, and device lifecycle tests against Postgres. |
| Full report | `make test-report` | Runs formatting, integration-aware tests, coverage, build validation, and writes `docs/TEST_REPORT.md`. |

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

`reports/` is ignored by git. Commit `docs/TEST_REPORT.md` when intentionally refreshing the maintained report.

## CI

GitHub Actions runs `make test-report` on pushes to `main` and on pull requests. The workflow starts a Postgres service, runs the report, builds all packages, and uploads the report artifacts.
