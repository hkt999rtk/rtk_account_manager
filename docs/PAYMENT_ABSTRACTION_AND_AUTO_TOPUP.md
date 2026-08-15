# Payment Abstraction And Automatic Top-Up Design

Status: proposed implementation design. Documentation review is required before
migrations, routes, workers, provider calls, or UI implementation begin.

Owner: `rtk_account_manager`.

Contract source of truth:

- `docs/rtk_cloud_contracts_doc/PAYMENTS_AND_BALANCE.md` after the contracts
  submodule is advanced to the reviewed contract commit.

## Decision Summary

Account Manager will own the phase-one commercial settlement domain because it
already owns Brand Cloud identity, membership, authorization, and audit. The
domain is kept separate from existing identity, registry, and provisioning
packages and can later be extracted to a Billing Service.

The design provides:

- one commercial account per Brand Cloud and currency;
- an append-only PostgreSQL monetary ledger;
- provider-neutral payment methods, intents, attempts, and webhook inbox;
- a customer-consented automatic top-up policy;
- a provider adapter registry whose first planned adapter is NewebPay;
- durable reconciliation and an emergency provider-disable control.

It does not make Account Manager a usage meter. Video Cloud and other services
continue to emit immutable usage facts. Pricing and invoice calculation are a
separate missing dependency and must produce an authenticated idempotent debit
instruction before usage affects the balance.

## Current Baseline And Gap

The repository currently has no balance, wallet, invoice, payment-method,
payment-intent, provider-adapter, or top-up implementation. Existing
organization `tier` fields distinguish evaluation and commercial accounts but
do not represent money or payment status.

The workspace already has durable patterns that can be reused:

- PostgreSQL migrations and store transactions;
- outbox/inbox idempotency and worker retry conventions;
- actor-aware audit events;
- permission-based organization scope;
- redacted structured logs and request correlation.

The payment design must reuse these operational patterns without coupling
payment correctness to the provisioning outbox or Redis.

## Architecture Boundary

```text
Cloud Admin UI/BFF
       |
       v
Account Manager payment HTTP API
       |
       v
payment application service
  |        |             |
  v        v             v
ledger   policy      payment orchestrator
  |        |             |
  +--------+-------------+
           |
       PostgreSQL
           |
           v
 payment worker/reconciler ---> PaymentProvider interface ---> NewebPay
           ^                                                    |
           +---------------- verified webhook inbox <-----------+

usage facts ---> pricing/invoice owner ---> authenticated debit instruction
```

HTTP handlers validate transport and authorization. They do not implement
provider cryptography or ledger arithmetic. The application service owns
transactions and domain transitions. Adapters own provider request/response
translation only.

## Proposed Package Layout

```text
internal/payment/
  account.go          commercial account and balance projection rules
  ledger.go           signed delta and reason validation
  intent.go           provider-neutral state machine
  policy.go           threshold, generation, limits, and re-arm rules
  service.go          use cases and transaction boundaries
  provider.go         PaymentProvider interface and capability vocabulary
  errors.go           stable domain errors

internal/paymentstore/
  postgres.go         transactional repository adapter
  account.go
  ledger.go
  method.go
  intent.go
  webhook.go
  reconciliation.go

internal/paymentprovider/newebpay/
  adapter.go
  crypto.go
  request.go
  response.go
  webhook.go
  errors.go

internal/api/
  payment_handlers.go

cmd/payment-worker/
  main.go
```

The package names are a target, not a requirement to split every file at once.
The mandatory boundary is that `internal/payment` does not import Gin,
PostgreSQL drivers, or a provider implementation.

## Provider Interface

The application layer consumes a narrow interface:

```go
type PaymentProvider interface {
    Name() string
    Capabilities(context.Context) Capabilities
    CreateSetup(context.Context, SetupRequest) (SetupResult, error)
    Charge(context.Context, ChargeRequest) (ChargeResult, error)
    Query(context.Context, QueryRequest) (QueryResult, error)
    VerifyWebhook(context.Context, WebhookRequest) (WebhookEvent, error)
    Refund(context.Context, RefundRequest) (RefundResult, error)
}
```

Implementation rules:

- `ChargeRequest` contains a provider-neutral amount, currency, opaque method
  reference, merchant order reference, and stable idempotency/correlation key;
- the interface never carries PAN or CVV;
- unsupported operations return a typed capability error, not a false success;
- adapter errors map to `declined`, `temporary`, `invalid_request`,
  `authentication`, `requires_action`, or `unknown` while preserving a
  redacted provider code for support;
- `unknown` is mandatory when a request may have reached the provider;
- orchestration persists intent and attempt state before and after the call;
- no adapter writes Account Manager tables directly.

`Refund` may initially return unsupported. It is included because refund and
chargeback behavior changes the ledger and must not later bypass the
abstraction.

## Data Model

All monetary fields are `BIGINT`, are interpreted as ISO currency minor units,
and are never floating point. Internal TWD values use cents. The initial public
product accepts whole New Taiwan dollars only, so TWD values are divisible by
100; the NewebPay adapter performs the checked conversion to its integer-dollar
`Amt`. All timestamps are UTC. Provider references are treated as opaque,
length-bounded strings.

### `commercial_accounts`

| Column | Notes |
| --- | --- |
| `id UUID PK` | Internal account identity. |
| `organization_id UUID NOT NULL` | Owning Brand Cloud; unique with currency. |
| `currency CHAR(3) NOT NULL` | Initially `TWD`. |
| `available_balance_minor BIGINT NOT NULL` | Transactional projection of ledger sum. |
| `state TEXT NOT NULL` | `active`, `attention_required`, `suspended`, `closed`. |
| `version BIGINT NOT NULL` | Monotonic projection version. |
| `created_at`, `updated_at` | UTC audit times. |

Unique: `(organization_id, currency)`.

### `balance_ledger_entries`

| Column | Notes |
| --- | --- |
| `id UUID PK` | Immutable entry ID. |
| `account_id UUID NOT NULL` | Commercial account. |
| `direction TEXT NOT NULL` | `credit` or `debit`. |
| `amount_minor BIGINT NOT NULL CHECK > 0` | Positive magnitude. |
| `currency CHAR(3) NOT NULL` | Must match account. |
| `reason TEXT NOT NULL` | Contract reason code. |
| `idempotency_scope`, `idempotency_key` | Unique per account. |
| `external_type`, `external_id` | Invoice, payment intent, adjustment, refund, or chargeback reference. |
| `balance_after_minor BIGINT NOT NULL` | Auditable projection after this entry. |
| `actor_type`, `actor_id`, `request_id` | Attribution. |
| `created_at` | Immutable commit time. |

No update/delete repository methods are provided. Corrections append a
compensating entry.

### `payment_consents`

Stores consent type/version, rendered-text SHA-256, accepted actor, accepted
time, locale, client surface, and revocation time/reason. It stores no card
data. Existing evidence remains after revocation.

### `payment_methods`

Stores account ID, provider, opaque customer/method references, safe display
metadata, provider capability snapshot, lifecycle state, consent ID, and
timestamps. An account may have several methods but only one policy-selected
default. Revocation is a state transition, not deletion.

### `auto_topup_policies`

Stores account ID, enabled state, threshold, top-up amount, currency,
payment-method ID, daily attempt/amount limits, cooldown, generation, armed
state, last trigger/success time, consent ID, actor, and timestamps.

`generation` increments on every policy replacement. The open-intent uniqueness
rule includes account and generation.

### `payment_intents` And `payment_attempts`

An intent stores account, amount, currency, reason, policy generation, provider,
method ID, normalized state, internal idempotency key, merchant order reference,
provider transaction reference, correlation ID, and timestamps.

Attempts are append-only observations of provider calls. They store operation,
attempt number, start/completion time, normalized result, redacted provider
code, request/response evidence digest, and next reconciliation time. Secrets
and provider payloads are never stored in these columns.

Recommended unique constraints:

```text
(account_id, idempotency_key)
(provider, merchant_order_reference)
(provider, provider_transaction_reference) WHERE reference IS NOT NULL
(account_id, policy_generation) WHERE automatic intent is open
(intent_id, attempt_number)
```

The partial uniqueness expression will be implemented using a generated/open
marker or an equivalent PostgreSQL partial index after migration review.

### `payment_webhook_inbox`

Stores provider, provider event reference when present, SHA-256 of the received
body, verification result, mapped intent, normalized event type, processing
state, received/processed time, and a redacted summary. Unique provider event
reference or payload digest provides replay safety.

### `payment_reconciliation_jobs`

Stores intent ID, reason, status, due time, attempt count, lease time, and safe
last error. PostgreSQL is the durable queue. Redis may wake workers but cannot
be the only record.

## Transaction Boundaries

### Debit And Threshold Evaluation

1. Begin PostgreSQL transaction.
2. Resolve the account from trusted organization identity.
3. Lock `commercial_accounts` with `FOR UPDATE`.
4. Insert the debit using its unique idempotency tuple.
5. Update the projected balance and version.
6. Evaluate the active policy against the new committed projection.
7. If eligible, create exactly one `created` automatic intent and a worker job.
8. Commit.

Provider I/O never occurs inside this database transaction.

### Provider Charge

1. Lease a `created` intent in PostgreSQL.
2. Persist a `processing` attempt before network I/O.
3. Call the adapter with the stable merchant order/idempotency reference.
4. Persist the normalized result.
5. An authorization-only response transitions to `authorized` and does not
   credit. On confirmation of the finance-approved capture/completion point,
   lock account and intent, transition once to `succeeded`, and append one
   credit in the same transaction.
6. On timeout or ambiguous response, transition to `unknown` and schedule
   query reconciliation using the same provider transaction/order reference.

### Webhook

1. Read a bounded body and compute its digest.
2. Ask the adapter to verify and normalize it.
3. Insert/dedupe the inbox record.
4. Validate merchant, amount, currency, and internal/provider references.
5. Schedule reconciliation or apply a conclusive legal transition.
6. On success, append the credit through the same service method used by query
   reconciliation.

Duplicate callbacks return a successful acknowledgement after dedupe when the
original event was valid. Invalid callbacks never reveal which internal intent
exists.

## Automatic Top-Up State

The policy is crossing-based, not a loop that polls and charges while the
balance remains low.

```text
armed --balance < threshold--> intent open --> succeeded --> disarmed
  ^                                  |              |
  |                                  |              +--> attention_required
  |                                  |                   if still below threshold
  |                                  +--> failed/unknown --> cooldown/reconcile
  |
  +-- balance >= threshold or authorized policy generation update
```

Daily limits use UTC calendar days in phase one. The API returns the timezone
and reset time explicitly. If product requires customer-local billing days,
that is a later contract change.

Default proposed guardrails, subject to finance approval:

| Setting | Proposed default | Constraint |
| --- | ---: | --- |
| `daily_attempt_limit` | 3 | 1-10 |
| `cooldown_seconds` | 3600 | at least 300 |
| `daily_amount_limit_minor` | no implicit value | customer/finance must choose; cannot be unlimited |
| top-up recursion | disabled | one success per crossing/generation |

The service rejects a policy that has no finite daily amount limit. Environment
configuration may impose a stricter platform maximum than the customer policy.

## Proposed API Behavior

The contract document reserves the route families. Before production code, the
implementation PR must add exact schemas and errors to Account Manager
`openapi.yaml`, regenerate/check documentation, and add route conformance tests.

Common rules:

- organization ID is resolved through existing membership and permission
  checks;
- create/update requests require `Idempotency-Key` where a side effect may
  occur;
- money is encoded as an integer `amount_minor` plus ISO currency;
- list APIs use bounded cursor pagination;
- write responses return normalized internal state, never provider secrets;
- policy updates require the consent version and accepted confirmation;
- setup responses may return a short-lived hosted URL but logs and audit must
  redact its query and token components;
- optimistic update uses policy `version`/ETag to prevent stale changes.

Stable planned error codes:

```text
PAYMENT_PROVIDER_UNAVAILABLE
PAYMENT_PROVIDER_NOT_CONFIGURED
PAYMENT_CAPABILITY_UNSUPPORTED
PAYMENT_METHOD_REQUIRED
PAYMENT_METHOD_INACTIVE
PAYMENT_CONSENT_REQUIRED
PAYMENT_AMOUNT_INVALID
PAYMENT_CURRENCY_UNSUPPORTED
PAYMENT_INTENT_CONFLICT
PAYMENT_STATUS_UNKNOWN
AUTO_TOPUP_LIMIT_REACHED
AUTO_TOPUP_POLICY_CONFLICT
BILLING_ACCOUNT_SUSPENDED
```

Decline details are customer-safe and do not expose fraud/risk signals.

## Authorization And Audit

The contract reserves the billing/payment permission actions. Initial role
mapping proposal:

| Actor | Read account/ledger/intents | Manage method/policy | Manual top-up | Reconcile |
| --- | --- | --- | --- | --- |
| Brand Cloud owner | Yes | Yes | Yes | No |
| Brand Cloud admin | Configurable explicit grant | Configurable explicit grant | Configurable explicit grant | No |
| Member/read-only | Optional read grant | No | No | No |
| Platform support | Redacted read with audited scope | No | No | No |
| Payment worker service | Required internal subset | No customer policy writes | Execute existing intent only | Yes |

Every mutation writes the existing Account Manager audit envelope with payment
resource type and request correlation. Support tooling must not display provider
credentials, full provider payloads, hosted-session tokens, card data, or
unredacted customer identifiers.

## NewebPay Adapter Design

The adapter name is `newebpay`. Domain and API resources do not use NewebPay
field names.

### Credentials And Configuration

Expected secret references:

```text
PAYMENT_NEWEBPAY_MERCHANT_ID
PAYMENT_NEWEBPAY_HASH_KEY
PAYMENT_NEWEBPAY_HASH_IV
```

Expected non-secret configuration:

```text
PAYMENT_NEWEBPAY_ENVIRONMENT=test|production
PAYMENT_NEWEBPAY_API_BASE_URL
PAYMENT_NEWEBPAY_CALLBACK_BASE_URL
PAYMENT_NEWEBPAY_ORDER_PREFIX
PAYMENT_NEWEBPAY_ENABLED=false
PAYMENT_NEWEBPAY_MERCHANT_INITIATED_ENABLED=false
```

Secret values come from the deployment secret manager and are validated at
startup without logging. A missing or malformed provider configuration disables
the adapter and fails payment writes with a typed error; it must not prevent
identity/registry APIs from starting.

### Provider-Specific Constraints

- `MerchantOrderNo` is length/character constrained; map the internal UUID to a
  unique prefixed reference that fits the provider limit and persist the map.
- NewebPay `Amt` is an integer New Taiwan dollar value. Reject fractional-dollar
  TWD input and perform a checked conversion from internal minor units.
- `TokenTerm` is not a public customer ID. Derive a stable non-identifying,
  length-bounded opaque value and persist the association.
- AES and integrity/check computation are isolated in a small tested crypto
  module with official fixture coverage.
- accepted callback data is decrypted/verified before field use;
- redirect/return URLs are browser UX signals; server-side NotifyURL or query
  reconciliation determines durable payment state;
- callback/query responses are normalized before the application sees them.

The public NewebPay documents demonstrate remembered-card and fixed periodic
payment flows, but do not by themselves prove that this merchant may perform
variable-time threshold top-ups. The provider must confirm and enable the
required capability. Until then the adapter reports
`merchant_initiated_charge=false` and policy enablement fails closed.

### Operational Prerequisites

- enterprise merchant account and enabled payment products;
- sandbox and production credentials stored separately;
- registered HTTPS callback URLs;
- stable egress IPs approved by NewebPay where required;
- dedicated sandbox payer/test cards;
- merchant consent and modification/cancellation records;
- finance-owned refund, chargeback, and reconciliation procedures.

## Security And Compliance Controls

- Prefer provider-hosted or embedded-tokenized setup. Never proxy raw card
  fields through Account Manager or Cloud Admin.
- Never store CVV/CVC under any condition.
- Encrypt opaque provider method/customer references at rest when they permit
  charging; keys live outside PostgreSQL.
- Restrict provider credentials to the payment worker and webhook verifier.
- Separate test and production merchant IDs, keys, callbacks, and data.
- Add SSRF-safe fixed provider base URLs; do not accept them from API requests.
- Bound callback body size, validate content type, and apply rate limiting.
- Redact common secrets plus provider field names before logs/artifacts.
- Preserve consent, policy, intent, ledger, refund, and chargeback audit evidence
  according to finance/legal retention decisions.
- Run dependency, SAST, secret, and redaction checks on adapter changes.
- Use a runtime provider kill switch that blocks new charges but still permits
  status query, reconciliation, and read APIs.

This design reduces PCI scope by avoiding card-data handling but does not claim
PCI exemption. Security/compliance review must classify the final hosted or
embedded flow before launch.

## Observability And Support

Metrics use low-cardinality provider/environment/status labels and never account
or transaction IDs as labels. Initial metrics:

```text
account_manager_payment_intents_total
account_manager_payment_attempts_total
account_manager_payment_reconciliation_backlog
account_manager_payment_unknown_age_seconds
account_manager_payment_webhooks_total
account_manager_auto_topup_triggers_total
account_manager_balance_reconciliation_mismatches_total
```

Structured logs include request/correlation ID, internal intent ID, provider,
normalized state, redacted provider code, operation, duration, and outcome.
Provider transaction references appear only in restricted fields and are
masked in general logs.

Alerts:

- any ledger/projection mismatch;
- oldest unknown intent above the reconciliation SLO;
- webhook authentication failures above baseline;
- callback-to-query disagreement;
- repeated automatic top-up failures;
- provider authentication/configuration failure;
- reconciliation backlog or worker lease stalls.

## Test Plan

### Unit

- integer credit/debit arithmetic and overflow boundaries;
- invalid currency, zero/negative amount, and reason validation;
- ledger idempotency and compensation;
- all legal/illegal intent transitions;
- strict `< threshold` behavior, equality behavior, re-arm, generation, cooldown,
  daily limits, inactive method, and no recursive charging;
- provider error normalization and timeout-to-unknown;
- NewebPay order/token-term mapping, crypto, integrity, malformed data, and
  redaction fixtures.

### PostgreSQL Integration

- concurrent debits produce a correct projection and one top-up intent;
- duplicate debit, callback, query, and worker execution converge;
- success and credit commit atomically;
- process failure at each transition resumes safely;
- row locks do not cross tenant boundaries;
- account/ledger reconciliation detects injected drift;
- revoked method/policy races cannot initiate a new charge.

### Provider Contract And E2E

- setup uses the provider-hosted/tokenized surface and stores no card data;
- sandbox success, decline, timeout, duplicate/out-of-order callback, query,
  cancel/refund where supported, and credential failure;
- one debit crossing creates one provider transaction and one credit;
- callback amount, merchant, or signature mismatch cannot credit;
- provider-disabled mode makes no new external request;
- artifacts pass secret/card-data redaction scans.

### UI And Live Staging

- desktop/mobile owner setup, validation, enable, update, disable, method revoke,
  limit reached, requires-action, failure, unknown, and success status;
- final screenshot per UI case/target without card or secret material;
- dedicated sandbox merchant and run-scoped Brand Cloud/test data only;
- database ledger, intent, provider query, callback, and runtime logs correlate
  to one run ID;
- cleanup revokes test methods and deletes run-scoped customer test data while
  retaining the redacted test report.

## Delivery Sequence And Definition Of Done

1. Accept contracts and this architecture document.
2. Confirm provider capability, merchant onboarding, consent, and finance rules.
3. Add exact OpenAPI schemas and planned Test IDs/catalog entries.
4. Add schema and pure domain/store implementation with unit/integration tests.
5. Add a fake provider and complete orchestration tests.
6. Add the NewebPay adapter behind disabled feature flags.
7. Add Cloud Admin BFF/UI and desktop/mobile evidence.
8. Pass sandbox live staging, reconciliation, refund, duplicate callback, and
   cleanup tests.
9. Enable one allowlisted canary Brand Cloud with conservative limits.
10. Expand only after ledger reconciliation and support metrics remain clean.

Implementation is not complete until the exact API is in `openapi.yaml`, all
reserved critical Test IDs have executable sources, required reports are PASS,
and automatic charging remains disabled by default outside the allowlist.
