# Multi-cloud Account Manager implementation design

Status: design-first target, not deployed acceptance. Canonical behavior is
[multicloud_ownership.md](https://github.com/hkt999rtk/rtk_cloud_contracts_doc/blob/codex/multicloud-owner-design/multicloud_ownership.md) in
[contracts PR #131](https://github.com/hkt999rtk/rtk_cloud_contracts_doc/pull/131).
Runtime work follows reviewed/merged docs-only PRs.

## Persistence and concurrency

Keep users, organizations and organization_members as the authoritative identity,
resource and ownership sources. Derive owner_user_id from the sole owner member;
do not create an independently writable ownership column. Add description, soft
deletion, lifecycle/ownership versions and durable operation records. Business
enable/disable state is distinct from a lifecycle fence.

A deferred constraint validates exactly one owner membership per non-deleted
brand_cloud at commit, including pending/disabled owners. Lock the organization
row before every membership/owner mutation, including direct SQL, to prevent
concurrent count-check races. Cover insert/update/delete and organization-kind
changes without changing legacy customer_org semantics. Operational access also
requires an enabled, verified, non-pending owner and an operational cloud.

Creation locks the user before counting non-deleted owned clouds. Incoming
transfers reserve quota; count reservations against concurrent creates/accepts.
Lock affected users in stable UUID order, then the cloud. Consume the reservation
at ownership commit; release only canceled precommit reservations. Shared clouds
do not consume owned quota. Filtered pagination total is separate from owned count.

Add viewer to membership/invitation constraints and permission catalog. Persist
owner-approved all-products/selected-Product scope separately from operational
Product roles. Migrate existing admin/member scope only from explicit Product
grants, never from membership alone; report ambiguous access for approval.
Accepted viewer scope itself grants reads and requires no Product assignment.
It is authoritative for viewer read projection, not a second ownership source.
Pending invitation replay compares cloud, target, role and the complete normalized
viewer scope (kind and selected Product UUID set, ignoring order). Changed scope
conflicts without changing the pending invite or sending mail; cancel/recreate is
required. Resend rotates only the token, preserving scope. Acceptance uses the
persisted scope. Test equivalent reordered IDs, changed Product sets, all-to-selected
conflict and cancel/recreate; old tokens must fail and only the new scope is granted.
PATCH /v1/developer/brand-clouds/{cloudId}/members/{userId} with access_scope
atomically replaces the current viewer's whole scope under the cloud lock.
Selected IDs are nonempty, unique and belong to the cloud. All-to-selected or
removing an ID invalidates excluded-resource grants/invitations, queued access
and caches via the authorization version; it never revives stale Product roles.
Scope-only PATCH on another role is rejected; changing away from viewer removes
the viewer grant without granting admin/member new Product permissions.
Product invites cannot create/re-enable cloud membership or expand that scope.
Removal invalidates grants, pending invitations and authorization version in one
transaction; rejoining does not revive them. Role/disable mutations invalidate
activation holds under the identity correction design.

## API and operations

Extend the existing developer namespace with filtered list and creation for any
eligible enabled/verified global developer, including one with no memberships
or only shared-cloud memberships. Creation is quota-checked, not gated on already
being an owner somewhere. Name/description PATCH, deletion-preflight, DELETE and
cloud management remain owner-only. Add operation status. Name
changes never mutate cloud UUID/slug. Generic membership/platform provisioning
cannot assign owner except atomic new-cloud creation; transfers use only the
handoff commit. Register invokes exactly the signup service/request/202 response,
including CAPTCHA, pending retry, sole-owner creation and transactional email.

Create and lifecycle mutations require Idempotency-Key scoped by actor/cloud/type;
same key with changed payload conflicts. DELETE creates a durable operation and
returns 202 after rechecking advisory preflight. Fence new descendant resources
and conflicting lifecycle work; require no uncleaned Product/device/job and a
Billing zero-balance/settled closure. Tombstone only after Billing closes; retain
all audit/mapping/monetary history. No nonempty-cloud cascade delete.

## Ownership handoff

Retain request/status/cancel/accept routes, replacing immediate role swap with
versioned preview, explicit confirmation by both parties and a durable operation.
Store source/target IDs, ownership version, amount/currency/Billing version,
reservation, cutoff and last completed phase. Phases are requested,
awaiting_acceptance, preparing, awaiting_balance_confirmation, committing,
finalizing, succeeded, blocked and canceled. A blocked operation also stores its
resumption phase. Only one nonterminal lifecycle operation holds a cloud fence.

Require Billing to confirm that the source owner has settled all outstanding
charges and that available cloud credit is nonnegative (`balance_minor >= 0`).
Check financial eligibility at request and acceptance; revalidate fenced settlement
and the confirmed nonnegative snapshot/version before sole-owner commit. Zero is
eligible; negative balance yields `balance_negative`. Nonnegative credit never overrides
unsettled usage, debt, pending payments/refunds/disputes or missing evidence.
If final settlement makes credit negative, keep the original owner and block
commit. Complete precommit cancellation/remote hold release before allowing the
original owner to settle/top up normally and start a new transfer. No ownership
or target quota consumption commits while any financial condition is unsatisfied.

After authenticated target acceptance, durable outbox/inbox messages prepare
Billing and cost-producing resource services. Messages bind operation/cloud ID,
ownership version, cutoff and reconciliation evidence. Drain/fence producers;
missing usage completeness or settlement proof blocks commit. Final amount
changes require both parties to reconfirm the new snapshot.

The commit transaction replaces the sole owner, consumes reserved quota,
increments ownership version, removes old-owner membership/Product grants and
pending invitations, transfers their Product-owner assignments to the new owner,
and emits the committed event. Other collaborators retain existing permissions.
Operational access remains fenced until Billing finalization acknowledgment.
Billing still applies responsibility-period visibility after an owner role is
granted: ownership does not expose predecessor invoices, ledger details, payment
intents/attempts, payment-method metadata, activity, statements or downloads.
New owner sees confirmed opening balance and own-period records only; mixed or
unknown periods are withheld unless safely projected. Retained full history is
for separately audited platform access. The user approved this financial-history
partition together with the design review on 2026-08-31; implementation remains
gated on the complete docs merge.

Before commit, cancellation requires confirmed release of remote holds. After
commit, retry finalization instead of reverting ownership. Timeout never releases
a fence; recovery checks durable committed version. Current persisted scope must
invalidate old-owner JWT/cache/delegated/service-account grants and queued work
derived from that membership, without revoking unrelated global sessions/clouds.

## Migration and verification

Use forward migration; preserve applied markers and unpublished identity
corrections for reconciliation with this design. Report zero/multiple owners,
invalid operational owners, orphan Product owners, ambiguous ACL/invitations,
pending transfers and quota conflicts. Never choose an owner or activate an
account automatically. Pending/disabled sole owners remain non-operational.

Test direct SQL and concurrent create/transfer/member invariants, totals/quotas,
register/signup parity, viewer and future-Product scopes, revocation/rejoin,
old-owner Product removal, stale/replayed messages, crashes at every handoff
phase, unavailable Billing/resource evidence and deletion races. Cross-service
and browser evidence precede coordinated staging deployment and legacy cleanup.
Transfer tests reject settled -1, allow 0/+1 only without other blockers, and
reject nonnegative-to-negative settlement races. A positive-to-zero change
requires fresh confirmation, not permanent rejection. Deletion continues to
require zero balance; do not reuse the transfer predicate for deletion.
