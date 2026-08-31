# Multi-cloud implementation checkpoint — 2026-08-31

This is implementation progress, not release acceptance or a replacement contract.

## Approved baseline

Contracts #131, Account Manager #298, Billing #7 and Cloud Admin #314 are merged.
Workspace #372 passed every current-head CI gate and merged at
`a7bc72615557b036db6d5621449e18c5a4556551`.
The isolated integration checkout is synchronized recursively and clean. The
original user's workspace and unpublished identity-correction checkout remain
untouched. No staging deployment or shared database migration has been performed.

## Implemented locally, not published

- Forward migration 052 enforces exactly one designated owner at transaction
  commit, serializes membership writes, and preserves legacy customer-org rules.
  Pending/disabled ownership is retained; operational access is fenced separately.
- Managed-cloud lists have consistent all/owned/shared pagination and an independent
  ownership quota; deleted clouds do not consume owned quota.
- Register delegates to the same pending, transactional email signup flow. No
  registration session is issued. Activation enables global login.
- Platform cloud creation requires an explicit existing verified global owner,
  checks their quota, and commits cloud/member/operator audit atomically. It does
  not default financial responsibility or membership to the platform operator.
- Generic account provisioning rejects owner assignment/demotion. Cloud member
  collection reads are owner-only. Legacy customer fixtures are explicitly separate
  from public registration tests.
- Forward migration 053 persists cloud-owner-approved Product admission separately
  from Product roles. It backfills only explicit existing Product assignments and
  rejects cross-cloud Product references. Membership removal disables old ACLs,
  deletes admission and cancels pending invitations; rejoin cannot revive them.
- Product invitations require an already accepted cloud membership. Delegated
  Product owners cannot expand the cloud owner's approved scope. Mutations lock
  users/cloud, recheck authorization, write audit and change authorization version
  transactionally. Cloud owners retain management after Product-only delegation.
  Product/device/fleet reads and video-device eligibility intersect Product scope.
  This is not yet complete viewer, download, job or cache authorization coverage.
- Cloud membership invitations use the same stable users-then-cloud lock order.
  Acceptance revalidates the inviter's current ownership and operational eligibility;
  a former owner retained as a member cannot admit invitees. Concurrent acceptance
  admits exactly once. This does not replace the pending Billing handoff work.
- Forward migration 054 adds cloud viewer membership and persisted selected/all
  Product scope. Viewer reads come from this scope, including future Products,
  without one Product role per resource. Pending invitation replay compares the
  normalized complete scope, resend preserves it, and acceptance uses stored scope.
  Owner-only member PATCH replaces scope atomically; changing away from viewer
  removes viewer scope and stale grants without granting new Product access.
- Viewer permission ceilings reject writes, Billing and playback even with stale
  high-privilege ACLs. Product managers cannot promote cloud viewers or expand their
  selected scope. Tenant device/profile/fleet queries no longer bypass scope for
  platform-capable users; independent admin routes remain separate.
- Scoped group/tag lists and group details count only authorized devices, including
  mixed groups. Their totals and rows use the same repeatable-read snapshot.
  Service OpenAPI now describes viewer scope, owner-only member PATCH and the actual
  member DELETE path (previously nested under the enable path by mistake).
- The generic organization member-create API rejects Brand Clouds, requiring the
  invitation workflow. Generic Brand Cloud role updates use the owner-checked
  transactional member mutation and reject direct owner assignment.
- Billing's separate implementation branch now has durable preparation,
  confirmations/commit/finalize/abort, responsibility/privacy and predecessor
  reversal store logic through `ed9c4b6`. Its production AM/collector/transport
  integration is still outstanding; see Billing's own checkpoint for evidence.
- Forward migration 055 and the AM acceptance store replace the immediate
  owner/admin role swap with a durable `preparing` operation. Request and acceptance
  separately require bound, fresh trusted Billing eligibility; nonnegative credit
  (including zero) never masks incomplete evidence or other financial blockers.
  A negative balance is reported as `balance_negative`. Missing Billing adapter or
  explicit producer inventory fails closed with HTTP 503, without issuing a transfer
  email or changing ownership. No production adapter is configured yet.
- The external eligibility check runs before local locks, then stable ordered
  user/cloud locks recheck source ownership/version, target eligibility, recipient
  email and proof freshness. Acceptance reserves a target quota slot and atomically
  records the operation, immutable participant inventory, prepare outbox and audit.
  Owned counts remain actual ownership; separate `reserved_count` accounts for
  incoming operations. Concurrent creation/acceptance share the target user lock.
  Acceptance returns HTTP 202 and grants no target membership or owner role.
- Accepted preparation adds a cloud eligibility fence; source/target can still
  inspect the operation through their authenticated global session. Other users
  and disabled/unverified participants cannot. Repeated target acceptance returns
  the same operation, including concurrent retries; legacy accepted requests with
  no durable operation cannot masquerade as new handoffs. Old pending requests
  without an ownership version are not silently adopted.
- Precommit cancellation persists `canceling` and abort commands. It retains the
  cloud fence and quota until Billing and every persisted participant return matching
  authenticated release evidence. A timeout, invitation expiry, duplicate HTTP
  delivery or one participant alone cannot release it. Replay is idempotent;
  changed receipts and unknown participants are rejected. Source membership and
  all balances remain unchanged. The internal ack method has no public route.
- Service OpenAPI describes the asynchronous acceptance and participant-only
  status, nonnegative/unknown financial errors and reserved quota. Its cancellation
  route now matches the actual `/owner-transfer/{transferId}/cancel` path.
- Forward migration 056 persists immutable participant preparation acknowledgments.
  Each trusted-adapter call binds the operation, cloud, source/target, ownership
  version and exact cutoff, and requires both a hold receipt and a drained-work
  checkpoint. A successful HTTP delivery is not accepted as preparation evidence.
  Readiness uses the inventory captured at acceptance, survives process restart,
  and requires every participant. This is a preparation gate only, not a financial
  confirmation, commit grant or owner change; there is no public receipt-write route.
- Late prepare receipts during cancellation do not release holds or restore
  readiness. Exact replay is stable, changed receipts conflict, and a terminal
  cancellation cannot record a new preparation. Receipt and audit writes share
  one transaction. The database now also rejects skipping initial preparation or
  completing cancellation with missing/empty producer inventory or missing release
  acknowledgments. This does not authenticate remote evidence on its own.
- Acceptance now rechecks the invitation's actual expiry after the external
  eligibility call and user/cloud locks, rather than using only request-start time.
- `internal/billinghandoff` implements the dedicated Billing HTTP client for
  prepare, live settlement, exact participant confirmation, commit authorization,
  finalize and precommit abort. It uses HTTPS except literal loopback test HTTP,
  refuses redirects, bounds request lifetime/response size, validates the complete
  echoed scope and nested cutoff/snapshot/grant/ack, and preserves int64 amounts.
  Missing zero-value amount/confirmation fields cannot silently become valid data.
  It sanitizes remote errors and does not autonomously retry or change durable IDs.
- Billing's optional dedicated HTTP runtime and this separately compiled AM client
  are tested over loopback TCP against isolated Billing PostgreSQL. This proves
  transport/serialization/persistence, not actual AM owner commit: fixture collector
  and AM decision evidence remain synthetic. The server/public preview and confirm
  adapter is now wired as described below; production eligibility and workers remain
  outstanding.

## Verification at this checkpoint

- Account Manager sole-owner/concurrency/eligibility/list tests passed repeated
  isolated PostgreSQL runs, including stale-repeatable-read writers and failed
  migration preflights. Published migration markers were not edited.
- Account Manager full API package passed with isolated PostgreSQL (54.322s).
  Covers current signup/register parity, encrypted email rollback, activation,
  global login, owner-only member listing, platform creation, device profiles,
  factory JWTs and App end-user multi-cloud binding.
- The earlier complete Account Manager suite was not green: its remaining failure
  was `TestBrandCloudOwnerTransferRequiresExistingTargetAndAcceptsWithLoggedInDeveloper`.
  That immediate-transfer expectation has now been replaced with verified durable
  preparation/no-owner-swap assertions; see the newer handoff evidence below.
  The earlier full run is recorded at
  `/tmp/rtk-multicloud-product-suite-20260831-r2.jsonl`.
- Product admission/delegation/revocation/concurrent-acceptance tests passed three
  consecutive isolated runs. Fresh-database migration tests verify backfill,
  provenance, cross-cloud rejection and no revival on marker replay. Injected
  audit failures roll back role changes, removal and Product ownership transfer.
- Cloud invitation owner-eligibility and concurrent-acceptance tests passed three
  repeated runs. The full API package passed again after invitation locking changes
  (`/tmp/rtk-multicloud-invitation-api-20260831.log`); `go vet ./...` passed.
- Viewer API/store/fresh-database constraint tests passed repeated runs, including
  future Products, scope reduction, role changes, rejoin, group/tag counts, token
  rotation, normalized replay, invalid/null scope and platform/tenant separation.
  Full API tests passed after service OpenAPI updates (39.733s), with viewer
  invitation/acceptance/PATCH responses validated against that contract.
  Logs: `/tmp/rtk-multicloud-viewer-api-final-20260831.log` and
  `/tmp/rtk-multicloud-viewer-suite-final-20260831.jsonl`. The complete rerun after
  the platform-permission guard still has only the known old owner-transfer test
  failure; it is not green. The full API suite passed again after generic member
  entry-point guards (`/tmp/rtk-multicloud-viewer-api-generic-20260831.log`). Focused
  tests additionally inject actual owner/admin ACLs and verify real Billing/payment
  permissions remain denied and Product role projection stays read-only.
- Billing full suite passed with a separate isolated PostgreSQL database. Its
  new eligibility tests cover -1/0/+1, int64 extremes, independent blockers,
  malformed/unknown evidence, and transfer-versus-deletion rules.
- Runtime default pre-PR/coverage gates, service CI and staging evidence are still
  outstanding. The successful documentation CI does not validate these runtime changes.

### AM handoff acceptance checkpoint

- Fresh isolated PostgreSQL `multicloud_am_handoff_test` on loopback port 63229
  applied all migrations through 055. Released 049 was not edited; the unpublished
  identity correction remains reserved for forward 051 integration. No shared or
  staging data was migrated.
- Full uncached `go test ./... -count=1` passed, including API 60.713s and store
  69.386s. The old transfer test now verifies that acceptance preserves source
  ownership and starts preparation, rather than expecting source-to-admin demotion.
  Final targeted race runs passed (store 4.231s, API 17.867s); the initial handoff
  suite passed three repeated runs (store 12.806s, API 7.046s). Durations are not
  performance benchmarks; integration fixtures serialize on a database advisory lock.
- Coverage includes -1/0/+1, positive balance with unresolved usage, stale/wrong-bound
  evidence, missing adapter, request-to-accept balance changes, target disable while
  the external check is in flight, same-request concurrency, competing target quota
  reservations, immutable outbox/receipts, audit rollback, post-expiry cancellation,
  partial/duplicate/changed/unknown release acknowledgments and no early membership.
  Participant status remains usable without cloud membership and while fenced.
- HTTP acceptance, participant status, real cancellation path and fail-closed
  financial/unavailable errors are validated against service OpenAPI. `go vet ./...`
  and `git diff --check` passed. Logs: `/tmp/rtk-am-handoff-suite-final-20260831.log`,
  `/tmp/rtk-am-handoff-race-final-20260831.log`,
  `/tmp/rtk-am-handoff-repeated-20260831.log`.
- These tests supply **synthetic Billing eligibility and resource acknowledgments**.
  They prove local persistence, reservations, admission fencing, replay and cancel
  semantics, not actual Billing HTTP delivery, complete producer settlement, owner
  commit, consent revocation or end-to-end staging acceptance. No runtime PR has
  been opened, merged or deployed for this implementation branch.

### AM preparation receipt verification — 2026-08-31

- Created a separate isolated PostgreSQL database `multicloud_am_prepare_test`
  on loopback 63229 and applied all migrations through 056. No already-applied
  migration was rewritten and no shared/staging database was changed.
- Full uncached `go test ./... -count=1` passed (API 58.893s, store 68.070s),
  including OpenAPI and existing authentication/resource integration tests.
  Log: `/tmp/rtk-am-prepare-suite-20260831.log`.
- The added direct-SQL transition test passed independently after that full run
  started (2.116s). Final preparation/expiry/cancellation/database race tests passed
  three repeated runs (store 6.247s, database 5.775s). Log:
  `/tmp/rtk-am-prepare-race-20260831.log`. `go vet ./...` and `git diff --check` passed.
- Coverage includes exact operation/cloud/party/version/cutoff binding, missing
  hold/drain proof, wrong producer, changed/identical replay, persisted inventory
  across restart/configuration change, outsider/disabled participant reads,
  concurrent receipts, audit rollback, late prepare during cancellation,
  immutable evidence and refusal to release with partial/empty inventory.
  Slow-eligibility expiry creates no operation or quota reservation.
- Receipt digests are **synthetic test fixtures**. These results validate AM's
  local evidence storage and transition guards, not actual remote hold/drain,
  Billing amount confirmation, committed ownership or end-to-end handoff. The
  public preview/confirm and production participant adapters remain outstanding.

### AM-to-Billing transport verification — 2026-08-31

- The AM client passes three repeated race runs (1.645s), covering trusted TLS,
  redirect refusal, scoped nested responses, cutoff/ownership mismatch, missing
  explicit zero/confirmation fields, negative credit, blocked snapshots, response
  size limits, malformed/cacheable replies and sanitized errors. Amounts 0, 1 and
  `math.MaxInt64` retain exact int64 serialization through confirmation.
- Full uncached AM suite passed on isolated `multicloud_am_prepare_test`
  (API 62.757s, store 72.795s, client 1.858s), including App end-user/authentication,
  migration and OpenAPI regressions. Log: `/tmp/rtk-am-transport-suite-20260831.log`.
- Billing's `TestHandoffAccountManagerClientContract` ran this checkout's actual
  client over loopback TCP into Billing's PostgreSQL-backed router. Three repeated
  Billing HTTP/client-contract race runs passed (9.517s); separate full Billing
  regression results are recorded in its checkpoint. Synthetic collector/AM
  decision receipts do not prove global-session checks or an actual AM owner swap.
- `go vet ./...` and `git diff --check` passed. No runtime PR, deployment, live
  payment, email or staging owner change was performed.

### Public preview and confirmation checkpoint — 2026-08-31

- Forward migration 057 adds immutable observed Billing snapshots, authenticated
  participant confirmation intents and acknowledgments. Snapshot cutoff and actor
  references are constrained; one actor/idempotency key cannot change snapshots.
  Confirmation intent and audit commit before remote delivery. Lost replies or
  local acknowledgment/audit failure retain the intent for exact explicit replay.
- Participant-only global-session GET status/preview and POST confirm use the
  dedicated Billing client. Preview requires every persisted preparation receipt,
  current eligible source/target, matching ownership version and unexpired request.
  Remote calls run outside AM locks; all local gates are rechecked afterward.
  The API rejects missing/null/negative amounts, unknown fields, trailing JSON,
  missing idempotency keys and non-human subject types. Zero is explicitly valid.
- Observed snapshots survive restart, outages, cancellation and terminal reload.
  Without fresh confirmable Billing evidence, confirmation flags reset to false;
  the previous amount remains history, not reusable approval. New amount/version
  requires new exact consent. Out-of-order responses cannot overwrite newer
  snapshots; a canceled operation cannot be revived by a delayed read.
- The server optionally configures paired `BILLING_HANDOFF_BASE_URL` and dedicated
  `BILLING_HANDOFF_TOKEN`. Startup validates trusted origin and credential isolation.
  No runtime environment or secret was changed. Initial eligibility and producer
  inventory are still not configured in production, so new transfers fail closed.
- Fresh isolated `multicloud_am_confirmation_test` and the separate cross-service
  `multicloud_am_public_http_test` databases on loopback 63229 applied through 057.
  No released migration marker or shared/staging database was modified.
- Database-backed API responses are validated against service OpenAPI, including
  zero preview, source/target confirmation, replay, reload, cancellation, outsider
  denial and malformed requests. Store tests additionally cover changed amount,
  remote reply loss, audit rollback, disabled target during I/O, older responses,
  expiry/cancellation during I/O, financial blockers and concurrent same-key writes.
- Billing's optional `TestHandoffAccountManagerPublicAPIContract` runs this
  checkout's `TestLiveOwnerHandoffPublicAPIWithBilling` with actual AM global
  sessions, request/email-token acceptance, HTTP transport and separate AM/Billing
  persistence. Both participants confirm zero and retries persist once; source
  remains owner and target still has no ordinary cloud membership. The bootstrap
  HTTP hook exists only in Billing's test wrapper, never its production router.
  Initial financial eligibility, producer receipts and settlement checkpoints
  are synthetic. This does not prove real producer settlement or owner commit.
- Confirmation does not release fences, consume quota, change owner/Product roles
  or finalize Billing. Pending intents have explicit replay but not yet an automatic
  delivery worker. Runtime CI, browser and staging acceptance remain outstanding.
- Full uncached `go test ./... -count=1` passed on the confirmation database
  (API 86.340s, store 110.684s), including migration, App end-user, authentication
  and OpenAPI regressions. Log: `/tmp/rtk-am-public-handoff-suite-20260831.log`.
  Billing's full suite with both cross-repository fixtures enabled also passed;
  its public-API fixture passed three repeated Billing-side race runs (13.555s).
  `go vet ./...` and `git diff --check` passed. Durations are not benchmarks;
  integration fixtures serialize on their database advisory lock.

  Final focused AM race tests passed three consecutive runs (API 30.071s,
  store 42.709s, config 1.444s), including confirmation replay/concurrency,
  financial drift, cancellation/expiry and API authorization/contract assertions.
  Log: `/tmp/rtk-am-public-handoff-race-20260831.log`. Cross-service fixture
  implementation and Billing evidence are committed locally at `07e9fa2`.

### Durable AM owner commit and finalization checkpoint — 2026-08-31

- Forward migration 058 adds immutable authorization requests, committed AM
  decisions and finalization acknowledgments. It extends the transition graph
  through committing/finalizing/succeeded without editing older migration files.
  Active-cloud uniqueness excludes completed history; incoming quota reservations
  are consumed at owner commit, not counted again during finalization.
- The trusted coordinator method persists a stable Billing authorization ID and
  snapshot version, command outbox and audit before calling Billing. Both locally
  acknowledged participant consents, all preparation receipts, current eligible
  users/source ownership/version, invitation expiry and target quota are rechecked
  under ordered user/cloud locks. Billing grants only its current settled snapshot.
  HTTP calls do not hold AM database locks. Lost grant replies retry the same ID.
- After the grant, one AM transaction records the irreversible decision, promotes
  target to the sole owner, increments ownership version, consumes quota, transfers
  source's active Product-owner duties, removes source membership/admission/ACLs,
  cancels its pending Product/cloud invitations and revokes source-created service
  account grants and unused claim tokens in this cloud. Other collaborators and
  unrelated global/cloud permissions are retained. Audit and finalize outbox commit
  with that decision; injected audit failure rolls back the entire owner swap.
- A deferred constraint rejects committed decisions without the matching owner
  transaction. Phase guards require preparation/consent and matching revocation;
  canceled/succeeded history is immutable. Cancellation may win before commit,
  including while grant delivery is in flight, but never after a committed decision.
  The persisted authorization ID must be used when reconciling precommit abort.
- Finalization uses the immutable AM decision, not current source access. Disabled
  source or lost response does not permit rollback. Billing acknowledgment queues
  release commands for the captured producer inventory. AM remains fenced until
  all participants acknowledge finalization/release; a timeout or Billing alone
  cannot mark success. Duplicate receipts are idempotent; changed receipts conflict.
- Participant status now exposes committing/finalizing/succeeded and retains
  committed consent history. No public endpoint accepts commit grants or release
  receipts. These are trusted coordinator/adapter methods; automatic delivery and
  real producer acknowledgment authentication remain required before rollout.
- Fresh isolated `multicloud_am_commit_test` applied through 058. Focused tests
  cover simultaneous commits, lost grant/finalize responses, owner-audit rollback,
  cancellation/disable/expiry/quota changes during grant I/O, Product/service/claim
  revocation, retained collaborators, quota conversion and gated finalization.
- The cross-repository public-API fixture now has consent-only and commit variants.
  The latter consumes the real Billing grant, commits AM's actual unique owner,
  posts that actual decision to Billing, validates two responsibility periods and
  the new current owner/version, then verifies public status and source/target
  access. Initial eligibility, producer preparation/release and settlement
  checkpoints remain synthetic; no real provider settlement, running producer
  drain, background worker, browser or staging acceptance is implied.
- Full uncached AM `go test ./... -count=1` passed (API 72.341s, store 105.925s).
  The final commit/finalization/related balance tests then passed three race runs
  (store 21.491s), including the added missing-consent, negative-after-consent,
  finalization-audit rollback and unrelated-cloud preservation checks. Logs:
  `/tmp/rtk-am-owner-commit-suite-20260831.log` and
  `/tmp/rtk-am-owner-commit-race-20260831.log`. Both-repository `go vet ./...` and
  `git diff --check` passed. The real-commit Billing fixture passed three repeated
  Billing-side race runs (23.104s). Durations are not performance benchmarks.
  The additional deferred-constraint test passed three race runs (2.883s): a
  direct SQL committed-decision insert without the matching owner transaction is
  rejected and leaves source ownership unchanged. Billing's actual-AM integration
  checkpoint is committed locally at `31aef1c`.

### Durable handoff recovery worker checkpoint — 2026-08-31

- Forward migration 059 adds per-operation scheduling/lease metadata and immutable
  precommit cancellation decisions. Phase/prepare/confirmation/finalization writes
  wake jobs without stealing leases. `SKIP LOCKED`, database time, random lease
  tokens and compare-and-swap completion partition work and recover crashes.
  A later wake generation beats an older attempt's backoff. Neither lease expiry
  nor retry count changes ownership, reservations, protocol phase or resource holds.
- `internal/worker/handoff` bounds batches and step lifetimes, applies capped
  exponential retry and continues after temporary database errors. There is no
  retry limit that declares a committed handoff canceled/completed. Logs and job
  outcomes use fixed sanitized codes, not raw remote diagnostics or credentials.
- The stage runner uses persisted operation/participant state, not browser or
  transient worker flags. It installs Billing's actual hold, obtains each resource
  adapter's bound hold/drain receipt and accepts Billing preparation evidence only
  from its authoritative settled snapshot after resource preparation. HTTP prepare
  delivery alone is never counted as drain or financial completeness.
- Before commit, the worker can replay an original authenticated confirmation
  intent whose response was lost, with the same actor/key/version/amount. It never
  manufactures source/target consent. Once both persisted acknowledgments exist,
  it automatically invokes the tested commit protocol, retries finalization and
  requests release only after Billing's durable finalize acknowledgment.
- Cancellation records a stable cancellation ID, attempted authorization ID and
  decision digest in the same transaction as canceling/audit/outbox. Older
  nonterminal canceling operations can recover this record from their durable
  phase; no timeout is treated as cancellation. Billing prepare is idempotently
  established before abort so cancellation can beat first delivery without treating
  404 as proof of release. Abort-pending never counts as a release acknowledgment.
- Billing now accepts the AM attempted authorization ID before a grant exists,
  while requiring exact equality if one was issued. Same-payload cancellation
  replay stays stable and late grant creation is blocked. This closes the window
  between sending an authorization request and observing its result.
- The new standalone `cmd/handoff-worker` validates dedicated Billing credentials
  and bounded timing. It does not auto-migrate, modify runtime config or install
  no-op producer adapters. Production resource adapters remain **unimplemented**;
  unknown/missing persisted participants hold progress. Programmatic adapter
  interfaces are exercised only with explicit synthetic resource fixtures so far.
- Fresh isolated `multicloud_am_worker_test` applied through 059. Initial database
  tests passed for partitioned claims, expired-lease recovery, stale completion,
  wake-versus-backoff, missing/cross-scope participant evidence, lost consent/grant/
  finalize replies across store restart, cancellation and terminal unscheduling.
  Worker unit tests cover bounded configuration, step deadline, graceful shutdown,
  sanitized failures and indefinite capped retry without dead-letter release.
- The cross-repository API fixture adds a worker variant. After real authenticated
  source/target confirmation, `RunOnce` claims persisted jobs and automatically
  drives actual AM owner commit, real Billing HTTP finalization and terminal status.
  Resource prepare/release and collector completeness remain synthetic; this is
  not proof of real producer drain or end-to-end staging acceptance.
- Full uncached AM suite passed on `multicloud_am_worker_test` (API 74.622s,
  store 110.193s). Final targeted store/worker/config race tests passed three runs
  (15.132s / 2.016s / 2.865s), including lease recovery, wake generations,
  cancellation-versus-commit and lost-response recovery. Logs:
  `/tmp/rtk-am-handoff-worker-suite-20260831.log` and
  `/tmp/rtk-am-handoff-worker-race-20260831.log`. Billing's three-mode HTTP fixture
  and abort cases also passed three race runs. Both repos passed `go vet ./...`
  and `git diff --check`; Billing's OpenAPI importer test passed. These are local
  correctness results, not runtime PR CI, scale benchmarks or deployment evidence.

## Managed cloud create/read/update checkpoint (060)

- Added forward migration 060 with immutable, actor/operation/cloud-scoped write
  receipts. Creation and PATCH persist the result, receipt and audit in the same
  transaction. Creation locks the eligible global user before checking ownership
  quota, so concurrent retries generate one cloud/owner and consume one slot.
- Developer POST now accepts only name/description with a required 1–200 printable
  character Idempotency-Key. PATCH supports partial name/description changes and
  explicit description clearing; UUID, slug and ownership cannot be supplied.
  Both enforce bounds in the store as well as strict, bounded API JSON parsing.
  Duplicate/unknown keys, null fields, trailing JSON and nonhuman sessions fail.
- PATCH serializes with ownership/collaboration mutations, checks the current
  owner and respects the active handoff fence. Replays check current eligibility,
  membership, lifecycle and ownership version; an old receipt is not an access
  grant. A replay returns the prior result without reapplying an older rename.
- Detail and list share the managed-cloud projection: derived sole owner, caller
  role, description, status and ownership version. Nonoperational clouds have no
  capabilities. Detail now uses the canonical `brand_cloud` envelope rather than
  requiring the legacy separate `membership` field. Active owners receive the
  implemented `cloud.update` capability. No delete capability is advertised yet.
- Store/API tests cover concurrent retry, quota, restart replay, actor isolation,
  tombstones, an eligible user with no remaining clouds, Unicode length bounds,
  immutable identity fields, audit rollback, role/scope rejection and handoff
  fencing. Local OpenAPI definitions reflect the implemented surfaces. Production
  activation, producer drain and staging acceptance are not asserted by these tests.
- Fresh isolated `multicloud_am_crud_test` applied through 060. The full uncached
  Go suite passed (API 87.660s, store 109.636s); targeted API/store race tests passed
  three runs (22.370s / 32.723s), including concurrent quota checks. Logs are
  `/tmp/rtk-am-managed-cloud-suite-20260831.log` and
  `/tmp/rtk-am-managed-cloud-race-20260831.log`. `go vet ./...`, diff whitespace
  checks and all 11 email-signup helper unit tests passed. These tests ran against
  loopback port 63229, not a shared database. Workspace-wide inventory, PR CI and
  coordinated staging acceptance remain release gates.
- This is not a complete CRUD release: deletion preflight, durable resource/Billing
  closure, DELETE/operation APIs and My Clouds BFF/UI remain pending. The stricter
  create payload/idempotency requirement and detail response must be coordinated
  with BFF/CLI callers before deployment. No shared database or runtime deployment
  has been changed.

## Cloud deletion advisory preflight checkpoint

- Added owner-only `GET /v1/developer/brand-clouds/{id}/deletion-preflight` with
  structured blockers, local counts and an exact nonzero Billing balance when
  available. It is a read-only observation, not a reservation or DELETE operation.
  A disabled Product/device is still an uncleaned resource. Completed device
  operation history does not count as running work; unresolved/dead-letter work
  remains a blocker. Counts are omitted when remote inventories overlap rather
  than adding potentially duplicate resource counts.
- Local reads use a repeatable, read-only snapshot. AM closes that transaction
  before bounded dependency I/O and rechecks owner eligibility, ownership and
  authorization versions plus local resources afterward. Lost ownership returns
  not-found; changed authorization invalidates dependency evidence. Accepted
  handoffs block deletion, and new resources appearing during I/O are included.
- The dedicated Billing client calls Billing's read-only deletion preflight using
  the cloud UUID, current global owner and ownership version. It validates exact
  binding, response fields, integer balance, known blocker codes, no-store and
  freshness. Zero balance does not suppress debt, pending payment/refund/dispute
  or missing usage/invoice/provider evidence. Transfer's >=0 rule is unchanged.
- Server startup wires the existing dedicated Billing transport if configured.
  Production resource observers are **not implemented/installed**. Missing or
  incomplete reviewed observer inventory blocks readiness, even if Billing says
  clear. Adapter interfaces and positive readiness are currently exercised only
  with explicit synthetic resource checkpoints. No no-op adapter is installed.
- Tests cover disabled resources, unresolved jobs, no preflight side effects,
  nonowner access, missing/expired/misbound evidence, dependency errors, new
  resources during I/O, owner disable and handoff acceptance during observation.
  Billing's cross-repository fixture exercises the actual AM HTTP client against
  Billing's real router/store. Collector checkpoints remain synthetic; this is
  not staging/production resource-drain or closure evidence.
- Durable DELETE operations, producer write fences/closure acknowledgments,
  Billing closure and operation status are still pending. Preflight eligibility
  must never be reused as authority for those steps. No schema change was needed
  in AM for this read-only surface; Billing added forward evidence migration 054.
- Full uncached AM Go suite passed on isolated `multicloud_am_crud_test` (API
  111.389s, store 148.576s). Targeted API/store/client race tests passed three runs
  (22.696s / 38.342s / 1.985s), including dependency-I/O state changes. Logs:
  `/tmp/rtk-am-deletion-preflight-suite-20260831.log` and
  `/tmp/rtk-am-deletion-preflight-race-20260831.log`. Vet and whitespace checks
  passed. These timings include parallel package/advisory-lock contention and are
  correctness results, not scale/latency benchmarks. No runtime PR or deployment
  was performed; full workspace inventory/CI and staging remain release gates.

## Durable cloud deletion checkpoint (061)

- Added owner-only DELETE and operation-status API under the existing developer
  namespace. DELETE requires exactly one valid Idempotency-Key and no body.
  Admission rechecks advisory evidence and local inventory, serializes user/cloud
  writes, persists the operation and producer inventory, and returns 202 plus
  Location. Identical retries return that operation, including after tombstoning;
  a different key cannot create another operation. Status never grants cloud
  resource access and remains limited to the original enabled/verified global user.
- Forward migration 061 installs a durable lifecycle fence, immutable producer
  hold/drain receipts, close-command outbox and completion evidence, plus leased
  recovery jobs. The existing eligibility predicate denies access during deletion.
  Local Product/device/job/factory/claim/invitation/ownership admission serializes
  with the cloud; historical jobs cannot be reactivated through status changes.
  The migration does not backfill or mutate historical ownership or applied markers.
- Trusted producer adapters must prove both a persistent hold and empty inventory,
  bound to the exact operation/cloud/owner/cutoff/authorization version. Their
  configured names must match the reviewed preflight observer inventory. Missing
  adapters or proof cannot be replaced with health/empty-page/no-op success.
- The AM Billing client validates exact scope, nested operation/readiness/receipt,
  no-store and bounded responses over the existing dedicated HTTPS transport.
  After complete producer holds and live zero-balance Billing readiness, AM
  persists the exact close command before delivery. Lost replies/restarts resend
  that command rather than choosing a new receipt or inferring success.
- Only a matching Billing closed acknowledgment permits the atomic AM tombstone,
  membership/ACL/invitation/claim revocation, completion record and audit. History
  is retained. SQL rejects direct unsupported tombstones/restoration; old fixtures
  now exercise the durable protocol instead of writing deleted_at directly. A
  post-Billing-commit owner disablement does not force rollback: the trusted
  recovery path completes the already-authorized tombstone, without restoring
  normal account access. Global sessions/certificates for other clouds are not
  indiscriminately revoked.
- Jobs use database-time leases, SKIP LOCKED claims and compare-and-swap release.
  Lost/expired worker leases can be reclaimed; stale workers cannot release a
  newer lease. Protocol commands remain idempotent if remote delivery overlaps.
  No retry limit or timeout removes the lifecycle fence.
- A cross-repository fixture exercises actual AM global-session DELETE/status,
  both independent databases and the real Billing HTTP/store. It deliberately
  drops the first successful Billing close reply. A new AM store instance claims
  the persisted job, obtains the original closure acknowledgment, tombstones and
  denies subsequent cloud detail reads. Producer inventory/holds and independent
  collector checkpoints are explicitly synthetic, not staging acceptance.
- Release gates still include production observer/hold/drain/provider/collector
  adapters, production worker enablement, complete queued/background
  mutation coverage, BFF/UI and cross-service acceptance. The fixed-command
  recovery gaps recorded at 061 are addressed by the local 062 checkpoint below;
  this does not remove the production release gates. Do not enable deletion in
  staging until the actual adapters and acceptance evidence are complete.
- Verification: full uncached AM suite passed on fresh isolated
  `multicloud_am_deletion_v3_test` through 061 (API 130.823s, store 187.510s,
  database 18.498s). Targeted deletion/preflight/CRUD/client race checks passed
  three runs (store 78.259s, API 26.929s, client 1.361s). OpenAPI validation,
  response-contract tests, vet and whitespace checks passed. A final targeted
  deletion run additionally checks the time-zone-independent cutoff comparison.
  Logs: `/tmp/rtk-am-cloud-deletion-v3-suite-20260831.log` and
  `/tmp/rtk-am-cloud-deletion-v3-race-20260831.log`. The real AM/Billing
  lost-reply test passed three Billing race-instrumented runs using the separate
  fresh `multicloud_am_deletion_http_test` database; log:
  `/tmp/rtk-am-billing-deletion-cross-race-20260831.log`. These are correctness
  tests, not scale benchmarks or production/provider acceptance. No runtime PR
  or shared-database/staging change was made.

## Deletion command recovery and cancellation checkpoint (062)

- A Billing `409 NOT_READY` is not permanent command rejection: a delayed retry
  could otherwise close the account after AM replaces or cancels that command.
  AM now requires Billing's exact operation/settlement/readiness-bound retirement
  receipt before recording retirement and issuing a replacement with fresh
  settlement evidence. Lost retirement replies retry the same command; unknown
  HTTP failures never authorize replacement. If Billing reports that closure won,
  recovery finishes the original AM tombstone, never reopens Billing.
- Forward migration 062 retains the immutable 061 command and copies it into
  append-only attempt history. Replacement is serialized on the cloud and requires
  retirement of every prior attempt. Cancellation prevents any new attempt. Cloud
  row writes invalidate stale REPEATABLE READ writers at these transitions; a
  row lock alone is not treated as snapshot invalidation.
- `RequestCloudDeletionCancellation` records authenticated current-owner intent.
  It is a store command, not a new public HTTP/BFF endpoint. The coordinator first
  resolves every issued close, persists cancellation, and waits for Billing and
  every recorded resource producer to acknowledge release. Even a producer that
  never observed prepare must install a durable cancellation tombstone so delayed
  prepare cannot reacquire its hold. Missing adapters/evidence keep access fenced.
- Billing cancellation does not restore revoked payment methods or consents.
  The AM lifecycle fence clears only after all release receipts and the final
  audit commit. SQL rejects premature completion, contradictory recovery evidence
  and replacement before retirement. Audit failure rolls back the relevant local
  transition. Once closure wins, cancellation instead converges on deletion.
- Canceled operations remain immutable history and are excluded from job claims
  and the active deletion fence. A new key may start another deletion; the original
  key replays its original canceled result. Keys are scoped to the requesting owner,
  so a subsequent owner can reuse a key without colliding with predecessor history.
- Verification uses fresh isolated PostgreSQL through 062, never the shared DB.
  Full uncached AM suite passed on `multicloud_am_deletion_recovery_v3_test`
  (store 171.541s); the subsequent actor-key regression also passed. Tests cover
  stale settlement, lost retirement reply, replacement admission, partial release,
  audit rollback, close winning cancellation and stale-snapshot rejection.
  The cross-repository fixture now tests four modes: lost close reply, stale evidence
  plus lost retirement reply, staged cancellation releases, and close winning
  cancellation. It invokes real AM/Billing stores and HTTP; resource/provider/
  collector receipts remain explicitly synthetic. Targeted AM deletion/preflight/
  CRUD/client race tests passed three runs (store 58.460s, API 22.417s, client
  2.852s), including the final actor-key regression. All four cross-service modes
  passed three Billing race-instrumented runs on the fresh AM
  `multicloud_am_deletion_recovery_http_test` database through current 062
  (34.467s); the AM child is separately compiled, with its race coverage in the
  AM run above. Logs: `/tmp/rtk-am-deletion-recovery-v3-suite-20260831.log`,
  `/tmp/rtk-am-deletion-recovery-v3-race-20260831.log` and
  `/tmp/rtk-am-billing-deletion-recovery-cross-race-20260831.log`.
  Vet and whitespace checks passed. No runtime PR, deployment or shared-data
  change occurred.

## Deletion recovery worker checkpoint (063)

- Added a real recovery-only executable and bounded polling service. It uses the
  dedicated Billing client, database-time leases, bounded batch concurrency,
  per-step/finish deadlines, capped persistent retry and signal-aware shutdown.
  It refuses implicit database selection, malformed timing, reused credentials
  or a database missing migration 063. It does not run migrations or expose HTTP.
- Separate store recovery configuration cannot admit new deletions, even after
  later preflight setup. Missing participants retain their recorded inventory and
  keep preparation/cancellation fenced. Persisted close commands can recover
  with existing proof; no producer/collector acknowledgment is fabricated.
- Forward migration 063 adds a scheduling generation. Cancellation, phase changes
  and retirement/release proof wake the job; stale completion cannot overwrite a
  newer wake with retry backoff. Blocker updates alone do not cause hot retries.
- Added worker build/release output and configuration examples, without enabling
  a service. See `cloud_deletion_recovery_worker.md` for exact runtime boundaries.
  Remaining producer/collector adapters and deployment acceptance are still gates.
- Verification: targeted store tests passed on fresh isolated
  `multicloud_am_deletion_worker_test` through 063; worker/config race tests passed
  three runs. The actual subprocess starts/stops cleanly on SIGTERM in three
  race-instrumented runs using `multicloud_am_deletion_worker_process_test` and
  makes no Billing calls for an empty queue. All four AM/Billing recovery modes now
  use the polling service and passed three Billing race-instrumented runs (36.596s),
  log `/tmp/rtk-am-billing-deletion-worker-cross-race-20260831.log`.
  Resource/provider/collector evidence remains synthetic, not production acceptance.
  Full uncached AM suite passed (store 122.596s) on
  `multicloud_am_deletion_worker_test`; log:
  `/tmp/rtk-am-deletion-worker-suite-20260831.log`. Three database race runs passed
  on fresh `multicloud_am_deletion_worker_race_test` (store 60.905s, API 21.871s);
  log: `/tmp/rtk-am-deletion-worker-race-20260831.log`. Final configuration parsing
  regression tests separately verify zero/negative batches, explicit whitespace
  values, missing database selection and duration overflow rejection. Build, vet
  and whitespace checks passed. No runtime PR or deployment occurred.

## Forward identity correction and activation checkpoint (051)

- Reconciled the preserved unpublished correction into forward migration 051,
  without changing published 049 or deleting any applied migration marker.
  The original dirty identity-correction worktree remains untouched.
- The correction now follows the approved multi-cloud invariant: exactly one
  designated owner for every non-deleted Brand Cloud. A pending/disabled owner
  retains ownership; their cloud is fenced for owner and collaborators until
  approved activation restores eligibility. Zero/multiple-owner conflicts block.
- Unsupported unchanged inherited credentials become reset/activation-required;
  global refresh grants and app-user certificates are revoked. Existing/remediated
  global credentials and SSO adoption remain protected. Mapping outcomes and
  historical audit events are preserved; correction appends auditable evidence.
- Activation restores only exact, unchanged recorded verification holds.
  Administrative disable, role/scope edits, removal and global disable invalidate
  holds. Email resend preserves an eligible hold, never invents provenance for an
  administrative disable. Outbox failure rolls back the user/member/hold together.
- Added current-state JWT/refresh/certificate and organization ACL checks, without
  removing the newer Product/viewer/cloud-eligibility gates. Platform capabilities
  remain independent of memberships and cannot impersonate a cloud-scoped grant.
- Added rollback-only `migrate --identity-preflight`, including deferred constraint
  validation, controlled blocking IDs and ineligible-owner cloud counts. It works
  on restores through 050 and on prior implementation candidates with 052–063
  already installed but 051 absent; no marker manipulation is needed.
- Release limitation: a pre-049 legacy restore with duplicate membership targets
  or a reset-required sole owner can still fail inside immutable published 049.
  A regression explicitly reproduces the duplicate-target refusal and verifies
  rollback preserves legacy evidence. Forward 051 cannot repair an earlier
  aborted transaction. A reviewed pre-cutover path is still required; do not
  bypass the failure, rewrite evidence or present this checkpoint as full cutover
  completion. See `identity_correction_runbook.md`.
- Targeted isolated PostgreSQL identity/membership/API tests passed through 063,
  including correction/replay, provenance conflict resolution, owner eligibility,
  OIDC adoption, stale JWT/refresh/certificate rejection, and full activation.
  Full uncached suite passed (API 80.113s, database 39.063s, store 91.456s);
  log: `/tmp/rtk-am-identity-suite-final-20260831.log`. Two targeted race runs
  passed (database 109.350s, store 41.448s, API 22.754s);
  log: `/tmp/rtk-am-identity-race-20260831.log`. Build, vet and whitespace
  checks passed. No runtime PR, staging or shared database was modified.

## Required next work

1. Complete cross-service authorization, downloads/background work and cache
   invalidation around the implemented Product admission/viewer scope. Reconcile
   generic provisioning/role APIs and all resource-mutation fences. The 051
   activation-hold correction is integrated above.
   Retire legacy human
   identity fallbacks, retaining only the required migration evidence.
2. Complete cloud deletion/closure and production handoff integration.
   Advisory owner-only deletion preflight now exists; wire real reviewed resource
   observers and Billing collectors. Fenced DELETE/operation state and leased
   recovery primitives and worker now exist through 063, including exact-command
   retirement and pre-close cancellation; finish the production-adapter gates and
   reviewed cancellation tooling/public-surface integration before enablement.
   Idempotent managed-cloud creation/PATCH and shared detail/list projection are
   implemented through 060; finish BFF/CLI compatibility before release.
   The automatic lease/retry coordinator is implemented through 059; wire
   production Billing eligibility, dedicated runtime credentials and actual
   producer hold/drain/release adapters. Deliver the committed ownership
   and authorization changes to resource services and queued work. Keep access
   fenced through every finalization acknowledgment; known commits only retry
   forward. Acceptance or two confirmations alone still do not complete transfer.
3. Integrate Billing's implemented store protocol/privacy with real AM decisions,
   evidence collectors, provider adapters and audited operations. A configured test
   producer list is not proof of complete production inventory. AM's eligibility
   fence is not proof of remote producer drain or protection for every already-in-flight
   resource mutation; transactional producer guards and cutoff evidence remain gates.
4. Complete the scoped My Clouds/Product UI and BFF, including request/cache/tab
   isolation and hosted-return binding.
5. Finish the reviewed pre-049 legacy cutover path without editing released 049;
   forward 051 is integrated. Run full migration/coverage/CI, coordinated
   backup/restore checks, and staging activation/device/certificate/MQTT acceptance.
   Legacy-table cleanup remains a separate post-acceptance operation.

No implementation PR is merged or deployed at this checkpoint. Neither passing
API tests nor the isolated financial predicate means the overall plan is complete.

## Product device display integration — 2026-08-31

Added `PATCH /v1/orgs/{orgId}/device-item-profiles/{profileId}/devices/{deviceId}/display`
for the cloud-scoped console. Only name/model strings are accepted. Omitted
fields remain unchanged; an empty model explicitly clears that display field.
Duplicate/unknown/null fields are rejected. The API is documented in OpenAPI and
covered by HTTP response-contract validation.

The store serializes users, cloud and device, checks the exact Product binding
and current device permission inside that transaction, updates only the two
display columns and records `device.display.updated` atomically. Existing
serial/MAC/manufacturer, status, claim material, operational metadata and Product
binding are not rewritten. The same ACL query is reused by normal reads and the
transactional check. The endpoint only handles Brand Clouds, not legacy customer
organizations. Cloud eligibility/viewer ceilings apply through that query.

This avoids using the legacy full-record device update from a display-only form:
the old update replaces omitted hardware/metadata columns. It does not assert that
all legacy producers, update endpoints or lifecycle operations are now fenced.
The old Admin provision helper also sends an empty request, while current AM
provision requires resolved activation inputs; the scoped UI does not offer a
nonfunctional replacement button. Claim/provision/deactivation orchestration is
still a separate integration gate.

Targeted store/API tests passed on the newly created loopback-only database
`multicloud_device_display_20260831` (port 63229), including concurrent independent
name/model updates, explicit model clearing, exact cloud/Product denial, unchanged
hardware/metadata, audit counts, stale admin ACL under viewer membership and
activation-hold denial. No migration, shared database or staging state changed.
Logs: `/tmp/rtk-device-display-am.log`; full-suite/race outcomes are recorded in
`test_report.md` after execution. Current AM and Admin OpenAPI validation passes.

A new read-only inventory overlay includes **all three current implementation
worktrees**, not only Admin. It exposes 21 blocking AM/Billing operation mappings
(19 inline-list parsing cases and two cross-feature requirement assignments),
plus stale workspace traceability. Diagnostic:
`/tmp/rtk-device-scope-inventory.4o3gdW/local-inventory/spec-inventory.json`.
These must be reconciled with the actual normative requirements before CI/release;
the earlier Admin-only zero-blocker inventory was not cross-service acceptance.

## Refreshed integration inventory — 2026-08-31

The three implementation branches now include the merged documentation-governance
mainline; canonical contracts are at `d261dd0`. Supporting notes and links follow
the new lowercase filenames. The preceding 21-blocker report is superseded by
the combined read-only candidate at
`/tmp/rtk-multicloud-refreshed-inventory.ypQM3C/local-inventory/`:
40 features, 391 requirements, 655 operations, 65 workflows, zero blocking
findings (91 existing nonblocking normative candidates remain).

Workspace inventory now parses YAML structure, including flow lists and quoted
colon paths, and resolves registered local Path Item references. It rejects remote
or unregistered reads, invalid pointers, cycles and ambiguous overrides. The
requirement denominator is unchanged; referenced consumer operations are added,
not removed to pass the check. Member update maps to the canonical multi-cloud
sharing feature; invitation retains its canonical invitation feature, with
multi-cloud sharing explicitly connected through `WF-MULTICLOUD-SHARING-001`.
Account Manager OpenAPI tests pass after those two mapping corrections.

The workspace's committed dependency snapshot is separate: 621 operations and
zero blocking findings against merged service mainlines. The 655-operation
candidate includes unmerged implementations and must not be presented as an
accepted deployment. Service PRs must merge before parent gitlinks change.
Runtime, migration, full CI and staging acceptance gates above remain open.

## Shared global-auth boundary — 2026-09-01

The shared JWT parser now rejects retired tenant identities for both access and
refresh tokens, including mixed claims and callers without API persistence.
Production tenant-token issuers were removed. Legacy claim fields remain only
for decoding and rejection; this does not remove historical records or tables.
Global human accounts and independent App end-users retain their existing issuers.
Product scope comes from the explicit cloud/organization URL, never tenant claims.
Device/fleet and Product handlers no longer select a tenant-human authorization path.

Full uncached Go tests and vet passed on the disposable loopback PostgreSQL
database `multicloud_identity_auth_20260901`. RSA/HMAC access/refresh tests cover
legacy, mixed and unsupported subjects alongside valid global/end-user controls;
HTTP tests confirm retired tenant routes remain 404 and old tokens receive 401.
Focused race tests also passed. See `test_report.md` for measured results.
No shared database, staging service, migration marker or certificate was changed.
Production participant wiring, full CI, migration rehearsal and staging gates
remain required; this checkpoint is not deployment acceptance.

## Human device-write serialization — 2026-09-01

The existing device create/update/status/delete HTTP handlers now call explicit
human-authorized store methods. Each transaction locks the actor, organization
and (where present) device, then repeats the current organization/device permission
query. Ownership changes and membership removals cannot leave a previously
admitted HTTP request authorized to write after their commit. Brand Cloud viewer,
activation, lifecycle and Product ceilings are retained; platform capability is
not cloud admission. Legacy customer-organization platform behavior is preserved.
Successful writes record their actor in an atomic audit event; failed audits roll
back the mutation. Public response schemas and legacy full-record semantics are
unchanged; the Product display editor still uses its separate narrow PATCH.

Database lock-wait tests preserve a stale ACL while switching the sole membership
owner, then prove all four pending writes are denied. HTTP tests pause body reads
after middleware admission, switch ownership, and verify create/update/status
requests return 404 without changing data. These switches are synthetic fixtures,
not substitutes for Billing prepare/commit/finalize acceptance.

Full uncached tests and vet pass; repeated store and HTTP race tests pass (details
in `test_report.md`). Only task-owned loopback databases were used. Producer
mutations, claim/provision/deactivation orchestration, generic unbound-device
creation retirement and other resource-family write fences still require review
and integration. This does not certify all in-flight resource operations or
authorize deployment/legacy-table cleanup.

## Migration failure recovery coverage — 2026-09-01

The first real combined-checkout PR-profile run passed functional tests but
failed database/store package coverage ratchets (80.65%/78.00%). New migration
failure tests verify unavailable inputs fail closed and marker/commit rejection
rolls back both schema and migration state, followed by a safe synthetic retry.
The full database race suite now measures 88.7%; store and the fresh governed
gate remain open. No published migration, threshold or requirement was changed.
See `test_report.md` for the exact local commands/artifacts and limitations.

## Deletion cancellation/evidence regression — 2026-09-01

New failure-path tests exposed a late producer response being acknowledged and
audited despite no receipt being inserted after cancellation. Conditional receipt
insertion now checks its affected-row count and returns conflict without false
audit evidence. Tests also cover atomic audit failure/retry, malformed retirement
proofs, conflicting release replays and canceled requests preserving owner and
worker lease state. Two race-enabled runs and store vet pass; the full governed
gate and production producer/Billing integration remain required. Financial
policy is unchanged: deletion requires zero balance; transfer permits >= 0.

## Billing advisory eligibility transport — 2026-09-01

The real Billing HTTP client now implements the store's eligibility interface;
the store and transport share the explicit request/response DTO. Requests bind
cloud, current owner/version, target, action and (at acceptance) transfer ID.
Required fields, receipt/digest, no-store, freshness, blocker codes and balance
consistency are validated before store admission. Unknown outcomes fail closed.
No production producer inventory, collector or adapter was inferred or enabled.

Focused transport race tests passed (1.718s), including -1/0/+1, missing/null
amounts, stale/mismatched evidence and HTTP failures. Actual independently
compiled AM client/Billing router tests also passed for all three balances at
request and acceptance, using only disposable loopback PostgreSQL. AM handoff
and deletion store race regressions passed (51.273s); `go vet ./...` and
`go build ./...` passed. Logs: `/tmp/rtk-am-eligibility-client.log`,
`/tmp/rtk-am-eligibility-store-regression.log` and
`/tmp/rtk-billing-eligibility-cross-service.log`.
This is protocol/admission evidence, not full ownership transfer, the governed
coverage gate, production collector completeness or staging acceptance.

## Human lifecycle operation admission — 2026-09-01

Provision/deactivate now require a human actor and reauthorize the exact lifecycle
permission under the actor/cloud/device write locks before creating any operation,
outbox message or pending projection. They reuse the CRUD authorization boundary;
Brand Cloud platform-only users, viewers, inactive owners and revoked owners do
not bypass membership or lifecycle fences. New operations add an atomic audit
event; successful idempotent replays retain the same operation/message and emit
no duplicate audit. Provision admission cannot select another operation type.

Focused store/API race tests passed (9.926s/7.044s), including five HTTP requests
paused after middleware admission and released after an owner switch. Neither
provision nor deactivate can queue work afterward. Audit rejection rolls back
the operation, outbox and pending metadata together. Existing lifecycle/CRUD
regressions, `go vet ./...` and `go build ./...` pass. Log:
`/tmp/rtk-am-lifecycle-admission-race.log`. Full uncached and governed runs are
tracked separately; this does not certify claim/unprovision, privileged producer
paths, draining already queued work, production handoff adapters or staging.

The full uncached suite subsequently passed at `d7f5c21`. The fresh workspace
PR-profile package gate remains FAIL solely on store coverage (79.30% versus
80%; overall 81.28%, artifact redaction PASS). See `test_report.md` for exact
source/run identities and limitations. No default pre-PR pass, runtime service
PR merge or deployment is claimed by this checkpoint.

## Claim/unprovision admission and independent App claims — 2026-09-01

Human claim resolution now locks and rechecks the global actor, cloud permission
and token Product scope before consuming the token. Unprovision repeats its
device permission within the transaction; its explicit platform override locks
and rechecks the global platform actor and the cloud's operational state. Neither
override nor ordinary admission bypasses an active handoff fence. Claim audit is
atomic and excludes token/provisioning material. A retained tenant-identity fixture
now explicitly proves rejection, followed by successful global-user unprovision.

Regression testing exposed App claim's earlier reuse of the human entrypoint and
two-transaction claim/binding write. App admission now independently locks an
active `end_users` actor and uses the shared token mutation inside the same
transaction as its Brand link, device binding and separately attributed audit.
No global membership is invented. Binding failure leaves the token unconsumed.
The App login/certificate identity boundary is unchanged.

Focused store/API race tests passed (13.148s/18.905s), including approved versus
unapproved Products, a viewer with a stale write ACL, revoked Product admission,
platform suspension/revocation, handoff/cancellation fences, audit rollback,
cross-identity denial, App binding rollback/retry and multi-Brand App claim.
The HTTP test pauses seven human mutations after middleware admission, switches
the owner and proves none may write; an additional HTTP test revokes platform
authority after admission and proves unprovision is denied. Log:
`/tmp/rtk-claim-admission-final-race.log`. Full suite/governed evidence follows
separately. Privileged claim transfer/reclaim, other producers, queued-work drain,
production collector/participant wiring, CI and staging remain release gates.
