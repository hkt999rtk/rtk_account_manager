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
- Billing has a tested financial eligibility predicate: settled credit >= 0 for
  transfer; exactly 0 for closure; missing evidence and pending monetary/setup
  work remain independent blockers. This predicate is not yet connected to a
  durable handoff or an HTTP endpoint.

## Verification at this checkpoint

- Account Manager sole-owner/concurrency/eligibility/list tests passed repeated
  isolated PostgreSQL runs, including stale-repeatable-read writers and failed
  migration preflights. Published migration markers were not edited.
- Account Manager full API package passed with isolated PostgreSQL (54.322s).
  Covers current signup/register parity, encrypted email rollback, activation,
  global login, owner-only member listing, platform creation, device profiles,
  factory JWTs and App end-user multi-cloud binding.
- The last complete Account Manager suite is not green. Its sole remaining failure
  is `TestBrandCloudOwnerTransferRequiresExistingTargetAndAcceptsWithLoggedInDeveloper`.
  The old immediate transfer/old-owner-admin flow must be replaced by the approved
  Billing handoff, not preserved merely to pass tests. The full run is recorded at
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

## Required next work

1. Complete cross-service authorization, downloads/background work and cache
   invalidation around the implemented Product admission/viewer scope. Reconcile
   generic provisioning/role APIs, activation holds and all resource-mutation fences.
   Retire legacy human
   identity fallbacks, retaining only the required migration evidence.
2. Implement cloud idempotent CRUD/deletion lifecycle and durable AM/Billing
   prepare/commit/finalize/abort, quota reservations and resource fences. Replace
   immediate transfer/old-owner-admin behavior; remove all old-owner grants.
3. Implement Billing responsibility history/privacy projection, source/target
   amount confirmations, consent/setup fencing, profile reset, late compensation
   isolation, dedicated credentials and crash/replay recovery.
4. Implement the scoped My Clouds/Product UI and BFF, including request/cache/tab
   isolation and hosted-return binding.
5. Reconcile the preserved unpublished identity correction as forward migration
   051 (do not edit released 049), then run full migration/coverage/CI, coordinated
   backup/restore checks, and staging activation/device/certificate/MQTT acceptance.
   Legacy-table cleanup remains a separate post-acceptance operation.

No implementation PR is merged or deployed at this checkpoint. Neither passing
API tests nor the isolated financial predicate means the overall plan is complete.
