# Brand Cloud tier evidence for OTA billing

Account Manager records each Brand Cloud's `evaluation` or `commercial` tier
change in the append-only `brand_cloud_tier_events` table. Migration 093 records
one baseline observation for existing clouds; it does **not** certify their
earlier tier. New clouds receive an event at creation.

Trusted Billing workloads can call
`GET /v1/internal/brand-clouds/{brandCloudId}/billing-tier` with
`period_start` and `period_end` set to one complete UTC month. The response
sets `commercial_for_full_period` only when history covers the month start,
the starting tier is commercial, and there is no tier change during the
month. A missing baseline, evaluation tier, or mid-month change fails this
proof. The endpoint requires `ACCOUNT_MANAGER_INTERNAL_AUTH_TOKEN` and sends
`Cache-Control: no-store`.

This is **tier evidence**, not a paid contract decision. Billing must also
verify the applicable Managed Cloud agreement and active billing account
before charging. Product OTA grant history remains a separate check for each
OTA usage fact. No OTA price card is activated by this migration or endpoint.
