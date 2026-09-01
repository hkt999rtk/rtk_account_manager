# Identity correction 051: operator runbook

Implements the reviewed [correction design](identity_migration_correction.md).
This runbook does not authorize a shared-environment deployment. Complete the
release review and maintenance-window approval first. Do not delete an applied
049 marker, rerun corrected 049 on a deployed database, or drop legacy evidence.

## Preparation and rollback boundary

1. Record the immutable current and candidate service/image revisions. The
   candidate Account Manager must contain migration 051, the multi-cloud migrations
   through 063, and the pending-user, cloud-eligibility, membership and activation-hold
   guards. Include all dependent services in the
   coordinated release acceptance, not just the migration executable.
2. Freeze account and membership writes, including email verification, reset,
   provisioning, OIDC linking/sign-in and administrative changes. Stop background
   writers that can alter this state. A migration transaction is not a substitute
   for the maintenance freeze.
3. Take a fresh recoverable database backup and verify restoration to an isolated
   database. Keep the backup paired with the previous service versions. Restrict
   access: it contains credentials and identity data. Do not put a database URL,
   password hash, token or certificate key in the PR or diagnostic report.
4. Use the candidate executable against that restored copy, migrated through
   `050_backfill_immediate_brand_account_acl.sql`. Do not run ordinary `migrate`
   merely to prepare an unknown copy: it would also commit 051 and later files.
   A restore that already has the multi-cloud migrations is also supported; never
   remove their markers to force a different order.
5. For a pre-049 restore, stop and inventory the published cutover separately.
   This implementation does not patch released 049. Its duplicate membership
   target and conflicting sole-owner activation cases can fail before 051 runs.
   Do not deduplicate legacy evidence, bypass verification, or mark 049 applied to
   evade that failure. A reviewed pre-cutover path and its tests remain a release
   gate. Empty new databases and compatible legacy fixtures apply normally.

## Rollback-only preflight

Supply the normal migration configuration through the approved secret/config
mechanism, with `DATABASE_URL` targeting the isolated restored database. The
existing configuration loader also requires the configured JWT signer settings;
do not reuse or expose production signer secrets merely for a local fixture.
From the service root, with the candidate migration files available:

```sh
go run ./cmd/migrate --identity-preflight
```

This mode does **not** call the normal committing migration runner. It executes
the exact 051 SQL in one repeatable-read transaction and rolls it back, including
schema/trigger changes and audit writes. It forces deferred constraints before
reporting readiness, but never writes a migration marker.
It can still acquire locks and do substantial work; run it on the restored,
write-frozen copy, not as a harmless production read query.

The JSON result identifies the migration and includes `ready`, `rolled_back`,
candidate user count, activation-hold counts before/after, and refresh/certificate
revocation counts. `ineligible_owner_clouds` reports designated-owner clouds
that must remain operationally fenced; it is not an instruction to reassign or
activate those owners. A controlled refusal includes `reason` and `blocking_ids`
(global user IDs, Brand Cloud IDs, or organization/user pairs). Counts after a
refusal are not a complete impact inventory: execution stops at the first
failed assertion. Exit zero requires readiness; refusal/error exits nonzero.
Accept only a successful result with **both** `ready=true` and `rolled_back=true`.
Record the restricted report and backup/revision identifiers in the release
evidence, then independently compare owner, membership and ACL mappings.

## Resolve refusals, never bypass them

- **Zero or multiple designated owners:** every non-deleted Brand Cloud must have
  exactly one owner membership, including disabled clouds. Resolve ambiguity
  through reviewed ownership evidence, never by choosing an arbitrary owner.
  A pending/disabled owner keeps ownership and Billing responsibility. Correction
  may suspend that sole owner; all tenant actors must then fail the cloud
  eligibility gate until approved activation restores eligibility. Never set
  verification flags directly or silently transfer responsibility.
- **Adopted SSO account with unsafe inherited local credentials:** use approved
  global credential remediation. Preserve SSO verification and memberships.
  Linked identities or post-cutover OIDC audit evidence block automatic account
  reset; excluding those rows does not fix the unsafe local credential.
- **Unresolved suspension provenance:** compare the pre-cutover backup and
  administrative audit history. A disabled membership alone is not proof of an
  activation suspension. Retain disabled access while the case is unresolved.
- **Conflicting resolution records:** reconcile the evidence and append one
  authorized later decision. Do not rewrite/delete the historical decisions.

For legacy ambiguity, the controlled maintenance procedure may append an
`identity_activation_hold_resolved` audit event after an authorized operator
reviews the evidence. This is not a public API, an automatic remediation, or
permission for the migration runner to invent an administrative decision.
Use a restricted database maintenance session and the real approving actor ID.
The event must contain:

| Field | Required value |
| --- | --- |
| `event_type` | `identity_activation_hold_resolved` |
| `actor_user_id` | Real authorized operator's global user ID |
| `organization_id` | Exact reviewed Brand Cloud ID |
| `subject_type`, `subject_id` | `user`, exact reviewed global user ID |
| Payload `decision` | `keep_disabled` or `restore_after_verification` |
| Payload `source` | `identity_migration` or `provisioning` |
| Payload `evidence` | Nonempty restricted backup/audit/review reference; no secrets |
| Payload `disabled_at`, `updated_at` | Exact current membership timestamp text |

Lock and re-read the specific membership in the same transaction that appends
the event; compare it to the approved evidence before recording the decision.
Capture timestamp strings from PostgreSQL (`disabled_at::text`,
`updated_at::text`), without client rounding, using the same session timezone as
the migration runner (normally UTC). The audit creation time must not precede
the membership update. Any intervening membership edit invalidates the decision.
Never use a bulk user-wide resolution or a fabricated approving actor.

`keep_disabled` overrides even automatic legacy backfill. The alternative only
permits a hold: it does not immediately enable a membership or verify an account.
The latest decision is authoritative; contradictory decisions with equal latest
timestamps are refused. Missing evidence/actor, stale timestamps, or an invalid
latest decision cannot authorize recovery. Re-run preflight after each approved
resolution until there are no unresolved cases. Reflect approved remediation in
the frozen source through the same controlled procedure, take/restore a fresh
backup, and repeat the complete preflight before deployment.

## Coordinated application and acceptance

Apply 051 only with the corrected Account Manager and all required consumers
ready, while the write freeze remains in effect. The normal migration runner
commits each file atomically and records 051 only on success. An owner or
provenance refusal rolls back that file. A successful 051 records the provenance
boundary. Subsequent 051 runs do not re-infer old
permissions or repeat credential correction events.

The runner commits files separately: failure of a later migration does not roll
051 back. Retain the maintenance freeze until the complete schema and matching
services are ready; never resume the previous service against a partially upgraded
database. Use the matched backup/service rollback if recovery cannot finish.

Before resuming traffic, verify migration/mapping counts, exactly one designated
owner for every non-deleted Brand Cloud, denied access for all actors of an
ineligible-owner cloud, unchanged historical audits, equivalent eligible ACL access,
and correction-event IDs. Verify candidate refresh grants/app-user certificates
are revoked and old access JWTs cannot bypass pending/disabled state. Verify email
activation restores only unchanged recorded holds; administrative disables and
role changes stay disabled. Reset alone must not release a hold. Revoked sessions
and certificates remain revoked after activation.

Run global signup/owner email activation/login, membership and multi-view checks,
device association, certificate enrollment and representative MQTT/load tests.
Confirm tenant auth routes return 404 and App end-user authentication remains
independent. Record actual deployed revisions and results, not only local tests.
Keep legacy tables, mapping/audit evidence and backups until all identity-plan
acceptance gates pass; cleanup is a separate reviewed migration.

If any application or acceptance step fails, retain the traffic freeze and
restore the matched backup **and** prior services together. A code-only rollback
cannot restore changed credentials, verification state or revoked grants.
