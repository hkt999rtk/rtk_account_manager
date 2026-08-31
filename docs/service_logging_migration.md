# Service Logging Migration

Status: implementation handoff.

Owner: `rtk_account_manager`.

## Goal

Move Account Manager API, migrations, workers, and scheduled jobs to
`rtk_cloud_logger` zap logs so account-side operations are traceable through the
central RTK Cloud logger.

## Current State

The service currently uses stdlib `log` in command entrypoints and Gin default
logging/recovery middleware for HTTP request logs. These outputs are not the
shared JSON service log contract.

## Required Changes

- Construct a root `*zap.Logger` with `rtk_cloud_logger` in every command.
- Replace stdlib `log.Printf`, `log.Println`, and `log.Fatal` with typed zap
  events.
- Replace Gin default request logging with zap request middleware that records
  method, sanitized path, status, latency, remote address, and request id.
- Keep panic recovery behavior, but make recovered panic logs structured and
  redacted.
- Propagate `trace_id`, `request_id`, and `operation_id` into outbox and inbox
  worker logs.
- Do not log auth headers, bearer tokens, refresh tokens, passwords, cookies,
  OIDC secrets, or database DSNs with credentials.

## Entrypoints To Cover

- `cmd/server`
- `cmd/migrate`
- `cmd/outbox-worker`
- `cmd/inbox-worker`
- `cmd/cleanup-tokens`
- `deploy/systemd/*.service`
- `deploy/systemd/*.timer`

## Acceptance Criteria

- Account Manager API and worker units emit single-line JSON zap logs.
- `GET /v1/health` request logs include `service`, `env`, `version`,
  `request_id`, `status`, and `duration_ms`.
- Provisioning lifecycle messages can be traced across outbox/inbox workers by
  `operation_id` or `trace_id`.
- `go test ./...` passes after migration.
