# Delegated Batch Job Authorization

Status: implementation design constrained by the canonical
`rtk_cloud_contracts_doc/batch_jobs.md` contract.

Account Manager remains the authority for the human actor, Brand Cloud,
Product scope, capability, authorization version, and ownership version. Cloud
Admin never persists a user access/refresh token for background execution.

## Admission And Exchange

An authenticated developer admits one immutable authorization using the job ID,
normalized scope digest, one required capability, Product IDs, and an expiry no
later than seven days. Admission locks and rechecks the cloud and effective user
scope, records both versions, and is idempotent by cloud, actor, and job ID.

Cloud Admin exchanges the authorization through a dedicated internal credential.
Exchange rechecks the exact binding and current authority and returns a
refresh-less five-minute access token with subject type `delegated_job`. Claims
include authorization ID, job ID, cloud ID, scope hash, capability, Product IDs,
and both versions. Normal API authorization intersects these claims with current
database authority. Delegated tokens cannot access auth/session, collaboration,
ownership, billing, or authorization-admission routes.

Revocation is idempotent. Expiry, membership loss, disabled membership, Product
scope loss, cloud state change, authorization-version change, or ownership-version
change makes exchange and delegated API use fail closed. Only the affected job
and cloud are invalidated.

## Persistence And Audit

`job_authorizations` stores immutable binding fields, state, timestamps, and
revocation reason. It stores no bearer token. Admission, exchange denial,
revocation, and version invalidation write safe audit events without token or CSV
content. Cleanup may retain terminal rows for audit but cannot reactivate them.

