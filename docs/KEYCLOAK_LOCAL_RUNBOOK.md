# Local Keycloak OIDC Runbook

This runbook describes a local developer setup for validating Account Manager
OIDC login against Keycloak. It is for local integration only.

Account Manager does not embed Keycloak and does not use Keycloak groups or
roles for authorization. Keycloak authenticates the user; Account Manager still
owns users, organization memberships, roles, devices, refresh tokens, and API
JWT issuance.

## Runtime Modes

| Mode | Purpose | Dependency |
| --- | --- | --- |
| Local Keycloak | Manual local OIDC login validation. | Docker Compose `oidc` profile. |
| CI fake OIDC | Automated OIDC tests. | In-process Go `httptest` OIDC/JWKS server. |
| Production | External company Keycloak or OIDC provider. | Runtime `OIDC_*` env vars and secret management. |

CI must not depend on the Docker Compose Keycloak service. The automated tests
use fake OIDC servers so CI remains deterministic and does not require a live
Keycloak container.

## Start Local Services

Copy the local environment file if needed:

```sh
cp .env.example .env
```

Start Postgres and Keycloak:

```sh
docker compose --profile oidc up -d postgres keycloak
```

Keycloak is available at `http://localhost:8081`. The local admin credentials
from `docker-compose.yml` are:

```text
username: admin
password: admin
```

The normal Postgres-only development flow is unchanged:

```sh
make db-up
```

That command starts only Postgres because the Keycloak service is behind the
optional `oidc` Compose profile.

## Create A Local Realm

Open the Keycloak admin console:

```text
http://localhost:8081/admin
```

Create a realm:

| Field | Value |
| --- | --- |
| Realm name | `rtk-local` |

The local issuer URL becomes:

```text
http://localhost:8081/realms/rtk-local
```

## Create The OIDC Client

Inside the `rtk-local` realm, create a client:

| Field | Value |
| --- | --- |
| Client type | `OpenID Connect` |
| Client ID | `rtk-account-manager` |
| Client authentication | `On` |
| Authorization | `Off` |
| Standard flow | `On` |
| Direct access grants | `Off` |
| Root URL | `http://localhost:8080` |
| Valid redirect URIs | `http://localhost:8080/v1/auth/oidc/keycloak/callback` |
| Web origins | `http://localhost:8080` |

After saving, open the client's credentials tab and copy the client secret.
Store it only in local runtime configuration, not in source control.

## Create A Test User

Create a Keycloak user whose email matches an existing Account Manager local
user:

| Field | Example |
| --- | --- |
| Username | `owner@example.com` |
| Email | `owner@example.com` |
| Email verified | `On` |
| Enabled | `On` |

Set a temporary or permanent password for that user in Keycloak.

Account Manager is pre-provision only. The same email must already exist as an
enabled local Account Manager user. You can create one through the normal local
registration flow:

```sh
curl -sS -X POST http://localhost:8080/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{
    "email": "owner@example.com",
    "password": "password123",
    "display_name": "Owner",
    "organization_name": "Owner Org"
  }'
```

## Configure Account Manager

Set these values in `.env`:

```sh
OIDC_ENABLED=true
OIDC_PROVIDER_ID=keycloak
OIDC_PROVIDER_NAME=Keycloak
OIDC_ISSUER_URL=http://localhost:8081/realms/rtk-local
OIDC_CLIENT_ID=rtk-account-manager
OIDC_CLIENT_SECRET='<client-secret-from-keycloak>'
OIDC_REDIRECT_URL=http://localhost:8080/v1/auth/oidc/keycloak/callback
OIDC_SCOPES='openid email profile'
OIDC_AUTO_LINK_EMAIL=true
```

`OIDC_AUTO_LINK_EMAIL=true` is convenient for local manual validation because it
links a verified Keycloak subject to an existing enabled local user with the
same normalized email. Leave it `false` when validating a pre-created
`user_identities` link policy.

Run migrations and start the API:

```sh
make migrate
make run
```

## Validate Provider Discovery

Check that Account Manager exposes the enabled public provider metadata:

```sh
curl -sS http://localhost:8080/v1/auth/oidc/providers | jq
```

The response must not include any client secret or Keycloak token.

## Run A Manual OIDC Login

Open the Account Manager login endpoint in a browser:

```text
http://localhost:8080/v1/auth/oidc/keycloak/login
```

Expected flow:

1. Account Manager creates hashed state and nonce records.
2. The browser is redirected to Keycloak.
3. The user signs in with the Keycloak test user.
4. Keycloak redirects back to Account Manager.
5. Account Manager validates state, nonce, issuer, audience, signature, expiry,
   and verified email.
6. Account Manager returns the normal access-token and refresh-token response
   shape.

Use the returned Account Manager access token for API calls:

```sh
curl -sS http://localhost:8080/v1/me \
  -H "Authorization: Bearer <account-manager-access-token>" | jq
```

## Validate Identity Linking

After a successful login, list the current user's linked external identities:

```sh
curl -sS http://localhost:8080/v1/me/identities \
  -H "Authorization: Bearer <account-manager-access-token>" | jq
```

Unlinking an identity does not delete the local user and does not change local
password login:

```sh
curl -sS -X DELETE http://localhost:8080/v1/me/identities/<identity-id> \
  -H "Authorization: Bearer <account-manager-access-token>"
```

With `OIDC_AUTO_LINK_EMAIL=false`, the same Keycloak subject cannot log in
again after unlinking unless an identity link is recreated.

## Admin-Managed Provider Option

The env-configured provider is enough for the first local flow. Platform admins
can also create a provider through the API when testing the admin-managed
configuration path:

```sh
curl -sS -X POST http://localhost:8080/v1/admin/identity-providers \
  -H "Authorization: Bearer <platform-admin-access-token>" \
  -H 'Content-Type: application/json' \
  -d '{
    "provider_id": "keycloak",
    "name": "Keycloak",
    "issuer_url": "http://localhost:8081/realms/rtk-local",
    "client_id": "rtk-account-manager",
    "client_secret_ref": "env:OIDC_CLIENT_SECRET",
    "scopes": ["openid", "email", "profile"],
    "enabled": true
  }'
```

The API accepts only `env:VAR_NAME` secret references. It does not accept or
return raw client secrets.

## Common Failures

| Symptom | Likely Cause | Fix |
| --- | --- | --- |
| `oidc_disabled` | `OIDC_ENABLED` is not `true` or no provider is enabled. | Update `.env` or enable the provider. |
| `invalid_oidc_state` | Callback was replayed, expired, or state was lost. | Start a fresh login. |
| `invalid_oidc_token` | Issuer, audience, signature, expiry, or nonce validation failed. | Check issuer URL, client ID, redirect URL, and system time. |
| `unverified_oidc_email` | Keycloak email claim is missing or not verified. | Mark the Keycloak user's email as verified. |
| `user_not_provisioned` | No enabled local Account Manager user matches the identity policy. | Pre-create the local user or identity link. |

## Stop Local Services

Stop the optional Keycloak profile and Postgres:

```sh
docker compose --profile oidc down
```

To delete local Keycloak and Postgres data:

```sh
docker compose --profile oidc down -v
```
