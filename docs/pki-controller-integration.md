# PKI controller integration

Account Manager remains the authority for human identities, local PKI roles,
active Brand Clouds and Products. It exposes `/v1/platform/pki/*` as an
authenticated proxy to the separate PKI controller. This integration is opt-in
and not production-qualified. Startup bootstrap is now sealed after its first successful use.

Apply migrations `074_pki_roles.sql` through `076_last_platform_admin.sql`. Migration 074 It creates `pki_admin`,
`security_custodian`, and `pki_auditor` roles but assigns them to nobody.
Existing Platform Admin permissions do not imply either approval role.

Configure:

- `PKI_CONTROLLER_URL`: HTTPS origin of the controller.
- `PKI_CONTROLLER_CLIENT_CERT`, `PKI_CONTROLLER_CLIENT_KEY`: internal-service
  mTLS identity with CN `account-manager`.
- `PKI_CONTROLLER_CA`: controller server trust bundle.
- `PKI_ENVIRONMENT`: exact registry/Video Cloud environment identifier.
- `PKI_OIDC_MFA_ACR`: the configured IdP assurance class that guarantees MFA.
- The existing Account Manager access-token signer must be RS256; distribute
  only its public key to the controller.

Operators can request fresh authentication through
`GET /v1/auth/oidc/{providerId}/login?pki_step_up=true`. The authorization request
includes `acr_values`, `max_age=0`, and `prompt=login`. The callback uses the
verified ID-token assurance and authentication time. It never interprets an
unverified browser claim or the mere presence of an OIDC session as MFA.

The resulting access token carries the original `auth_time`. PKI requests require
MFA within five minutes. Refresh tokens carry no MFA authority; obtaining an
ordinary refreshed access token requires a new step-up before PKI administration.
Local login remains compatible but does not acquire PKI step-up authority.

Each proxy call rechecks exact active local role assignments. Provisioning and
activation also recheck the current cloud/product relationship. The server signs
a request-bound, one-minute assertion; the browser never receives that assertion
or controller workload credentials. Controller errors are returned without
provider credentials or private CA material.

Root/Brand requests still require a distinct PKI Administrator and Security
Custodian. Cloud/product creation alone never creates a CA. Offline ceremony tooling and the Cloud Admin `/platform/pki` page implement the CSR exchange workflow. Administrative recovery, production custody evidence and recovery qualification remain unfinished.

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
a change. Two-person administrative recovery remains unfinished.

## Console MFA callback

Configure a dedicated OIDC provider whose registered redirect URL is
`https://<console-host>/api/pki/oidc/<provider-id>/callback`. The provider must be
available to Account Manager's generic OIDC login API. Sign in to Cloud Admin,
open `/platform/pki`, and reauthenticate with that provider ID. The console binds
state to the original session and provider, rejects a different returning user,
and checks the new access token against the PKI controller before updating the
server-side session. Tokens are never returned by the console callback.
