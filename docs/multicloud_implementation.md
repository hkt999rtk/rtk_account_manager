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

Platform new-cloud creation explicitly requires `owner_user_id` for an existing,
enabled, verified global user, locks and enforces that user's ownership quota,
and commits cloud, sole-owner membership and operator audit together. It never
defaults the financial owner to the platform operator or grants that operator
membership. A new developer uses the public email-activation onboarding first.
An ownership field in cloud PATCH is rejected; it cannot bypass handoff.

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

The dedicated Billing client implements the advisory eligibility boundary through
`POST /v1/internal/billing/clouds/{orgId}/ownership-eligibility`. It binds the
current owner/version, target account, request/accept action and transfer ID to
fresh independently collected evidence. Missing, malformed, stale or mismatched
responses fail closed; this query neither reserves funds nor authorizes commit.
The response uses an explicit snake_case JSON contract, also used for newly
recorded request/acceptance audit snapshots. Existing immutable snapshots are
not rewritten and are not used to bypass live revalidation. Installing this
client alone does not enable admission: the reviewed producer inventory and
authenticated preparation/release adapters remain mandatory.

Human provision/deactivate admission must lock the actor, cloud and device and
recheck the specific lifecycle permission in the same transaction that creates
the operation/outbox and pending projection. A request admitted by middleware
before revocation or handoff cannot enqueue work after the authority changes.
Missing human actors are rejected; platform capability alone is not Brand Cloud
authority. Successful new operations have an atomic audit event; idempotent
replays do not emit another event. This admission fence does not certify draining
previously admitted work: producer consumption/cutoff acknowledgments are separate.

Claim resolution uses the same actor/cloud lock order before locking a claim
token, rechecks `claim.resolve`, and enforces the token's Product within the
owner-approved collaboration scope. It atomically audits the claim without
recording the token or provisioning secrets. Unprovision rechecks
`device.unprovision` before locking the device/claim and enqueueing removal.
The separately audited platform override must revalidate current global platform
authority under lock; it cannot bypass an inactive or lifecycle-fenced Brand
Cloud. All paths resolve and recheck the device's cloud under the cloud/device
locks. Retained legacy identity rows cannot authorize these human operations.
App end-user claim remains a separate admission path against `end_users`, never
global human membership. Its claim consumption, Brand link, device binding and
App-actor audit commit in one transaction. A disabled end-user or inactive/fenced
cloud cannot consume a token; link/binding failure must leave the token retryable.

Privileged claim transfer/reclaim must recheck the active global platform actor
inside the mutation transaction. Resolve the source without taking resource
locks, lock source/target clouds in sorted order, then lock and revalidate the
device, claim and token. Both Brand Clouds must be operational (including no
pending handoff/deletion), and any Brand Cloud destination must own the retained
Product. The operation does not move or infer a Product, create a second owner,
or bypass Billing handoff. Legacy customer manufacturer-profile semantics stay
unchanged. A changed source or mismatched claim/device/token scope is a conflict,
not permission to act on a cloud that was not locked. Audit failure rolls back
the entire device/token/claim mutation.

Platform claim-token creation/revocation use the same transaction-local platform
authority check. Lock all affected clouds (explicit organization and the Product's
manufacturer cloud) before Product/token rows, and revalidate the observed scope.
A Product-bound Brand Cloud token must reference that cloud's Product; legacy
customer manufacturing tokens retain their independent manufacturer relationship.
Unbound platform manufacturing tokens remain supported. Creating/revoking tokens
cannot bypass an inactive cloud or handoff/deletion fence. Audit token IDs and
scope only, never claim secrets or provisioning material; audit failure rolls
back the token write. Repeated revocation retains its original timestamp and
does not emit duplicate audit. Low-level fixture/bootstrap persistence is not
included in the HTTP persistence interface and must not authorize human requests.

Product create/update/disable reauthorize inside their mutation transaction with
actor -> cloud -> Product locks. Existing Product edits require current Product
admission and `registry_device.manage`; viewer ceilings still apply. Creating a
new Product requires cloud-wide creation authority, not an assignment to some
existing Product (the current admission model grants this to the cloud owner).
The explicit platform-admin route rechecks current platform capability and cloud
operational state; an ordinary cloud route cannot select that override. Product
PATCH reads its current state under lock so concurrent disjoint patches do not
overwrite one another. Product mutation, creator assignment and audit are atomic.

Factory production-run issuance uses that same Product write authority, cloud
fence and lock order. Validate the scoped active Product inside the transaction,
insert the run and audit, and sign against the locked Product snapshot before
commit. The issuer is a trusted in-process signing callback, never a network
call or browser-supplied implementation. Missing/failed/empty signing rolls back
the run; commit failure must not return the locally generated JWT. Only a
successful commit can publish the token response. This issuance fence is not
proof of factory consumption authorization: consumers must still enforce current
run/cloud/ownership authority and handoff cutoffs for previously issued tokens.

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
