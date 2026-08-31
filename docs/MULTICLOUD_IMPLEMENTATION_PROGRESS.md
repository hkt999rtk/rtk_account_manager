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
- Billing has a tested financial eligibility predicate: settled credit >= 0 for
  transfer; exactly 0 for closure; missing evidence and pending monetary/setup
  work remain independent blockers. This predicate is not yet connected to a
  durable handoff or an HTTP endpoint.

## Verification at this checkpoint

- Account Manager sole-owner/concurrency/eligibility/list tests passed repeated
  isolated PostgreSQL runs, including stale-repeatable-read writers and failed
  migration preflights. Published migration markers were not edited.
- Account Manager full API package passed with isolated PostgreSQL (38.846s).
  Covers current signup/register parity, encrypted email rollback, activation,
  global login, owner-only member listing, platform creation, device profiles,
  factory JWTs and App end-user multi-cloud binding.
- The last complete Account Manager suite is not green. Its remaining store
  failures include `TestBrandCloudOwnerTransferRequiresExistingTargetAndAcceptsWithLoggedInDeveloper`
  and `TestProductCollaborationInvitationVisibilityAndOwnershipTransferIntegration`.
  Their old transfer/admission semantics must be replaced by the approved flows,
  not preserved merely to pass tests. The two API fixture failures in that run
  were subsequently corrected and the full API package rerun passed.
- Billing full suite passed with a separate isolated PostgreSQL database. Its
  new eligibility tests cover -1/0/+1, int64 extremes, independent blockers,
  malformed/unknown evidence, and transfer-versus-deletion rules.
- Runtime default pre-PR/coverage gates, service CI and staging evidence are still
  outstanding. The successful documentation CI does not validate these runtime changes.

## Required next work

1. Complete explicit Product admission/viewer scope and consistent authorization,
   revocation/rejoin, queued work and cache invalidation. Retire legacy human
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
