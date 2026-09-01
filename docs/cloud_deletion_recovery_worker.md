# Cloud deletion recovery worker

Implementation checkpoint; this is not approval to enable staging deletion.
The canonical multi-cloud lifecycle contract remains authoritative.

## Execution boundary

`cmd/cloud-deletion-worker` processes existing `cloud_deletion_jobs`; it does not
serve HTTP, apply migrations, provision accounts, accept new DELETE requests or
create collector/producer evidence. `ConfigureCloudDeletionRecovery` cannot enable
new deletion admission, including if preflight observers are subsequently added.
The separately configured API still needs the reviewed complete resource inventory
and actual preflight observers before accepting new deletion operations.

The executable installs the dedicated Billing transport and the reviewed Video
Control Plane deletion producer. Missing captured participants still stop
preparation or cancellation, never becoming successful no-op acknowledgments. A
stored close command with all durable preparation evidence can be retried after
process restart without inventing new preparation evidence. Known Billing closure
only finishes AM tombstoning; it never rolls back to an active Billing account.

## Configuration and startup

Run the separately built `rtk-account-manager-cloud-deletion-worker` only in the
reviewed environment with the following settings supplied by its secret/config
manager. No command here copies secrets or starts a staging service.

| Setting | Requirement/default |
| --- | --- |
| `DATABASE_URL` | Explicit; no implicit localhost/shared-database fallback |
| `BILLING_HANDOFF_BASE_URL` | Trusted credential-free HTTPS origin; literal loopback or exact `.svc.cluster.local` HTTP is allowed for isolated/internal Kubernetes transport |
| `BILLING_HANDOFF_TOKEN` | Dedicated credential, not a login or other service token |
| `VIDEO_CONTROL_PLANE_HANDOFF_BASE_URL` | Trusted Video Control Plane internal origin |
| `VIDEO_CONTROL_PLANE_HANDOFF_TOKEN` | Dedicated participant credential, distinct from Billing and user credentials |
| `CLOUD_DELETION_WORKER_POLL_INTERVAL` | `5s`, positive and at most one minute |
| `CLOUD_DELETION_WORKER_LEASE_DURATION` | `2m`, between 30 seconds and five minutes |
| `CLOUD_DELETION_WORKER_STEP_TIMEOUT` | `45s`, plus five-second finish margin strictly below lease duration |
| `CLOUD_DELETION_WORKER_BATCH_SIZE` | `10`, between 1 and 128 |

Malformed explicit durations/integers fail startup. Migration
`063_cloud_deletion_worker_wake.sql` must already be applied by the separate
reviewed migration procedure; the worker refuses an older schema. Build/release
bundles include the executable. The LKE deployment installs one worker with the
handoff feature set and restricts its participant ingress through NetworkPolicy.

## Retry and shutdown

- Claiming uses database-time leases and `SKIP LOCKED`. Claimed jobs start
  concurrently within the configured batch bound; a later job does not wait out
  its lease behind slow earlier HTTP calls.
- Claims and coordinator attempts have bounded deadlines; lease finish has its
  own five-second deadline. A step timeout can persist retry using the still-live
  process context. Database failures leave persisted leases recoverable.
- Retry delay doubles from five seconds to at most five minutes using the persisted
  claim count. There is no maximum-attempt deletion, dead-letter terminal state or
  timeout-based authorization/hold release.
- Protocol phase changes, retirement receipts and release receipts increment a
  scheduling generation. A stale worker's finish cannot overwrite that wakeup
  with backoff. Blocker-only status updates do not wake a hot retry loop.
- SIGINT/SIGTERM cancels in-flight work and waits for bounded operations to return.
  Unfinished leases are not reported as success or cleared by shutdown. Another
  worker reclaims them after expiry and reuses the same persisted command IDs.
- Structured logs include operation/cloud IDs and coarse outcomes only. Raw
  database, provider and transport diagnostics or credentials are not logged.

## Verification and remaining release gates

Unit/race tests exercise bounded parallel work, timeout, shutdown, claim failure,
lease loss, finish failure, capped retry and log filtering. Isolated PostgreSQL
tests exercise cancellation wake versus stale backoff, recovery-only admission
denial, absent producer evidence and fixed-command recovery. An actual subprocess
test starts the executable entry point and sends SIGTERM using a dedicated empty
database; it proves no remote work is fabricated. The real AM/Billing four-mode
fixture now drives `clouddeletion.Service.RunOnce`, not manual repeated coordinator
calls. Fixture collector/provider/resource evidence is still synthetic.

Financial reconciliation, BFF/UI, migration/restore review and staging
activation/device/certificate/MQTT acceptance remain release gates. This worker
and its tests alone do not satisfy the overall multi-cloud delivery criteria.
