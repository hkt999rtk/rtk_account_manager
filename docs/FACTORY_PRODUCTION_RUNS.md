# Factory Production Runs

This document defines the Account Manager side of factory production-run
authorization for initial device certificate enrollment.

## Purpose

A production operator must choose the exact manufacturing scope before a factory
can request device certificates:

- brand cloud
- device item profile, also called Product or product profile
- production validity period
- allowed device quantity
- optional factory and batch identifiers

Account Manager persists that scope as a production run and signs a factory
enrollment JWT. The factory enrollment daemon uses that JWT as the canonical
certificate selector. URL names, tenant slugs, CSR fields, and request-body
overrides must not select the cloud CA or Product CA.

## PKI Selection Model

The intended device-certificate hierarchy is:

```text
Platform Root CA
  -> Platform Device CA
      -> Developer Cloud Device CA
          -> Product CA
              -> Device Certificate
```

Account Manager does not sign certificates directly in this flow. It binds the
production run to `brand_cloud_id` and `device_item_profile_id`; the factory
enrollment daemon forwards those immutable identifiers to certissuer. Certissuer
then resolves the Developer Cloud Device CA and Product CA through issuer policy.

## API

`POST /v1/admin/brand-clouds/{brandCloudId}/device-item-profiles/{profileId}/production-runs`

Authentication: platform-admin bearer token.

Request:

```json
{
  "factory_id": "factory-a",
  "batch_id": "batch-20260617",
  "allowed_quantity": 250,
  "valid_from": "2026-06-17T00:00:00Z",
  "valid_until": "2026-06-19T00:00:00Z"
}
```

Response:

```json
{
  "production_run": {
    "id": "run-uuid",
    "brand_cloud_id": "brand-cloud-uuid",
    "device_item_profile_id": "profile-uuid",
    "factory_id": "factory-a",
    "batch_id": "batch-20260617",
    "status": "active",
    "allowed_quantity": 250,
    "issued_quantity": 0,
    "valid_from": "2026-06-17T00:00:00Z",
    "valid_until": "2026-06-19T00:00:00Z"
  },
  "factory_jwt": "<secret bearer token>",
  "token_type": "Bearer",
  "expires_at": "2026-06-19T00:00:00Z",
  "audience": "factory-enroll"
}
```

## JWT Claims

| Claim | Required | Meaning |
| --- | --- | --- |
| `sub` | yes | `factory_production_run:<production_run_id>` |
| `aud` | yes | Factory enrollment audience, default `factory-enroll`. |
| `jti` | yes | Unique JWT id for audit and replay correlation. |
| `production_run_id` | yes | Stable production-run id. |
| `brand_cloud_id` | yes | Immutable developer cloud selector. |
| `device_item_profile_id` | yes | Immutable Product/profile selector. |
| `profile_key` | no | Human-readable profile key for audit/debug only. |
| `factory_id` | no | Factory identifier selected for this run. |
| `batch_id` | no | Manufacturing batch identifier. |
| `allowed_quantity` | yes | Maximum number of device certificates intended for this run. |
| `nbf` | yes | Production-run start time. |
| `exp` | yes | Production-run end time. |

The JWT must be treated as secret material. It authorizes the factory enrollment
daemon to select the CA path for the run; it is not a general Account Manager
API token.

## Validation Rules

- `brandCloudId` in the URL must identify an active brand cloud.
- `profileId` must belong to that brand cloud.
- disabled device item profiles cannot create production runs.
- `allowed_quantity` must be positive.
- `valid_until` must be after `valid_from`.
- `FACTORY_PRODUCTION_JWT_SECRET` must be configured before issuance.

## State And Audit

Production runs are stored in `factory_production_runs`. Creation emits a
`factory_production_run_created` audit event with the brand cloud, profile,
factory, batch, quantity, and validity window.

`issued_quantity` is reserved for quota enforcement. The current implementation
creates the run and signs a bounded JWT; the next enforcement step is a consume
operation called by factory enrollment after each successful certificate issue.

## Configuration

| Variable | Purpose |
| --- | --- |
| `FACTORY_PRODUCTION_JWT_SECRET` | HS256 signing secret for factory production JWTs. Keep separate from user access and refresh token secrets. |
| `FACTORY_PRODUCTION_JWT_AUDIENCE` | Expected factory enrollment audience. Defaults to `factory-enroll`. |
