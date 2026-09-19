# PKI controller integration

Account Manager remains the authority for human identities, local PKI roles,
active Brand Clouds and Products. It exposes `/v1/platform/pki/*` as an
authenticated proxy to the separate PKI controller. This integration is opt-in
and not production-qualified. Startup bootstrap is now sealed after its first successful use.

Apply migrations `074_pki_roles.sql` through `077_admin_recovery.sql`. Migration 074 creates `pki_admin`,
`security_custodian`, and `pki_auditor` roles but assigns them to nobody.
Existing Platform Admin permissions do not imply either approval role.

The migration runner recognizes the historical Test Lab filename sequences
`068_test_lab_sessions.sql` / `069_test_lab_bindings.sql` /
`070_test_lab_console_identity.sql`, and `070_test_lab_sessions.sql` /
`071_test_lab_bindings.sql` / `072_test_lab_console_identity.sql`, as aliases of
the current 071/072/073 files. It preserves the old markers, adds the canonical
markers with their original application times, and never replays their session
revocations. Only exact recorded filenames qualify; unrelated migrations sharing
the same numbers do not. Adoption checks pinned current SQL digests and fails if
the files changed. Rehearse against a current dev snapshot before live migration;
do not drop existing Test Lab tables or manually fake migration completion.

Configure:

- `PKI_CONTROLLER_URL`: HTTPS origin of the controller.
- `PKI_CONTROLLER_CLIENT_CERT`, `PKI_CONTROLLER_CLIENT_KEY`: internal-service
  mTLS identity with CN `account-manager`.
- `PKI_CONTROLLER_CA`: controller server trust bundle.
- `PKI_ENVIRONMENT`: exact registry/Video Cloud environment identifier.
- `PKI_REQUIRE_USER_MFA`: optional future human-login MFA enforcement; defaults
  to `false`. Configure the same policy on the controller. Ordinary authenticated
  users with current roles may use PKI without MFA when disabled.
- `PKI_OIDC_MFA_ACR`: required only when optional human MFA is enabled; the
  configured IdP assurance class must guarantee MFA. Devices never use MFA.
- The existing Account Manager access-token signer must be RS256; distribute
  only its public key to the controller.

If optional human MFA is enabled, operators can request fresh authentication through
`GET /v1/auth/oidc/{providerId}/login?pki_step_up=true`. The authorization request
includes `acr_values`, `max_age=0`, and `prompt=login`. The callback uses the
verified ID-token assurance and authentication time. It never interprets an
unverified browser claim or the mere presence of an OIDC session as MFA.

The resulting access token carries the original `auth_time`. With optional MFA
enabled, PKI requests require verified MFA within five minutes. Refresh tokens
carry no MFA authority; they require a new step-up only under that enabled policy.
With the default policy, valid ordinary/local user login is sufficient for the
authentication boundary; current roles and distinct approvals still apply.
Ordinary assertions carry `mfa=false` and `auth_time=0`, never fabricated assurance.

Each proxy call rechecks exact active local role assignments. Provisioning and
activation also recheck the current cloud/product relationship. The server signs
a request-bound, one-minute assertion; the browser never receives that assertion
or controller workload credentials. Controller errors are returned without
provider credentials or private CA material.

Normal Cloud/Product creation automatically records a durable PKI job in the same
database transaction. A service-authenticated worker resolves current active
business context and requests the immutable Cloud/Product scope, never human
approval roles. OpenBao retains internal CA keys. Products wait for Cloud CA
readiness; certificate-dependent operations fail closed until PKI is ready.
The API/UI exposes pending, ready, failed or cancelled independently of business
status, with a stable operation ID. Owner transfer does not replace issuer keys.
Manual lifecycle/recovery requests still use the distinct administrative roles.
The offline ceremony page is a historical/special-purpose tool, not signup UX.
See the authoritative `platform_pki.md` section 8 for Root custody and the verified
dev/staging inventory. Production qualification remains unfinished.

## Sealed startup bootstrap

Migration 075 permanently seals existing installations with any Platform Admin
record or assignment, including disabled administrators. On an empty installation,
startup creates the first administrator and the seal atomically. Concurrent starts
cannot create additional administrators. Existing ordinary accounts cannot be
promoted by a bootstrap email. After sealing, startup ignores bootstrap environment
credentials; remove those environment values from the deployment. The legacy
provisioning helper also refuses to run after sealing.

The seal is immutable; account disablement does not reopen it. This is not an
administrative recovery mechanism. Migration 076 protects the last active administrator and the canonical Platform
Admin system role after sealing. It covers user disablement/demotion, pending
verification, assignment removal and system-role disablement/rename. A database
write lock serializes removals, including repeatable-read transactions. API
callers receive a 409 `platform_admin_required` response when the guard rejects
a change. Migration 077 adds independently approved administrator recovery.

## Optional future human-login MFA callback

Configure a dedicated OIDC provider whose registered redirect URL is
`https://<console-host>/api/pki/oidc/<provider-id>/callback`. The provider must be
available to Account Manager's generic OIDC login API. Sign in to Cloud Admin,
open `/platform/pki`, and reauthenticate with that provider ID. The console binds
state to the original session and provider, rejects a different returning user,
and checks the new access token against the PKI controller or Account Manager’s
local recovery authorization before updating the server-side session. Tokens are never returned by the console callback.

## Independently approved administrator recovery

Apply migration `077_admin_recovery.sql`. Recovery is owned by Account Manager
and remains available when the PKI controller is down. It requires sealed
bootstrap, a verified active requester with `platform_admin` or `pki_admin`, and
an existing verified active target account distinct from the requester.

1. In `/platform/pki`, sign in (complete MFA only if enabled) and use **Recover platform
   administration** to submit the target account ID and incident reason.
2. Independently controlled `pki_admin` and `security_custodian` identities review
   the request ID, exact target/reason and request digest. Both approve that digest.
   Neither the requester nor target can approve; one identity cannot fill both roles.
3. A PKI Administrator executes the request within one hour of its creation.
   Account Manager rechecks target status and both approvers' current live roles,
   holds those authorization rows through the transaction, and grants the
   canonical Platform Admin assignment and user flag atomically.

Every operation requires an authenticated human with current roles. Optional MFA,
if enabled, must be authenticated within five minutes; refresh grants no new MFA
assurance. Requests bind their target and reason to a SHA-256 digest;
reusing an idempotency key with a different payload conflicts. Completed execution
replays without granting again. Immutable request/approval/audit records preserve
the incident evidence, and successful grants also appear in the ACL audit log.
Passwords and the sealed bootstrap record are preserved.

The equivalent Account Manager API routes are:

- `GET /v1/platform/admin-recovery`: check current recovery access (no mutation).
- `POST /v1/platform/admin-recovery`: `Idempotency-Key` plus
  `{"target_user_id":"<UUID>","reason":"<incident reason>"}`.
- `GET /v1/platform/admin-recovery/{requestId}`: review the complete request and approvals.
- `POST /v1/platform/admin-recovery/{requestId}/approve`:
  `{"role":"pki_admin","request_sha256":"<reviewed digest>"}`; the independent
  custodian uses `security_custodian` in a separate authenticated session.
- `POST /v1/platform/admin-recovery/{requestId}/execute`: `{}`.
- `POST /v1/platform/admin-recovery/{requestId}/cancel`: `{}`, requester only.

Cloud Admin proxies these through `/api/platform/admin-recovery` using its
server-side session. Browser writes require same-origin JSON and an idempotency
key. A controller outage does not bypass role checks or an enabled human MFA policy.

Pre-provision independently controlled recovery approvers before an incident.
This workflow requires those identities and a working Account Manager database
and IdP. Database/IdP loss requires the separate backup and disaster-recovery
procedure, which is still unfinished. Disabling compromised prior accounts and
revoking their existing sessions remain separate incident-response steps. Real
custodian accounts and a live recovery drill remain production acceptance gates.

## Managed controller connection

`PKI_CONTROLLER_SOCKET` opts into the host-local `pkimanagement` owner from Video
Cloud. Set the same `PKI_CONTROLLER_URL` HTTPS origin and `PKI_ENVIRONMENT` on
both processes. Leave `PKI_CONTROLLER_CLIENT_CERT`, `PKI_CONTROLLER_CLIENT_KEY`
and `PKI_CONTROLLER_CA` unset in this mode. The owner holds the registered
`service:account-manager` key, renews through the certificate issuer, verifies
both remote Service servers and closes connections when identity/trust is denied.
Account Manager continues signing exactly the same human authorization assertions;
MFA remains disabled by default. The socket mode never falls back to static TLS.

Run both processes as the same UID, sharing a dedicated 0700 socket directory.
Only the owner mounts the private credential-state directory and provisioner
material. The socket is 0600; no TCP listener is exposed. Configure the two remote
server pins/CA files, registry verifier grants and provisioner on the owner using
Video Cloud's `deploy/pki/account-manager-managed.env.example`. Service authorities,
server receipts and signed CRLs must already exist. The owner serves only after
initial managed issuance/validation succeeds. Account Manager may start earlier;
PKI calls fail unavailable until the socket owner is ready. Keep the owner state
across restarts. After a crash, an existing stale socket is rejected rather than
silently unlinked; remove it only after verifying the old owner has stopped.

This is local implementation support; live dev rollout and durable consumer CRL
receipt qualification are separate acceptance items. Public Account Manager
HTTPS remains on its existing boundary; this only changes controller egress.

`APP_CERT_ISSUER_SOCKET` may select the same private socket for App certificate
issuance. In that mode, leave `APP_CERT_ISSUER_CLIENT_CERT`,
`APP_CERT_ISSUER_CLIENT_KEY` and `APP_CERT_ISSUER_CA_FILE` unset. The owner accepts
only `POST /v1/certificates/app/issue` for the configured certissuer origin and
presents the same registered `service:account-manager` identity. Login and human
authorization behavior do not change.
# Non-production maintenance command

After a reviewed Device Root rebuild, `rtk-account-manager-device-pki-admin`
inventories one Cloud's ready/failed automatic jobs. It requires `--cloud-id`,
`--replacement-root-id`, explicit `PKI_ENVIRONMENT=dev|staging` and the selected
`DATABASE_URL`. Default is dry-run; `--apply --confirm-environment <environment>`
atomically queues new operation IDs and audits their predecessors. Confirm the
replacement Root pin and fence writers before apply. Business IDs are unchanged;
pending, cancelled, disabled and deleting scopes are not reactivated. This is not
a startup migration or a production/dual-chain migration tool.
