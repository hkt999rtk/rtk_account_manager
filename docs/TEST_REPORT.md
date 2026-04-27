# Test Report

Generated: 2026-04-27T04:28:23Z

## Summary

| Check | Result |
| --- | --- |
| Overall | PASS |
| Formatting | PASS |
| Tests | PASS |
| Build | PASS |
| Coverage threshold | PASS |

## Coverage

| Metric | Value |
| --- | --- |
| Total statement coverage | 74.9% |
| Minimum required coverage | 70.0% |
| Coverage mode | atomic |
| Coverage scope | ./internal/... |

## Test Execution

| Metric | Value |
| --- | --- |
| Go packages | 10 |
| Test cases started | 23 |
| JSON pass events | 29 |
| JSON fail events | 0 |
| Integration database | Postgres via TEST_DATABASE_URL |

## Commands

```sh
gofmt -l .
TEST_DATABASE_URL='***' go test -json ./... -coverpkg=./internal/... -coverprofile=reports/coverage.out -covermode=atomic
go tool cover -func=reports/coverage.out
go tool cover -html=reports/coverage.out -o reports/coverage.html
go build ./...
```

## Artifacts

| Artifact | Purpose |
| --- | --- |
| reports/test-events.json | Machine-readable Go test event log. |
| reports/coverage.out | Go coverage profile. |
| reports/coverage.txt | Function-level coverage summary. |
| reports/coverage.html | HTML coverage report. |
| reports/gofmt.txt | Files requiring gofmt, empty when formatting passes. |
| reports/build.txt | Build output, empty when build passes. |

## Coverage Gaps To Watch

- Command entry points under `cmd/*` are intentionally validated by `go build ./...`, not unit coverage.
- Store and database behavior are primarily covered through API integration tests.
- Add or update integration tests whenever authorization, membership, token, migration, or device lifecycle behavior changes.
