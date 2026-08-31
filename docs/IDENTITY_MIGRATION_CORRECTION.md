# Unified identity migration correction design

This service design refines the existing unified-human-identity requirements in
`SPEC.md` and the canonical `AUTH.md` / `AUTHORIZATION.md`. It does not add a
tenant login, change public signup, or authorize a staging migration by itself.
Implementation and deployment follow review of this design.

## Verified gaps in migration 049

Isolated PostgreSQL cases against the published migration demonstrate:

- A verified legacy account with an empty or malformed password hash becomes a
  verified global user instead of an activation/reset-required account.
- Two legacy identities mapping to one global user and one target Brand Cloud
  can produce multiple rows for one `organization_members` conflict target.
  PostgreSQL refuses that statement before role precedence can be applied.

The same fixture suite verifies that identical trusted hashes merge, different
hashes require reset, existing global credentials/verification are preserved,
and a sole-owner credential conflict aborts the migration transaction. The
database already enforces normalized emails on both identity tables; tests do
not drop those constraints to manufacture unsupported input.

## Fresh cutovers

The corrected 049 source keeps the approved identity model and transaction:

1. Existing global users keep their password and verification state. Their
   credentials are not replaced using legacy tenant credentials.
2. A new global user inherits a hash only when every merged source is verified,
   no source is pending activation, all hashes are identical, and the common
   hash has a supported bcrypt representation: `$2a$`, `$2b$` or `$2y$`, a
   two-digit bcrypt cost in 04–31, and the 53-character bcrypt salt/hash body.
   Format validation does not claim knowledge of the original plaintext.
3. Empty, malformed, conflicting or unverified source credentials produce a
   non-authenticating reset marker, `email_verified=false`, and
   `signup_pending_verification=true`. Mapping records say
   `password_reset_required`; they never store a plaintext password.
4. Before the membership upsert, reduce source rows to one row per
   `(organization_id, global_user_id)`. Merge roles by `owner > admin > member`,
   preserve the earliest creation timestamp, and retain the existing disabled
   state rule: any eligible enabled source keeps the merged member enabled;
   otherwise retain the disabled state. Reset-required identities remain
   disabled by the cutover policy. Apply the same precedence against an
   existing global membership.
5. Preserve ACL mapping, original mapping outcomes on replay, historical audit
   actors, token/certificate revocation, and the final enabled/verified owner
   assertion. An owner conflict aborts all writes in the transaction.

The published migration number is not deleted from an existing database to
force execution. Corrected 049 is for fresh databases and pre-cutover restores;
already-applied databases use the forward correction below.

## Already-applied databases: forward correction before cleanup

Add a new migration after 050 rather than rerunning 049 on a live database.
The preflight report classifies a correction candidate only when all of these
are true:

- Its persisted mapping outcome is `created_user`, not `existing_user`.
- The legacy source group has one identical, malformed bcrypt hash, with the
  verified/non-pending source state that allowed the old 049 to copy it.
- The current global hash still equals that legacy hash and the current user
  is still verified/non-pending. A user who has since activated, reset a
  password, or otherwise changed credentials is not overwritten.

For those exact candidates, atomically set the reset-required account state,
replace the unusable hash with a non-authenticating marker, disable affected
memberships, revoke their global refresh grants, and update all corresponding
mapping conflict statuses while preserving original mapping outcomes. Record
the correction reason and affected global/legacy IDs in a new audit event;
do not rewrite historical events or include raw hashes, tokens or passwords.

Preflight and postflight both check active Brand Clouds for an enabled,
verified, non-disabled global owner. If correction would remove the final
valid owner, stop and resolve ownership/activation through the approved global
flows before retrying. Do not bypass verification in the database. Do not
enable disabled users or change unrelated global users/memberships.

Legacy tables and the migration mapping remain available until correction and
the full identity acceptance checks pass. Replaying the forward correction
does not emit another correction event or reset an already-corrected user.

## Verification and rollback

Required isolated PostgreSQL evidence includes:

- Single tenant, same-email/same-hash, different-hash and unverified sources.
- Empty/malformed hashes and valid bcrypt representations.
- Existing verified and unverified global users with unchanged credentials.
- Duplicate target memberships, collisions with existing global memberships,
  and all three role-precedence levels.
- Equivalent global ACL assignments, disabled legacy assignments, unchanged
  historical audit actor/subject/payload, and retained migration mappings.
- Sole-owner conflict: no partial user, membership, mapping, token revocation,
  or applied-migration marker survives the refused transaction.
- Forward repair of an unchanged bad migrated credential; preservation of an
  existing global user and a migrated user whose password has since changed;
  correction audit, refresh revocation, owner refusal and idempotent replay.

Before shared staging execution, freeze account writes and make a fresh
recoverable database backup. Run preflight on a restored copy, compare counts
and mapping/owner/ACL assertions, then coordinate service deployment with the
approved maintenance window. On failure, use the matched database backup and
previous service versions; a code-only rollback is not a credential rollback.
No legacy table cleanup proceeds on a failed or incomplete assertion.
