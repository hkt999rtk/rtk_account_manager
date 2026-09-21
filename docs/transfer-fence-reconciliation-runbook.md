# Account-only transfer-fence reconciliation

This procedure is for a platform administrator investigating an uncertain
Account Manager claim transfer/reclaim **before Video Cloud activation**. It is
not a way to move an active device, session, clip, telemetry history, or media
between organizations. Keep the support ticket, source/target organization,
Account Device ID, Video Cloud device ID, and the attempted reservation ID
together. Do not put claim-token values, certificates, private keys, or other
secrets in `evidence`.

## Inspect first

Use a platform-admin access token to call
`GET /v1/admin/device-claims/{claimId}/transfer-fence`. Account Manager locks
the account device, claim, and token, then reads the authenticated Video Cloud
reservation. Interpret the returned `status` as follows:

| Status | Meaning | Operator action |
| --- | --- | --- |
| `absent` | No remote fence and no account-side transfer generation. | Do not cancel. Investigate the original request result before starting a new transfer. |
| `committed` | Account owner/token/device and stored generation match the remote target. | Do not cancel. Continue or replay the normal provision operation with that generation. |
| `cancelable` | The original source claim/version and Video Cloud binding match exactly; no account provision operation has been recorded. | Confirm the ticket, target organization, device IDs, and `reservation_id`, then use the guarded cancellation endpoint if abandoning this attempt. |
| `manual_review` | Binding, claim version, lifecycle evidence, or metadata is inconsistent. | Stop. Do not call Video Cloud's internal DELETE directly. Escalate for coordinated state investigation. |

A `503` or timeout is **not** evidence that no reservation exists. Restore the
authenticated Video Cloud connection and inspect again. A `409` while reading
account rows means state changed concurrently; inspect again rather than using
an earlier result.

## Cancel only a proved stale generation

For a `cancelable` result, submit
`POST /v1/admin/device-claims/{claimId}/transfer-fence/cancel` with JSON:

```json
{
  "reservation_id": "<exact ID returned by inspection>",
  "reason": "Account transfer did not commit; abandon this attempt",
  "evidence": {"ticket": "<support ticket>"}
}
```

The server repeats every check under account row locks and calls Video Cloud's
authenticated exact-ID cancellation while those locks remain held. Video Cloud
holds its device lifecycle lock and refuses cancellation if any known device,
session, factory, clip, telemetry, outbox, or rollout state exists. A successful
response is `200` with `status=cancelled`; Account Manager writes a
`device_claim_transfer_fence_cancelled` audit event. Inspect again and expect
`absent` before a new attempt.

For `409`, re-inspect; do not change the requested generation to force a match.
For a timeout or `503`, the remote DELETE or audit commit may have completed.
Re-inspect rather than assuming either outcome. If the state is `committed` or
`manual_review`, leave the fence in place and escalate. There is no TTL or
automatic cleanup, because expiry could reopen the old-owner activation race.

## Acceptance evidence still required

Staging must exercise lost reserve responses, ambiguous account commits,
wrong-generation cancellation, a concurrent activation, lost DELETE responses,
repeated inspection, and audit visibility across both services. This runbook
does not replace that acceptance or the separate active-device migration design.
