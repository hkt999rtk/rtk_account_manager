# Developer Console test leases

Migration 068 adds `test_lab_sessions`; it does not change device identifiers.
Authenticated managed-cloud sessions can create a five-minute lease with
`POST /v1/developer/brand-clouds/{cloud}/test-lab/sessions`, supplying
`product_id`, `device_id` and `account_id`. Creation checks cloud/device management permission,
the existing runtime device binding and a per-user maximum of three active leases.

`POST .../sessions/{session}/credentials` rechecks the actor, cloud, live device
authorization and Product services, then uses the existing trusted Video Cloud
lifecycle connection to obtain a 30-second nonrefreshable runtime token.
`DELETE .../sessions/{session}` revokes only the caller's lease in the same cloud.

The internal app-token authorizer recognizes `subject_type=test_lab` and uses
`user_id` as the test lease identifier for this subject type. Ordinary user and
app authorization contracts are unchanged. Expired/revoked leases fail closed.

Requires `TEST_LAB_ENABLED=true`, `ACCOUNT_MANAGER_ENV` set to dev/development/
local/staging, and the existing Video Cloud lifecycle URL/token binding. Runtime
credentials are not stored in the lease table. MQTT revocation is bounded by the
short token lifetime, not an instantaneous disconnect of established connections.

## Test-only account and binding API

Migration 070 replaces separate App password authorization. POST `/accounts` now
accepts only `{}` and uses the authenticated Console developer ID. A unique stable
`test_lab_console_users` mapping points to an internal end-user with no usable
password or external login identity. Email matching/adoption is never performed.
The response shows the Console email as a label. Cloud-scoped access is reused and
renewed on page load/every minute, with current permission checks. Old credential
inputs are rejected; prior password-based delegations are revoked on upgrade.

Migration 069 adds delegated account access, rate-limited login attempts and hashed
one-use bind grants. Existing leases are revoked on upgrade. No passwords,
private keys, certificates or bearer credentials are stored in these tables.
All paths below are under `/v1/developer/brand-clouds/{brandCloudId}/test-lab` and
require the existing managed human developer session plus the feature gate.

| Method / path | Input / result |
| --- | --- |
| POST `/accounts` | `{}` uses the Console login. Returns `{id,end_user_id,email,expires_at}` without a second login. |
| DELETE `/accounts/{accountId}` | Revoke this developer's delegation; 204, idempotent. |
| GET `/devices` | Query `account_id,product_id,limit,offset`; returns `devices,next_offset,has_more`. Rows include `id,name,bound,bindable,provision_status,connection_status`. |
| GET `/devices/{deviceId}` | Same account/Product query; checks binding and returns `runtime_ready`. |
| POST `/devices/{deviceId}/grant` | `{account_id,product_id}`; returns 120-second `claim_token`. |
| POST `/devices/{deviceId}/bind` | Same scope plus `claim_token`; repeat while bound is idempotent. |
| POST `/devices/{deviceId}/unbind` | Same scope; soft-disables only this end-user binding, revokes its grants/leases; preserves device/certs/other users. |
| POST `/devices/{deviceId}/provision` | Same scope plus `operation_id,activity_id,clip_public_key` (SPKI RSA >=2048 bits); queues the ordinary lifecycle outbox, returns 202. |

Access lasts 30 minutes and is automatically renewed from the Console login.
Other-user binding/pending unbind/conflicting provisioning inputs return 409,
inaccessible scope or expired proof 404, malformed input 400. There is no password
login/signup API in Test Lab. Runtime rechecks account, developer permissions, binding,
Product services and activated state, including after an Unbind in another tab.

Eligibility requires both purpose=test and a completed issued factory enrollment
from developer-console/pki-test in the exact cloud/Product. A metadata flag is not
proof. Grants are developer-approved dev testing authority, not production claim
overrides. Unbind invalidates all old grants for this end-user/device; rebind must
obtain a fresh grant. No UUID/devid conversion or destructive Unprovision is used.
Provisioning retains the first activity ID and public key; retries must reuse them.
Unbind is blocked while provisioning is pending. Existing workers consume the
normal outbox; binding alone neither provisions nor proves connectivity.
