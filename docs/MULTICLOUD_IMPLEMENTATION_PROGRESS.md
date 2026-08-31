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

## Required next work

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

### Remaining delivery gates

1. Complete cross-service authorization, downloads/background work and cache
   invalidation around the implemented Product admission/viewer scope. Reconcile
   generic provisioning/role APIs, activation holds and all resource-mutation fences.
   Retire legacy human
   identity fallbacks, retaining only the required migration evidence.
2. Complete cloud idempotent CRUD/deletion and the AM handoff coordinator beyond
   acceptance/cancel preparation. Add production Billing eligibility/command adapters,
   dedicated credentials, outbox delivery/retries and actual producer hold/drain
   adapters for the persisted preparation acknowledgments. Implement the AM-side
   versioned settlement preview and both participant confirmations against Billing's
   authoritative snapshots. Then commit
   the sole-owner swap, consume the reservation, transfer Product-owner duties and
   remove every old-owner membership/delegated grant atomically with the committed
   outbox event. Keep access fenced until Billing finalize acknowledgment; after a
   known commit only forward retry is legal. Current 055/056 deliberately have no
   successful owner-commit path; acceptance is not transfer completion.
3. Integrate Billing's implemented store protocol/privacy with real AM decisions,
   evidence collectors, provider adapters and audited operations. A configured test
   producer list is not proof of complete production inventory. AM's eligibility
   fence is not proof of remote producer drain or protection for every already-in-flight
   resource mutation; transactional producer guards and cutoff evidence remain gates.
4. Implement the scoped My Clouds/Product UI and BFF, including request/cache/tab
   isolation and hosted-return binding.
5. Reconcile the preserved unpublished identity correction as forward migration
   051 (do not edit released 049), then run full migration/coverage/CI, coordinated
   backup/restore checks, and staging activation/device/certificate/MQTT acceptance.
   Legacy-table cleanup remains a separate post-acceptance operation.

No implementation PR is merged or deployed at this checkpoint. Neither passing
API tests nor the isolated financial predicate means the overall plan is complete.
