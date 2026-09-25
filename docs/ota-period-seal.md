# OTA Platform Product-Grant Period Seal

Status: local implementation for operator-run generation and delivery; not
deployed or evidence of a completed OTA billing month.

Owner: rtk_account_manager.

Last reviewed: 2026-09-25.

Canonical contract:
[Product OTA Delivery And Billing](rtk_cloud_contracts_doc/ota_delivery_and_billing.md).

Account Manager supplies Billing's `platform_grants` seal. The independent
OTA producer supplies its own receipt/fact seal. Billing requires both before
closing a month with OTA pricing. The Platform seal contains the sorted
Brand Cloud Product IDs whose immutable service-grant history included
`ota` during any part of the UTC month, including zero-use Products. It has
no OTA fact counts or fact digest. `source_sha256` hashes the complete
observed grant history before the month end; the high-water JSON carries
that digest and row count. A stable UUIDv8 seal identity makes exact retries
safe.

Apply migration `088_product_service_grants_immutable.sql` before running
this command. It rejects UPDATE and DELETE of historical grant rows;
backfill and normal Product changes remain insert-only. An older database
without that migration is not a qualified billing source.

## Preview And Submit

Run the command only after the selected UTC month has ended. It takes one
repeatable-read database snapshot and rejects a period whose end is still in
the future according to PostgreSQL's clock.

```sh
DATABASE_URL='postgres://...' go run ./cmd/ota-period-seal \
  --organization-id '<brand-cloud-uuid>' --month 2026-09
```

Preview prints the JSON seal without contacting Billing. Review the
`product_ids`, period, `source_sha256`, and nonempty high-water marker.
Then use the same command with `--submit`, an internal HTTPS
`BILLING_OTA_PERIOD_SEAL_BASE_URL`, and the dedicated
`BILLING_OTA_PLATFORM_SEAL_TOKEN`. The token must match Billing's
`BILLING_OTA_PLATFORM_SEAL_TOKEN`; do not reuse the OTA producer or any
other service credential. The client refuses redirects, bounds response
size and time, and accepts only an exact seal echo. Changed replay gets a
conflict; an uncertain response can be retried.

The command is currently operator-run, not a scheduled worker. This is a
remaining release gate: every charged Brand Cloud and UTC month needs an
authenticated Platform seal, including months with no OTA Products. Billing
must fail close when the seal is missing. Do not claim automatic collection
or charge readiness until scheduling, alerting, and staging reconciliation
are qualified.

Grant `created_at` is the available historical effective-time source. The
seal conservatively includes any Product whose `ota` option was effective
for a positive interval of the month. This does not reconstruct historical
Product active/inactive status or device entitlements, which lack equivalent
month-snapshot evidence in this path. Those limits must be reviewed before
commercial activation; they cannot be replaced with the current Product
row or a zero-usage assumption.
