# Unified identity migration correction design

This service design refines the existing unified-human-identity requirements in
`SPEC.md` and the canonical `AUTH.md` / `AUTHORIZATION.md`. It does not add a
tenant login, change public signup, or authorize a staging migration by itself.
Implementation and deployment follow review of this design.
The multi-cloud target in [MULTICLOUD_IMPLEMENTATION.md](MULTICLOUD_IMPLEMENTATION.md)
supersedes this document's former active-owner cutover gate. Historical findings
below describe the published migration, not the new designated-owner invariant.

## Verified gaps in migration 049

Isolated PostgreSQL cases against the published migration demonstrate:

- A verified legacy account with an empty or malformed password hash becomes a
  verified global user instead of an activation/reset-required account.
- Two legacy identities mapping to one global user and one target Brand Cloud
  can produce multiple rows for one `organization_members` conflict target.
  PostgreSQL refuses that statement before role precedence can be applied.
- Global email activation currently clears every disabled membership for the
  user, including an administrative disable. PostgreSQL cases reproduce this
  for both newly migrated and pre-existing global users. Activation succeeds,
  but restores permissions that were not suspended for email verification.

The same fixture suite verifies that identical trusted hashes merge, different
hashes require reset, existing global credentials/verification are preserved,
and a sole-owner credential conflict aborts the migration transaction. The
database already enforces normalized emails on both identity tables; tests do
not drop those constraints to manufacture unsupported input.

## Pre-cutover design target and implementation boundary

The pre-cutover correction must keep the approved identity model and transaction:

1. Existing global users keep their password and verification state. Their
   credentials are not replaced using legacy tenant credentials.
2. A new global user inherits a hash only when every merged source is verified,
   no source is pending activation, all hashes are identical, and the common
   hash has a supported bcrypt representation: `$2a$`, `$2b$` or `$2y$`, a
   two-digit bcrypt cost in **10–12**, and the 53-character bcrypt salt/hash
   body. The service generates cost 10 (`bcrypt.DefaultCost`); this migration
   policy rejects weaker legacy work factors and bounds inherited verification
   work to at most four times cost 10. Costs below 10 or above 12 require reset
   without attempting an expensive password comparison. Format/work-factor
   validation does not claim knowledge of the original plaintext and does not
   change an existing global user's credential policy.
3. Empty, malformed, conflicting or unverified source credentials produce a
   non-authenticating reset marker, `email_verified=false`, and
   `signup_pending_verification=true`. Mapping records say
   `password_reset_required`; they never store a plaintext password.
4. Before the membership upsert, reduce source rows to one row per
   `(organization_id, global_user_id)`. Merge roles by `owner > admin > member`,
   preserve the earliest creation timestamp, and retain the existing disabled
   state rule: any eligible enabled source keeps the merged member enabled;
   otherwise retain the disabled state. Eligible reset-required memberships
   are temporarily suspended with explicit activation provenance (below).
   Administratively disabled sources do not receive that provenance. Apply
   the same precedence against an existing global membership.
5. Preserve ACL mapping, original mapping outcomes on replay, historical audit
   actors and token/certificate revocation. Validate exactly one designated owner
   for every non-deleted Brand Cloud; zero/multiple-owner conflicts abort all
   writes. Pending/disabled sole owners survive but their clouds remain fenced
   from operational access until owner eligibility is restored through approved
   flows. Legacy customer-organization invariants are unchanged.

Published 049 and its applied marker remain unchanged. The implemented forward
051 covers databases already through 050; it does not repair a failure inside
049 before that point. A pre-049 legacy restore with duplicate membership targets
or a reset-required sole owner needs a separately reviewed cutover path satisfying
the rules above. It remains a release blocker, not a reason to rewrite legacy
evidence or bypass the old assertion. Empty new databases apply the normal chain.
See [the operator runbook](IDENTITY_CORRECTION_RUNBOOK.md).

## Already-applied databases: forward correction before cleanup

Add a new migration after 050 rather than rerunning 049 on a live database.
The preflight report classifies a correction candidate only when all of these
are true:

- Its persisted mapping outcome is `created_user`, not `existing_user`.
- The legacy source group has one identical bcrypt hash outside the supported
  representation/work-factor policy (including empty or malformed hashes), with the
  verified/non-pending source state that allowed the old 049 to copy it.
- The current global hash still equals that legacy hash and the current user
  is still verified/non-pending. A user who has since activated, reset a
  password, or otherwise changed credentials is not overwritten.
- No linked `user_identities` record or post-cutover OIDC sign-in evidence
  indicates account adoption. OIDC can claim the global account without
  changing its password hash or verification flags; those accounts are not
  unchanged correction candidates. Report them separately for credential
  remediation through the approved global flows, preserving SSO access and
  memberships. Do not mark an adopted SSO identity pending automatically or
  silently count its unsupported password as repaired. An adopted account
  whose unsafe inherited local hash is still active is a **blocking preflight
  condition**, not an allowed exclusion: complete approved password reset or
  explicitly disable only that local credential before retrying. Neither the
  forward migration nor account-traffic resumption proceeds with such a hash
  still usable. SSO verification state and memberships remain unchanged.

For those exact candidates, atomically set the reset-required account state,
replace the unusable hash with a non-authenticating marker, disable affected
memberships, revoke their global refresh grants and active app-user
certificates, and update all corresponding
mapping conflict statuses while preserving original mapping outcomes. Record
the correction reason and affected global/legacy IDs in a new audit event;
do not rewrite historical events or include raw hashes, tokens or passwords.

Preflight and postflight validate exactly one designated owner membership for
every non-deleted Brand Cloud, regardless of owner activation/disable state.
Zero/multiple-owner conflicts block cutover. Separately record operational
eligibility: owner membership and global user are enabled, email is verified,
and signup is not pending. A pending/disabled sole owner passes cardinality but
its cloud cannot resume operations for any tenant actor. Credential correction
can suspend that owner without replacing/removing ownership. Deploy the cloud
eligibility guard before traffic resumes; reject cutover if this denial cannot
be proven. Do not bypass verification, automatically activate or reassign owners,
or change unrelated users/memberships. Legacy customer_org retains its gate.

Revoking refresh grants alone does not stop an already-issued access JWT.
The service change accompanying the forward migration must enforce the current
global user and membership state on every human authorization path:

- Global bearer authentication and refresh reject a disabled or
  `signup_pending_verification` user even when the JWT signature/expiry or
  refresh grant is otherwise valid. Activation endpoints remain available
  through their one-time email tokens, not the pending user's access JWT.
- `GetRole`, organization authorization and organization-scoped ACL permission
  evaluation require an enabled membership and an enabled, non-pending global
  user. A still-active global `role_assignment` cannot bypass a suspended or
  administratively disabled membership. Platform-only ACL capabilities remain
  independent of membership but still require an eligible global user.
- Certificate enrollment and services consuming delegated authorization must
  honor the same current-state denial. Validate existing JWT, refresh and
  certificate paths, not just the login form. Activation restores only the
  eligible memberships below; it never resurrects revoked grants/certificates.

The maintenance sequence must deploy these guards with the correction before
account traffic resumes. Do not run the forward SQL against an older service
that continues accepting pending users or ignores disabled memberships.

Legacy tables and the migration mapping remain available until correction and
the full identity acceptance checks pass. Replaying the forward correction
does not emit another correction event or reset an already-corrected user.

## Activation suspension is not administrative revocation

Email verification proves control of the account; it does not authorize
rejoining a Brand Cloud from which an administrator removed access.

- Introduce an internal `organization_member_activation_holds` table keyed by
  `(organization_id, user_id)`, referencing the membership. Store the exact
  `disabled_at` and `updated_at` values written by the activation suspension,
  its source (`signup`, `provisioning`, or `identity_migration`), and creation
  time. Do not infer an activation hold from `disabled_at IS NOT NULL` alone.
- The pre-cutover correction and forward 051 create this table idempotently. The
  migration, public signup and global-user provisioning write the hold in the
  same transaction as any membership they suspend solely for verification.
  Current developer signup creates an enabled owner membership; it needs no
  hold while the global pending-user authorization guard blocks access.
  Existing administrative disables never become activation holds. Suspending
  an otherwise enabled membership is distinct from preserving an old disable.
- `VerifyEmailToken` only restores memberships with an explicit hold whose
  stored suspension timestamps still match the current membership. Delete
  consumed/stale holds in the activation transaction. An intervening role or
  disable change invalidates the hold; administrative membership mutations
  explicitly remove it. Never use an unconditional user-wide re-enable.
- Forward correction records holds only for memberships it newly suspends.
  Already-applied 049 suspensions need pre-cutover provenance: a `created_user`
  mapping, originally enabled legacy membership, and matching migration and
  current membership timestamps can establish an unchanged migration hold.
  Existing-global or ambiguous cases require comparison with the pre-cutover
  backup. The preflight report lists unresolved cases; it must not guess or
  silently restore rights. Retain the disabled state until an authorized
  membership action resolves ambiguity, and do not remove legacy evidence
  while these cases remain unresolved.
- Preflight also inventories in-flight global email provisioning, not only
  legacy migration rows. For an unverified global user, regardless of
  `signup_pending_verification`, an unchanged newly provisioned
  membership is eligible for a `provisioning` hold only when its `created_at`,
  `disabled_at` and `updated_at` all match a recorded
  `brand_cloud_account_created`/`brand_cloud_account_assigned` audit event for
  the same user and organization with `activation_mode=email`, and the global
  email-verification token issuance corroborates that transaction. The token
  may have expired (resend is allowed), but it must not have been consumed.
  An older administrative disable, later membership edit, missing provenance
  or inconsistent record is not backfilled automatically. Resolve those cases
  from backup/audit evidence or an authorized membership action before the
  verification behavior switches. Pause verification during this preflight;
  do not consume a pending token while its expected membership recovery is
  unresolved. Backfill is idempotent and never creates a hold for an already
  enabled membership or an already-verified account. In particular, the
  pre-upgrade `/v1/auth/register` flow can create an unverified, non-pending
  global user whose later email provisioning legitimately suspended a new
  membership. Do not exclude that state. Token-status and activation UI checks
  must allow its corroborated global email-verification token while preserving
  expiry, consumption and disabled-user checks. This recovery rule does not
  decide the future public-register compatibility contract.
- Password reset remains separate from email verification. A pending account
  must complete global email activation before its activation holds can be
  released; resetting its password alone must not grant membership access.

## Verification and rollback

Required isolated PostgreSQL evidence includes:

- Single tenant, same-email/same-hash, different-hash and unverified sources.
- Empty/malformed hashes, supported bcrypt representations at costs 10 and 12,
  and rejection of costs 04/09/13/31 without expensive password comparison.
- Existing verified and unverified global users with unchanged credentials.
- Duplicate target memberships, collisions with existing global memberships,
  and all three role-precedence levels.
- Equivalent global ACL assignments, disabled legacy assignments, unchanged
  historical audit actor/subject/payload, and retained migration mappings.
- Zero/multiple designated-owner conflict: no partial user, membership, mapping, token revocation,
  or applied-migration marker survives the refused transaction.
- Activation of eligible migrated/signup/provisioned memberships, preservation
  of administrative disables (including a disable after the hold was created),
  existing-global users, role changes, stale holds and activation-token replay.
- In-flight pre-upgrade provisioning: corroborated unchanged suspension is
  backfilled and activates; administrative/edited/ambiguous membership does
  not gain access; unverified pending and unverified non-pending globals,
  token-status/UI eligibility, expired-token resend and idempotent backfill
  are covered.
- An otherwise valid pre-correction global JWT, refresh grant and certificate
  cannot bypass the pending state; direct role/ACL checks cannot bypass a
  disabled membership; platform-only and App end-user boundaries remain valid.
- Forward repair of an unchanged bad migrated credential; preservation of an
  existing global user and a migrated user whose password has since changed;
  correction audit, refresh revocation, designated-owner preservation and idempotent replay.
- OIDC adoption with unchanged password/verification fields is excluded from
  automatic account-state repair and blocks cutover while its unsafe local
  credential remains active. After approved local-credential remediation,
  cutover preserves SSO state/memberships. Verified-but-pending sole owners
  satisfy cardinality but fail operational eligibility; all tenant actors are
  denied until approved activation. Zero/multiple-owner cases still abort.

Before shared staging execution, freeze account writes and make a fresh
recoverable database backup. Run preflight on a restored copy, compare counts
and mapping/owner/ACL assertions, then coordinate service deployment with the
approved maintenance window. On failure, use the matched database backup and
previous service versions; a code-only rollback is not a credential rollback.
No legacy table cleanup proceeds on a failed or incomplete assertion.
