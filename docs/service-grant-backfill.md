# Legacy Product Service Grant Backfill

Status: local implementation, not run on a deployed environment. This is a
prerequisite for enabling `ACCOUNT_MANAGER_PLATFORM_SERVICE_PRODUCT_WRITES`; it
does not enable that gate or register any service.

The existing `device_item_profiles.service_options` array is the source for
legacy Products. The backfill inserts revision 1 only where no immutable grant
exists. Such rows have `legacy=true`, `catalog_revision=0`, empty manifest
bindings, and a digest of the recorded option set and any existing logging
retention setting. They are not checked against
today's service catalog and never acquire MQTT or another option implicitly.
Products with an existing grant are verified against their current options but
not rewritten. Existing production runs keep a null service revision; runs
issued after backfill pin the immutable snapshot. Thus a historical HTTP-only
Shadow Product remains HTTP-only until explicitly migrated.

## Operator sequence

1. Take a matched database backup and freeze Product/production-run writes.
   Keep Product write gate and all optional-service cutover flags off. Apply
   schema migrations through `092_product_service_apply_authorization.sql` using the
   normal migration deployment. Use the appropriate Account Manager
   `DATABASE_URL` for the environment.
2. Run `go run ./cmd/migrate -service-grant-backfill-report`. This command is
   read-only and needs only `DATABASE_URL`, not API JWT secrets. Review
   `products`, `already_versioned`, `needs_backfill`, `without_mqtt`,
   `option_counts`, and every `issues` entry. The command exits nonzero when
   `ready=false`. Investigate malformed or mismatched rows; do not normalize
   them automatically. A Product with no grant and an option outside the five
   historical codes (`mqtt`, `iot_shadow`, `video_streaming`, `video_storage`,
   `device_logging`)
   is also blocked for explicit reconciliation. This legacy-only guard does
   not constrain already-versioned Products or future registered plugins.
3. When the report is approved, run
   `go run ./cmd/migrate -service-grant-backfill-apply -service-grant-backfill-expected-sha256=<snapshot_sha256>`.
   Apply re-reads under table locks and compares the entire Product/grant
   snapshot to the reviewed report. Missing or stale SHA-256, any invalid row,
   or any database error leaves the whole backfill uncommitted.
4. Run the read-only report again. Require `ready=true` and
   `needs_backfill=0`, and compare option counts and Product snapshots to the
   pre-migration report. Keep the write gate off until the separate registry,
   factory, token, SDK, and deployment acceptance checks pass.

This is an additive migration; rerunning apply with a fresh reviewed digest is
idempotent. Do not delete grant rows to roll back after runs or Claim Tokens
refer to them. Roll back an environment only from a matched database backup
with its corresponding application release, then require fresh service leases.
