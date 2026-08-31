# Keycloak / OIDC SSO Specification

## Source Of Truth

This document is the detailed specification for Keycloak/OIDC SSO in account
manager. `spec.md` remains the top-level product and API specification and
links here for the OIDC details.

The local development flow for validating this integration against Keycloak is
documented in [keycloak_local_runbook.md](keycloak_local_runbook.md).

## Goal

Add Keycloak/OIDC as an authentication option while keeping account manager as
the owner of local users, organization membership, roles, device authorization,
refresh tokens, and API JWT issuance.

Keycloak is an external identity provider. It is not embedded in account
manager and does not own account-manager authorization policy.

## First Implementation Decisions

- OIDC uses an account-manager backend callback flow. The backend redirects the
  browser to Keycloak, receives the authorization-code callback, validates the
  OIDC response, and returns the existing account-manager token response shape.
- Local email/password login remains supported.
- User provisioning is pre-provision only. OIDC login never creates a local
  user, organization, or membership.
- Organization roles are resolved only from account-manager local memberships.
  Keycloak groups, realm roles, and client roles are not authorization inputs in
  the first implementation.
- Successful SSO login issues account-manager access and refresh JWTs. Clients
  keep using `Authorization: Bearer <account-manager-access-token>` for API
  calls.
- The first implementation supports one active Keycloak/OIDC provider, resolved
  from an enabled database provider first and environment configuration as a
  fallback.
- Platform-admin provider CRUD stores only secret references such as
  `env:OIDC_CLIENT_SECRET`; raw client secrets are not stored or returned.

## Backend Callback Flow

1. `GET /v1/auth/oidc/providers` returns enabled public provider metadata when
   `OIDC_ENABLED=true`; it returns an empty list or disabled response when OIDC
   is not enabled.
2. `GET /v1/auth/oidc/:providerId/login` creates a one-time OIDC state and
   nonce, stores only their hashes, and redirects the browser to the provider
   authorization endpoint using the authorization-code flow.
3. `GET /v1/auth/oidc/:providerId/callback` validates state, exchanges the code,
   validates the ID token, resolves the local account-manager user, links the
   identity only under the configured policy, and returns the existing login
   token response shape.

## User Provisioning And Linking

- Unknown SSO users return `403 user_not_provisioned`.
- OIDC login never creates users, organizations, or memberships.
- Disabled local users cannot authenticate through SSO.
- With the default `OIDC_AUTO_LINK_EMAIL=false`, callback accepts only an
  existing `user_identities` link for the validated provider subject.
- If `OIDC_AUTO_LINK_EMAIL=true` is explicitly enabled, callback may link a
  validated provider subject to an existing pre-provisioned local user whose
  normalized email exactly matches the verified Keycloak email.
- The Keycloak email claim must be present and `email_verified=true`.

## Authorization Model

- Account-manager persisted ACL facts remain the authorization source.
- Keycloak groups, realm roles, and client roles grant no permissions directly.
  They can affect authorization only through account-manager-managed
  `external_group_mappings`, which create scoped product role assignments.
- A successful SSO login proves identity. Unmapped external groups grant
  nothing.

## Token Model

- Account manager issues its own access and refresh JWTs after successful SSO.
- Account-manager refresh-token rotation, logout, revocation, and disabled-user
  checks remain unchanged.
- Keycloak access tokens and refresh tokens are not persisted in the first
  implementation.
- Keycloak token values must not be logged, stored in reports, or returned by
  account-manager APIs.

## Security Policy

- Callback validation must require state and nonce validation.
- ID token validation must require issuer, audience, signature, and expiry
  checks.
- The provider issuer must match the configured `OIDC_ISSUER_URL`.
- The token audience must include the configured `OIDC_CLIENT_ID`.
- Expired, replayed, or consumed login state must be rejected.
- Raw `state` and `nonce` values must not be stored.
- Provider discovery and admin list/show responses must never expose client
  secrets.

## Data Model

### `identity_providers`

Stores configured external OIDC provider metadata. The implementation resolves
an enabled database provider first and falls back to the environment-configured
provider when no enabled database provider exists.

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `id` | UUID | Yes | Primary key. |
| `provider_id` | Text | Yes | Stable URL-safe provider identifier, unique. |
| `name` | Text | Yes | Human-readable provider name, for example `Keycloak`. |
| `type` | Text | Yes | `oidc` for the Keycloak integration. |
| `issuer_url` | Text | Yes | Expected OIDC issuer URL. |
| `client_id` | Text | Yes | OIDC client id registered in Keycloak. |
| `client_secret_ref` | Text | No | Optional secret reference for admin-managed providers; raw secrets must not be returned by APIs. |
| `scopes` | Text array | Yes | Requested scopes, default `openid,email,profile`. |
| `enabled` | Boolean | Yes | Disabled providers are hidden from public discovery and cannot start login. |
| `metadata` | JSONB | Yes | Non-secret provider metadata, default `{}`. |
| `created_at` | Timestamp | Yes | Creation timestamp. |
| `updated_at` | Timestamp | Yes | Last update timestamp. |

Constraints:

- `provider_id` is unique and must not be blank.
- `issuer_url` must match the issuer in validated ID tokens.
- Raw client secrets must not be exposed in list/show API responses or reports.

### `user_identities`

Links local account-manager users to external OIDC identities.

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `id` | UUID | Yes | Primary key. |
| `user_id` | UUID | Yes | References `users.id`. |
| `provider_id` | UUID | Yes | References `identity_providers.id`. |
| `issuer_url` | Text | Yes | Issuer observed and validated at link time. |
| `subject` | Text | Yes | OIDC `sub` claim. |
| `email` | Text | Yes | Verified email claim used for pre-provision matching. |
| `email_verified` | Boolean | Yes | Must be true for a successful SSO link/login. |
| `claims` | JSONB | Yes | Non-secret normalized identity claims, default `{}`. |
| `linked_at` | Timestamp | Yes | Time the identity was linked. |
| `last_login_at` | Timestamp | No | Last successful SSO login time. |
| `created_at` | Timestamp | Yes | Creation timestamp. |
| `updated_at` | Timestamp | Yes | Last update timestamp. |

Constraints:

- `(provider_id, subject)` is unique.
- `(user_id, provider_id)` should be unique for the first implementation unless
  a later policy allows multiple subjects from the same provider.
- `email` must be normalized lowercase and must match a verified Keycloak email
  claim at link time.
- Disabled local users cannot authenticate through linked identities.

### `oidc_login_states`

Stores short-lived OIDC login state for backend callback validation.

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `id` | UUID | Yes | Primary key. |
| `provider_id` | UUID | Yes | References `identity_providers.id`. |
| `state_hash` | Text | Yes | Hash of the generated OIDC state value. |
| `nonce_hash` | Text | Yes | Hash of the generated OIDC nonce value. |
| `redirect_url` | Text | Yes | Callback URL used for this login attempt. |
| `post_login_redirect_url` | Text | No | Optional allowed application redirect target after token issuance. |
| `expires_at` | Timestamp | Yes | Short expiry, recommended maximum 10 minutes. |
| `consumed_at` | Timestamp | No | Set after successful or terminal callback processing. |
| `created_at` | Timestamp | Yes | Creation timestamp. |

Constraints:

- Raw `state` and `nonce` values must not be stored.
- `state_hash` is unique while active.
- Expired or consumed state records must not be accepted.
- Callback handling must consume state atomically so replayed callbacks fail.

## API Contract

### Public Auth

| Method | Path | Auth | Description |
| --- | --- | --- | --- |
| `GET` | `/v1/auth/oidc/providers` | No | List enabled public OIDC provider metadata without secrets. |
| `GET` | `/v1/auth/oidc/:providerId/login` | No | Start Keycloak/OIDC login and redirect to the provider authorization endpoint. |
| `GET` | `/v1/auth/oidc/:providerId/callback` | No | Handle backend OIDC callback and return the existing token response shape. |

### Current User

| Method | Path | Auth | Description |
| --- | --- | --- | --- |
| `GET` | `/v1/me/identities` | Yes | List current user's linked external identities. |
| `DELETE` | `/v1/me/identities/:identityId` | Yes | Unlink one of the current user's external identities when policy allows. |

### Platform Admin Provider Management

These endpoints are admin-only. They manage provider metadata and secret
references, not raw client secrets.

| Method | Path | Auth | Role | Description |
| --- | --- | --- | --- | --- |
| `POST` | `/v1/admin/identity-providers` | Yes | Platform admin | Create an OIDC identity provider configuration without exposing raw secrets. |
| `GET` | `/v1/admin/identity-providers` | Yes | Platform admin | List OIDC identity provider configurations without raw secrets. |
| `GET` | `/v1/admin/identity-providers/:providerId` | Yes | Platform admin | Show one OIDC identity provider configuration without raw secrets. |
| `PATCH` | `/v1/admin/identity-providers/:providerId` | Yes | Platform admin | Update OIDC identity provider metadata, status, or secret reference. |
| `DELETE` | `/v1/admin/identity-providers/:providerId` | Yes | Platform admin | Disable or remove an OIDC identity provider when no active policy blocks it. |

## Configuration

| Variable | Description |
| --- | --- |
| `OIDC_ENABLED` | Enables OIDC login routes when `true`. |
| `OIDC_PROVIDER_ID` | Stable provider id used in `/v1/auth/oidc/:providerId/...`, for example `keycloak`. |
| `OIDC_PROVIDER_NAME` | Display name returned by provider discovery, for example `Keycloak`. |
| `OIDC_ISSUER_URL` | Expected Keycloak/OIDC issuer URL. |
| `OIDC_CLIENT_ID` | OIDC client id registered for account manager. |
| `OIDC_CLIENT_SECRET` | OIDC client secret. Must be provided through runtime secret management and never logged. |
| `OIDC_REDIRECT_URL` | Exact backend callback URL registered with Keycloak. |
| `OIDC_SCOPES` | Space-separated scopes, default `openid email profile`. |
| `OIDC_AUTO_LINK_EMAIL` | Whether callback may link a validated provider subject to an existing pre-provisioned local user by verified email. Default `false`. |

## Implementation Tests

Tests must cover:

- Provider discovery when OIDC is disabled and enabled.
- Login redirect creates hashed state and nonce records.
- Callback rejects invalid state, replayed state, invalid nonce, invalid issuer,
  invalid audience, expired token, and unverified email.
- Callback rejects unknown users with `403 user_not_provisioned`.
- Callback rejects disabled local users.
- Successful callback links an identity under the configured policy and returns
  the existing account-manager token response shape.
- Local email/password login continues to work after OIDC is enabled.
- Keycloak groups, realm roles, and client roles do not directly grant
  account-manager organization access; mapped groups grant only the scoped role
  assignments configured in Account Manager.
- Keycloak access tokens and refresh tokens are not persisted.

## Acceptance Criteria

The Keycloak/OIDC authentication implementation is acceptable when:

- Existing email/password login, refresh-token rotation, and account-manager JWT
  behavior remain supported and documented.
- Keycloak is treated as an external IdP, not as an embedded account-manager SSO
  server.
- Account-manager local users, organization memberships, roles, and device
  authorization remain the source of truth for API access.
- The backend callback flow validates state, nonce, issuer, audience, signature,
  expiry, and verified email before issuing account-manager JWTs.
- Unknown SSO users return `403 user_not_provisioned`.
- Disabled local users cannot login through SSO.
- Successful SSO returns the existing token response shape and does not persist
  Keycloak access or refresh tokens.
- Automated tests cover the OIDC behavior matrix and prove local login still
  works.
