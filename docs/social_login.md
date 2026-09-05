# Google and GitHub Login

Account Manager supports optional Google OIDC and GitHub OAuth login for the
Cloud Admin sign-in pages. Password login and account signup continue to work.
Provider buttons are discovered at runtime and remain hidden when the provider
is disabled or incompletely configured.

## Account behavior

- An existing provider identity signs in its linked local user.
- A first-time identity is linked to an existing local user with the same
  verified email address.
- If no local user has that email address, Account Manager creates an active
  evaluation Connect+ account and its default Brand Cloud.
- Socially created accounts are marked email-verified immediately. No email
  verification message or verification step is created.
- A disabled, pending, or unverified existing local account cannot use social
  login until its local status is resolved.
- Provider access and refresh tokens are never persisted. Account Manager
  issues its normal access and refresh tokens after successful login.

## Shared callback

Register this callback URL with both providers:

```text
https://<cloud-admin-host>/api/auth/social/callback
```

For local development, use the actual Cloud Admin origin, for example:

```text
http://localhost:8080/api/auth/social/callback
```

The configured value must match the registered callback exactly. Production
configuration requires HTTPS.

## Environment

```dotenv
SOCIAL_LOGIN_CALLBACK_URL=https://<cloud-admin-host>/api/auth/social/callback
SOCIAL_OAUTH_STATE_SECRET=<random-secret-at-least-32-characters>

GOOGLE_LOGIN_ENABLED=false
GOOGLE_OAUTH_CLIENT_ID=
GOOGLE_OAUTH_CLIENT_SECRET=

GITHUB_LOGIN_ENABLED=false
GITHUB_OAUTH_CLIENT_ID=
GITHUB_OAUTH_CLIENT_SECRET=
```

Enable providers independently after their credentials are available. Startup
rejects an enabled provider with missing credentials, an invalid callback URL,
or a state secret shorter than 32 characters.

For Google, create a Web application OAuth client and add the shared callback
as an authorized redirect URI. For GitHub, create an OAuth App and set its
authorization callback URL to the same value. Keep all client secrets in the
runtime secret manager; do not commit them to an environment file.

## HTTP flow

Cloud Admin uses these Account Manager endpoints:

- `GET /v1/auth/social/providers` discovers enabled providers.
- `POST /v1/auth/social/start` creates single-use state, nonce, and PKCE values
  and returns the provider authorization URL.
- `POST /v1/auth/social/callback` exchanges the authorization code, validates
  the provider identity and verified email, resolves or creates the local
  account, and returns the standard Account Manager session material.

The OAuth state is stored only as a hash, expires after ten minutes, and can be
consumed once. The post-login destination accepts only local Customer or
Platform routes.
