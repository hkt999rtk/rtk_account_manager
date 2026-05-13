# Account Manager Backend Specification

## 1. Product Goal

Build a backend account and device manager similar in spirit to Amazon IoT Device Manager. The system manages user accounts, organizations, and device registries for devices such as IP cameras, MQTT devices, and generic connected devices.

The v1 service is a REST API backend only. It stores account and device state in Postgres and provides authentication, organization membership, role-based authorization, and registry-only device management.

This service is not a Web UI or dashboard. Enterprise dashboard UX, BFF routes,
console-local sessions, preferences, and non-authoritative display caches belong
to `rtk_cloud_admin`. This service remains the authoritative backend control
plane for identity, tenant context, authorization, entitlement, device registry,
and provisioning intent.

Provisioning and account/video event-channel integration are the v2 surface implemented by this repository. [PROVISIONING_AND_EVENT_CHANNEL_PLAN.md](PROVISIONING_AND_EVENT_CHANNEL_PLAN.md) tracks rollout history and verification status, and this spec must stay aligned with the shared contracts in `contracts/PROVISION.md` and `contracts/CROSS_SERVICE_CHANNEL.md`.

## 2. V1 Scope

### Included

- Go backend server using Gin.
- Postgres persistence.
- SQL migrations for schema management.
- Email/password authentication.
- Authenticated current-user password change.
- Authenticated current-user account disable/delete.
- JWT-based API authentication.
- Refresh-token support for longer sessions.
- Organization-based account model.
- Multiple users per organization.
- Role-based access control with `owner`, `admin`, and `member`.
- Owner-managed organization profile updates.
- Organization-owned devices.
- Organization-scoped device groups and device tags for fleet selection.
- Device categories:
  - `ip_camera`
  - `mqtt_device`
  - `generic`
- Device CRUD operations.
- Device status management.
- OpenAPI YAML as the API contract source of truth.
- Docker Compose based local Postgres environment.

### Out of Scope

- Frontend UI.
- MQTT broker integration.
- Camera streaming.
- Telemetry ingestion.
- Device command dispatch.
- Device self-registration or provisioning flows in v1.
- Device certificate management.
- Third-party/social login.
- Executable batch operations, OTA campaign execution, and firmware rollout policy.
- Custom RBAC permissions.
- Multi-region deployment concerns.

## 2.1 V2 Scope: Provisioning And Cross-Service Channel

The shared contracts in `contracts/` define the product-level integration boundary between account manager, Realtek video server, and an independent cross-service channel runtime.

V2 adds:

- Account-side provisioning operation APIs for organization-owned registry devices.
- Explicit account-manager to Realtek video server identity mapping, especially `video_cloud_devid`.
- Cross-service command publication to `account.video.commands`.
- Cross-service event consumption from `video.account.events`.
- Idempotent operation tracking with `operation_id`.
- Outbox/inbox persistence for at-least-once delivery.
- Retry and dead-letter state for failed cross-service messages.
- Account-side projection of provisioning, deactivation, online-state, and selected metadata events.
- Metadata merge support so cross-service projections do not overwrite unrelated device metadata.
- A broker adapter boundary so Azure Event Hubs or an equivalent broker can be used behind the same contract.

V2 must not:

- Merge the cross-service channel runtime into the account-manager API process.
- Treat Realtek video server activation as equivalent to account-manager `online` status.
- Use Realtek video server `POST /setup_eventhub` as the account/video cross-service channel.
- Assume account-manager device UUID and Realtek video server `devid` are the same unless deliberately configured by integration.

V2 logical streams:

| Stream | Direction | Purpose |
| --- | --- | --- |
| `account.video.commands` | Account manager to video-side integration worker | Device lifecycle commands that request Realtek video server side effects. |
| `video.account.events` | Video-side integration worker to account manager | Device lifecycle results and state projections consumed by account manager. |

V2 message types:

- `DeviceProvisionRequested`
- `DeviceProvisionSucceeded`
- `DeviceProvisionFailed`
- `DeviceDeactivateRequested`
- `DeviceDeactivateSucceeded`
- `DeviceDeactivateFailed`
- `DeviceOnlineChanged`
- `DeviceMetadataChanged`

V2 operation and message statuses:

- `pending`
- `published`
- `succeeded`
- `failed`
- `retrying`
- `dead_lettered`

Account-manager-owned video metadata keys:

- `video_cloud_devid`
- `video_cloud_activation_status`
- `video_cloud_activity_id`
- `video_cloud_activated_at`
- `video_cloud_deactivated_at`
- `video_cloud_last_error`

## 3. Core Concepts

### Organization

An organization represents an account boundary. Devices belong to organizations, and users gain access to devices through organization membership.

### User

A user is a human account authenticated by email and password. A user may belong to one or more organizations.

### Organization Member

An organization member links a user to an organization and assigns a role.

### Device

A device is a registry entry owned by an organization. The server assigns each device a UUID. External identity fields such as serial number and MAC address are stored as optional metadata and must not replace the server UUID as the primary identifier.

### Device Group

A device group is an organization-scoped registry target set. Groups contain existing account-manager device UUIDs and do not execute device commands by themselves.

### Device Tag

A device tag is an organization-scoped label attached to an existing device. Tags are selection metadata only; they do not replace device metadata JSON or trigger product-side operations.

## 4. Data Model

### `organizations`

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `id` | UUID | Yes | Primary key, generated by server/database. |
| `name` | Text | Yes | Organization display name. |
| `tier` | Text | Yes | One of `evaluation`, `commercial`; defaults to `commercial`. |
| `evaluation_device_quota` | Integer | Yes | Evaluation-tier active-device quota, constrained to 1-200 and defaulting to 5. |
| `created_at` | Timestamp | Yes | Creation timestamp. |
| `updated_at` | Timestamp | Yes | Last update timestamp. |

Constraints:

- `name` must not be blank after trimming whitespace.

### `users`

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `id` | UUID | Yes | Primary key, generated by server/database. |
| `email` | Text | Yes | Unique, normalized email address. |
| `password_hash` | Text | Yes | Hashed password; raw passwords are never stored. |
| `display_name` | Text | No | User display name. |
| `email_verified` | Boolean | Yes | Set after email verification succeeds. |
| `email_verified_at` | Timestamp | No | Time email verification completed. |
| `signup_pending_verification` | Boolean | Yes | Public signup accounts cannot log in until this is cleared. |
| `platform_admin` | Boolean | Yes | Allows platform-admin quota decisions and metrics access. |
| `created_at` | Timestamp | Yes | Creation timestamp. |
| `updated_at` | Timestamp | Yes | Last update timestamp. |
| `disabled_at` | Timestamp | No | Set when user access is disabled. |

Constraints:

- `email` must be stored lowercase and trimmed.
- Disabled users must not authenticate, refresh tokens, or access protected organization/device APIs with existing access tokens.
- Self-service account deletion is implemented as account-manager user soft-disable by setting `disabled_at`; it does not remove organizations, memberships, devices, or product-level device state.

### `organization_members`

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `organization_id` | UUID | Yes | References `organizations.id`. |
| `user_id` | UUID | Yes | References `users.id`. |
| `role` | Text | Yes | One of `owner`, `admin`, `member`. |
| `created_at` | Timestamp | Yes | Membership creation timestamp. |
| `updated_at` | Timestamp | Yes | Last update timestamp. |

Constraints:

- `(organization_id, user_id)` is unique.
- Every organization must have at least one `owner`.
- The database must reject committing an organization without at least one `owner`.
- The database must reject deleting or downgrading the final `owner` membership for an organization.
- A user must not access organization resources without an active membership.

### `devices`

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `id` | UUID | Yes | Primary key, generated by server/database. |
| `organization_id` | UUID | Yes | References `organizations.id`. |
| `name` | Text | Yes | Device display name. |
| `category` | Text | Yes | One of `ip_camera`, `mqtt_device`, `generic`. |
| `serial_number` | Text | No | Unique per organization when present. |
| `mac_address` | Text | No | Optional hardware identifier. |
| `manufacturer` | Text | No | Optional manufacturer name. |
| `model` | Text | No | Optional model name. |
| `status` | Text | Yes | One of `unknown`, `online`, `offline`, `disabled`. |
| `last_seen_at` | Timestamp | No | Last known device activity time. |
| `metadata` | JSONB | Yes | Flexible device attributes, default `{}`. |
| `created_at` | Timestamp | Yes | Creation timestamp. |
| `updated_at` | Timestamp | Yes | Last update timestamp. |
| `disabled_at` | Timestamp | No | Set when device is disabled. |

Constraints:

- Device UUID is the canonical identifier.
- `name` must not be blank after trimming whitespace.
- `serial_number` is unique within the same organization when present.
- Device access is always scoped by `organization_id`.
- Soft-disabled devices remain readable but cannot be updated or have status changed.
- Repeating a delete on an already disabled device is idempotent and returns success.

### `device_groups`

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `id` | UUID | Yes | Primary key, generated by server/database. |
| `organization_id` | UUID | Yes | References `organizations.id`. |
| `name` | Text | Yes | Group display name, unique per organization. |
| `description` | Text | No | Optional group description. |
| `created_at` | Timestamp | Yes | Creation timestamp. |
| `updated_at` | Timestamp | Yes | Last update timestamp. |

Constraints:

- `name` must not be blank after trimming whitespace.
- `(organization_id, name)` is unique.
- Group access is always scoped by `organization_id`.

### `device_group_members`

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `organization_id` | UUID | Yes | References the owning organization through group and device constraints. |
| `group_id` | UUID | Yes | References `device_groups.id`. |
| `device_id` | UUID | Yes | References `devices.id`. |
| `created_at` | Timestamp | Yes | Assignment timestamp. |

Constraints:

- `(group_id, device_id)` is unique.
- Group assignments require the group and device to belong to the same organization.
- Repeating the same assignment is idempotent.
- Soft-disabled devices may remain in groups so registry selections can show disabled targets explicitly; executable operation owners must decide whether to skip them.

### `device_tags`

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `organization_id` | UUID | Yes | References the device organization. |
| `device_id` | UUID | Yes | References `devices.id`. |
| `tag` | Text | Yes | Organization-scoped selection label for the device. |
| `created_at` | Timestamp | Yes | Creation timestamp. |
| `updated_at` | Timestamp | Yes | Last update timestamp. |

Constraints:

- `tag` must not be blank after trimming whitespace.
- `(organization_id, device_id, tag)` is unique.
- Repeating the same tag assignment is idempotent.

### `refresh_tokens`

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `id` | UUID | Yes | Primary key. |
| `user_id` | UUID | Yes | References `users.id`. |
| `token_hash` | Text | Yes | Hash of refresh token. |
| `expires_at` | Timestamp | Yes | Expiration timestamp. |
| `revoked_at` | Timestamp | No | Set on logout or token revocation. |
| `created_at` | Timestamp | Yes | Creation timestamp. |

Refresh tokens must be stored hashed, not in raw form.

### `auth_tokens`

Stores one-time email verification and password reset tokens.

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `id` | UUID | Yes | Primary key. |
| `user_id` | UUID | Yes | References `users.id`. |
| `purpose` | Text | Yes | One of `email_verification`, `password_reset`. |
| `token_hash` | Text | Yes | Unique hash of the one-time token. |
| `expires_at` | Timestamp | Yes | Expiration timestamp. |
| `consumed_at` | Timestamp | No | Set after successful one-time use. |
| `created_at` | Timestamp | Yes | Creation timestamp. |

Auth tokens must be stored hashed, not in raw form, and are throttled by
`user_id` and `purpose`.

### `quota_raise_requests`

Tracks evaluation-tier quota raise requests and platform-admin decisions.

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `id` | UUID | Yes | Primary key. |
| `organization_id` | UUID | Yes | References the evaluation organization. |
| `requested_by` | UUID | Yes | References the requesting user. |
| `requested_quota` | Integer | Yes | Requested quota, constrained to 1-200. |
| `use_case` | Text | Yes | User-submitted use case. |
| `contact_info` | JSONB | Yes | Contact metadata for follow-up. |
| `status` | Text | Yes | One of `pending`, `approved`, `declined`. |
| `decided_by` | UUID | No | Platform admin who made the decision. |
| `decision_reason` | Text | No | Decision note. |
| `created_at` | Timestamp | Yes | Creation timestamp. |
| `updated_at` | Timestamp | Yes | Last update timestamp. |
| `decided_at` | Timestamp | No | Decision timestamp. |

### `audit_events`

Records evaluation-tier lifecycle and administrative audit facts.

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `id` | UUID | Yes | Primary key. |
| `event_type` | Text | Yes | Stable audit event type. |
| `actor_user_id` | UUID | No | User responsible for the event when known. |
| `organization_id` | UUID | No | Related organization when known. |
| `subject_type` | Text | Yes | Type of audited subject. |
| `subject_id` | Text | Yes | Identifier of audited subject. |
| `payload` | JSONB | Yes | Event details, default `{}`. |
| `created_at` | Timestamp | Yes | Creation timestamp. |
| `updated_at` | Timestamp | Yes | Last update timestamp. |

### `device_operations` (V2)

Tracks idempotent provisioning and deactivation operations.

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `id` | UUID | Yes | Primary key. |
| `operation_id` | Text | Yes | Unique business idempotency key. |
| `correlation_id` | Text | Yes | Groups cross-service workflow messages. |
| `organization_id` | UUID | Yes | References `organizations.id`. |
| `device_id` | UUID | Yes | References `devices.id`. |
| `operation_type` | Text | Yes | One of `provision`, `deactivate`. |
| `status` | Text | Yes | One of the V2 operation statuses. |
| `requested_by` | UUID | No | User or service that requested the operation. |
| `request_payload` | JSONB | Yes | Original normalized request payload. |
| `result_payload` | JSONB | Yes | Result payload, default `{}`. |
| `error_code` | Text | No | Stable error code from projection or worker failure. |
| `error_message` | Text | No | Human-readable error details. |
| `retryable` | Boolean | No | Whether the failure may be retried. |
| `created_at` | Timestamp | Yes | Creation timestamp. |
| `updated_at` | Timestamp | Yes | Last update timestamp. |
| `completed_at` | Timestamp | No | Set when operation reaches a terminal state. |

Constraints:

- `operation_id` is unique and prevents duplicate business side effects.
- Duplicate `operation_id` with the same normalized request payload returns the existing operation.
- Duplicate `operation_id` with a conflicting payload returns `409 Conflict`.

### `device_message_outbox` (V2)

Stores account-side commands before broker publication.

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `id` | UUID | Yes | Primary key. |
| `message_id` | Text | Yes | Unique delivery message ID. |
| `operation_id` | Text | Yes | References the business operation. |
| `correlation_id` | Text | Yes | Cross-service workflow correlation ID. |
| `causation_id` | Text | No | Message that caused this message, when known. |
| `stream` | Text | Yes | Expected `account.video.commands`. |
| `message_type` | Text | Yes | Supported account-to-video command type. |
| `schema_version` | Text | Yes | Supported payload schema version. |
| `partition_key` | Text | Yes | Account-manager `device_id`. |
| `payload` | JSONB | Yes | Message payload. |
| `status` | Text | Yes | One of `pending`, `published`, `retrying`, `dead_lettered`. |
| `attempt_count` | Integer | Yes | Publish attempts. |
| `last_error` | Text | No | Last publish error. |
| `available_at` | Timestamp | Yes | Earliest retry/publish time. |
| `published_at` | Timestamp | No | Successful publish time. |
| `created_at` | Timestamp | Yes | Creation timestamp. |
| `updated_at` | Timestamp | Yes | Last update timestamp. |

Constraints:

- `message_id` is unique.
- Device lifecycle `partition_key` must equal account-manager `device_id`.

### `device_message_inbox` (V2)

Stores consumed video-side events and deduplication state.

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `id` | UUID | Yes | Primary key. |
| `message_id` | Text | Yes | Unique broker message ID used for deduplication. |
| `operation_id` | Text | Yes | Business operation ID. |
| `correlation_id` | Text | Yes | Cross-service workflow correlation ID. |
| `causation_id` | Text | No | Message that caused this event, when known. |
| `stream` | Text | Yes | Expected `video.account.events`. |
| `message_type` | Text | Yes | Supported video-to-account event type. |
| `schema_version` | Text | Yes | Supported payload schema version. |
| `partition_key` | Text | Yes | Account-manager `device_id`. |
| `payload` | JSONB | Yes | Message payload. |
| `status` | Text | Yes | One of `processed`, `failed`, `retrying`, `dead_lettered`. |
| `attempt_count` | Integer | Yes | Projection attempts. |
| `last_error` | Text | No | Last projection error. |
| `received_at` | Timestamp | Yes | Broker receive time. |
| `processed_at` | Timestamp | No | Successful projection time. |
| `created_at` | Timestamp | Yes | Creation timestamp. |
| `updated_at` | Timestamp | Yes | Last update timestamp. |

Constraints:

- `message_id` is unique and deduplicates broker redelivery.
- Unknown message types, invalid schema versions, and unmapped devices must not be silently dropped.

## 5. Authentication

- Users authenticate with email and password.
- Passwords must be hashed with a modern password hashing algorithm.
- API access uses JWT bearer tokens.
- Protected endpoints require `Authorization: Bearer <access_token>`.
- Refresh tokens may be used to issue new access tokens.
- Refreshing a session rotates the refresh token. The previous refresh token is revoked and must not be accepted again.
- Logout revokes the active refresh token.
- `POST /v1/auth/verify-email` consumes a one-time email verification token
  and marks `user.email_verified=true`.
- `POST /v1/auth/resend-verification` issues a replacement verification token
  for unverified users and returns an enumeration-safe `202 Accepted` for
  unknown, already verified, disabled, or throttled users.
- `POST /v1/auth/forgot-password` issues a password reset token and returns an
  enumeration-safe `202 Accepted` for unknown, disabled, or throttled users.
- `POST /v1/auth/reset-password` consumes a one-time password reset token,
  updates the password, and revokes all active refresh tokens for the user.
- `PATCH /v1/me/password` lets the authenticated current user change their password after presenting the current password and a new password of at least 8 characters.
- Password change revokes all active refresh tokens for the user. Existing access tokens remain valid until their normal expiry.
- `DELETE /v1/me` lets the authenticated current user disable their own account. The operation revokes active refresh tokens and refuses to disable the user while they are the last active owner of any organization.
- Self-service account deletion is account-manager user lifecycle only. It preserves organization memberships and registry/device records, and it does not imply product-level device deletion or deactivation.
- Verification and password reset tokens expire after 30 minutes, are stored
  hashed, become one-time-use after consumption, and are throttled to five
  issued tokens per user/purpose per hour.
- Auth token delivery is an explicit adapter boundary. The API process accepts
  an `AuthTokenSink` implementation for email/SMS/dev-test delivery. The
  production server entrypoint wires `AUTH_TOKEN_DELIVERY=log` as the local
  dev/test adapter so one-time verification and reset tokens are emitted to the
  server log until a real mail or SMS adapter is configured.
- Quota-raise decision delivery is also an explicit adapter boundary. The API
  process accepts a `QuotaRaiseNotificationSink` for approval/decline
  notifications. The production server entrypoint wires SMTP delivery when
  `SMTP_HOST` and `SMTP_FROM` are configured, and otherwise falls back to the
  local log adapter so quota decisions are observable in dev/test until a real
  mail adapter is configured.
- Third-party/social login is a deferred first-phase lifecycle capability and
  must not be presented as available API behavior until implemented.
- Expired or revoked refresh tokens may be removed by an explicit maintenance command.

### Self-Service Evaluation Tier

`rtk_cloud_workspace/docs/business-model.md` defines a public evaluation tier
(default 5 devices, ceiling 200 on request, non-commercial use) and a private
commercial tier (no minimum scale, one-time license + annual maintenance).
Account manager is the planned owner of the API surface that supports the
evaluation tier:

- `POST /v1/auth/signup` creates an evaluation-tier user and initial
  organization in a signup-pending state, issues an email verification token,
  and returns `202 Accepted` without login tokens.
- `POST /v1/auth/verify-email` consumes the verification token and clears the
  signup-pending state so the account can log in.
- The existing `POST /v1/auth/register` endpoint remains the internal-use path
  for account creation that is not part of the public signup flow.
- Organizations carry tier metadata (`tier ∈ {evaluation, commercial}`) and an
  evaluation device quota (`evaluation_device_quota`, default `5`, maximum
  `200`). Device registration rejects evaluation-tier organizations whose
  active device count would exceed the quota with typed error
  `EVALUATION_QUOTA_EXCEEDED`; commercial-tier organizations ignore the quota.
- `POST /v1/orgs/{org_id}/quota-raise-requests` stores a user-submitted quota
  raise request with requested quota, use case, and contact info. Platform
  admin approval or decline updates the request status and applies approved
  quotas up to the 200-device ceiling.
- The evaluation-tier lifecycle emits audit events for signup, email
  verification, and quota-raise submission/decision, and the admin metrics
  snapshot surfaces signup counts, verification completion, quota-raise
  status counts, and live quota utilization for evaluation organizations.

The implementation in this repository should stay aligned with the paired
wire-contract updates in `rtk_cloud_contracts_doc` before the issue is closed.

## 6. Authorization

Authorization is organization scoped.

| Role | Permissions |
| --- | --- |
| `owner` | Manage organization, manage members, manage devices, view all organization resources. |
| `admin` | Manage devices, view organization members, view organization resources. |
| `member` | View organization resources and devices. |

Rules:

- Only `owner` may invite/add members, remove members, or change member roles.
- Only `owner` may disable or enable member user accounts.
- Only `owner` may remove another `owner`.
- The last active `owner` in an organization must not be removed, downgraded, or disabled; disabled owner memberships do not satisfy this invariant.
- Disabling a member user sets `users.disabled_at`, revokes that user's active refresh tokens, and prevents login, refresh, and protected API access.
- Enabling a member user clears `users.disabled_at`.
- `owner` and `admin` may create, update, disable, delete, and update status for devices.
- `owner` and `admin` may create, update, delete, and assign device groups and tags.
- `owner` and `admin` may initiate provisioning and deactivation operations for devices.
- `member` may list and read devices but may not modify them.
- `member` may list groups, group devices, and tags but may not modify them.
- `member` may read provisioning state but may not initiate provisioning or deactivation.
- No user may access an organization without an active membership.
- No endpoint may allow cross-organization device access.

## 7. API Contract

All endpoints are versioned under `/v1`.

### Auth

| Method | Path | Auth | Description |
| --- | --- | --- | --- |
| `POST` | `/v1/auth/register` | No | Create a user and initial organization. |
| `POST` | `/v1/auth/signup` | No | Create a public evaluation-tier account in signup-pending verification state. |
| `POST` | `/v1/auth/login` | No | Authenticate with email/password. |
| `POST` | `/v1/auth/refresh` | No | Exchange refresh token for new access token. |
| `POST` | `/v1/auth/verify-email` | No | Consume an email verification token and mark the user email verified. |
| `POST` | `/v1/auth/resend-verification` | No | Issue a replacement email verification token for an unverified user, with enumeration-safe response semantics. |
| `POST` | `/v1/auth/forgot-password` | No | Issue a password reset token, with enumeration-safe response semantics. |
| `POST` | `/v1/auth/reset-password` | No | Consume a password reset token, set a new password, and revoke active refresh tokens. |
| `POST` | `/v1/auth/logout` | Yes | Revoke current refresh token/session. |
| `GET` | `/v1/me` | Yes | Return current user and memberships. |
| `DELETE` | `/v1/me` | Yes | Disable current user account and revoke refresh tokens. |
| `PATCH` | `/v1/me/password` | Yes | Change current user password and revoke refresh tokens. |

### Organizations and Members

| Method | Path | Auth | Description |
| --- | --- | --- | --- |
| `GET` | `/v1/orgs` | Yes | List organizations current user belongs to. |
| `POST` | `/v1/orgs` | Yes | Create a new organization with current user as `owner`. |
| `GET` | `/v1/orgs/:orgId` | Yes | Get organization details. |
| `PATCH` | `/v1/orgs/:orgId` | Yes | Update organization details. |
| `POST` | `/v1/orgs/:orgId/quota-raise-requests` | Yes | Submit an evaluation-tier quota raise request. |
| `GET` | `/v1/orgs/:orgId/members` | Yes | List organization members. |
| `POST` | `/v1/orgs/:orgId/members` | Yes | Add a user to the organization. |
| `PATCH` | `/v1/orgs/:orgId/members/:userId` | Yes | Update member role. |
| `PATCH` | `/v1/orgs/:orgId/members/:userId/disable` | Yes | Disable a member user account and revoke active refresh tokens. |
| `PATCH` | `/v1/orgs/:orgId/members/:userId/enable` | Yes | Re-enable a disabled member user account. |
| `DELETE` | `/v1/orgs/:orgId/members/:userId` | Yes | Remove member from organization. |

### Platform Admin

| Method | Path | Auth | Role | Description |
| --- | --- | --- | --- | --- |
| `POST` | `/v1/admin/quota-raise-requests/:requestId/approve` | Yes | Platform admin | Approve a pending quota raise request and apply the approved evaluation quota. |
| `POST` | `/v1/admin/quota-raise-requests/:requestId/decline` | Yes | Platform admin | Decline a pending quota raise request with an optional decision reason. |
| `GET` | `/v1/admin/metrics` | Yes | Platform admin | Return evaluation signup, verification, quota request, and quota utilization metrics. |

### Devices

| Method | Path | Auth | Description |
| --- | --- | --- | --- |
| `POST` | `/v1/orgs/:orgId/devices` | Yes | Create device. |
| `GET` | `/v1/orgs/:orgId/devices` | Yes | List devices. |
| `GET` | `/v1/orgs/:orgId/devices/:deviceId` | Yes | Get device details. |
| `PATCH` | `/v1/orgs/:orgId/devices/:deviceId` | Yes | Update device fields. |
| `DELETE` | `/v1/orgs/:orgId/devices/:deviceId` | Yes | Soft-disable device by setting `status` to `disabled` and `disabled_at`. |
| `PATCH` | `/v1/orgs/:orgId/devices/:deviceId/status` | Yes | Update device status. |

### Fleet Registry

| Method | Path | Auth | Role | Description |
| --- | --- | --- | --- | --- |
| `GET` | `/v1/orgs/:orgId/device-groups` | Yes | `owner`, `admin`, `member` | List organization device groups. |
| `POST` | `/v1/orgs/:orgId/device-groups` | Yes | `owner`, `admin` | Create a device group. |
| `GET` | `/v1/orgs/:orgId/device-groups/:groupId` | Yes | `owner`, `admin`, `member` | Get a device group. |
| `PATCH` | `/v1/orgs/:orgId/device-groups/:groupId` | Yes | `owner`, `admin` | Update a device group. |
| `DELETE` | `/v1/orgs/:orgId/device-groups/:groupId` | Yes | `owner`, `admin` | Delete a device group and its assignments. |
| `GET` | `/v1/orgs/:orgId/device-groups/:groupId/devices` | Yes | `owner`, `admin`, `member` | List devices assigned to a group. |
| `PUT` | `/v1/orgs/:orgId/device-groups/:groupId/devices/:deviceId` | Yes | `owner`, `admin` | Add a device to a group; repeated assignment is idempotent. |
| `DELETE` | `/v1/orgs/:orgId/device-groups/:groupId/devices/:deviceId` | Yes | `owner`, `admin` | Remove a device from a group; repeated removal is idempotent when both resources exist. |
| `GET` | `/v1/orgs/:orgId/devices/:deviceId/tags` | Yes | `owner`, `admin`, `member` | List tags for a device. |
| `PUT` | `/v1/orgs/:orgId/devices/:deviceId/tags/:tag` | Yes | `owner`, `admin` | Add a tag to a device; repeated assignment is idempotent. |
| `DELETE` | `/v1/orgs/:orgId/devices/:deviceId/tags/:tag` | Yes | `owner`, `admin` | Remove a device tag; repeated removal is idempotent when the device exists. |

Fleet registry APIs are selection primitives only. Account manager owns group, tag, and device UUID facts; OTA campaign execution, command dispatch, firmware rollout policy, certificate lifecycle, video-side operations, and frontend console workflows remain outside this repository until a linked follow-up deliberately adds that scope.

### Device Provisioning (V2)

| Method | Path | Auth | Role | Description |
| --- | --- | --- | --- | --- |
| `POST` | `/v1/orgs/:orgId/devices/:deviceId/provision` | Yes | `owner`, `admin` | Create or reuse a provisioning operation and enqueue `DeviceProvisionRequested`. |
| `GET` | `/v1/orgs/:orgId/devices/:deviceId/provisioning` | Yes | `owner`, `admin`, `member` | Return latest provisioning operation, account-side readiness projection, and projected video metadata. |
| `POST` | `/v1/orgs/:orgId/devices/:deviceId/deactivate` | Yes | `owner`, `admin` | Create or reuse a deactivation operation and enqueue `DeviceDeactivateRequested`. |
| `POST` | `/v1/orgs/:orgId/devices/claim/resolve` | Yes | `owner`, `admin` | Resolve an opaque Claim Token into an organization-owned registry device and provisioning input. |

Provision request body:

```json
{
  "video_cloud_devid": "device-1",
  "activity_id": "activity-1",
  "clip_public_key": "<clip-public-key>",
  "operation_id": "optional-client-idempotency-key"
}
```

Current claim and bind ownership policy:

Account manager owns the final account-side claim/bind authorization decision
for organization-owned registry devices. SDKs, apps, and integration services
may parse or submit normalized claim material, but they must not decide final
ownership locally.

The current implemented `/provision` API does not create a device from raw claim
material. A caller must first address an existing enabled registry device by
`organization_id` and account-manager `device_id`. The registry device remains
owned by that organization; submitted video lifecycle material is stored as
provisioning request payload and projected video metadata, not as a replacement
for account-manager ownership.

The shared contracts define the first implemented app-facing Claim Token
resolution surface:

```http
POST /v1/orgs/:orgId/devices/claim/resolve
```

The implemented endpoint accepts an opaque `claim_token` plus a display name,
makes the account-manager-owned claim/bind policy decision, creates or matches the
organization registry device, and returns `claim_id`, `device`, and
`provision_input` containing `video_cloud_devid`, `activity_id`, and
`clip_public_key`. Claim resolution remains separate from cloud provisioning:
resolving a Claim Token does not create a provisioning operation, publish an
outbox message, or call Realtek video server directly.

Raw claim-material endpoint decision:

- `rtk_account_manager` exposes the contract-defined
  `POST /v1/orgs/:orgId/devices/claim/resolve` endpoint as the first app-facing
  Claim Token flow. Broader transfer, reset, and already-claimed policy
  extensions remain behind future endpoints.
- The current `POST .../provision` endpoint only accepts the existing-device
  video lifecycle fields `video_cloud_devid`, `activity_id`, and
  `clip_public_key`, plus optional `operation_id`.
- The SDK claim-material parsers normalize QR payloads, serial numbers,
  activation codes, MAC addresses, and factory identities into typed
  `ClaimMaterial`; they do not resolve account ownership or start account/video
  provisioning.
- Standalone or raw claim fields such as `claim_material`, `qr_code`,
  SDK-normalized `qr_payload`, `serial_number`, `activation_code`,
  `mac_address`, and `factory_identity` are rejected by this endpoint instead
  of being ignored or treated as authoritative ownership keys.
- Claim Token resolve must first resolve claim material to exactly one
  account-manager registry device or create an explicit pending claim record
  before the caller may start cloud provisioning.

Device categories and video credential scope:

Account-manager device categories (`ip_camera`, `mqtt_device`, and `generic`)
are product registry categories. They are not Realtek video server credential
scope names and they must not be used to infer which video-cloud token family is
issued. Any registry device category may participate in the account/video
lifecycle flow when the device has, or claim resolution returns, a valid
`video_cloud_devid` mapping plus the required video lifecycle input.

Account manager does not issue Realtek video server device tokens,
device-bound `camera` tokens, app/subscriber scoped tokens, or device
certificates. It persists the account registry record, creates/reuses lifecycle
operations, publishes/projections lifecycle messages, and stores the projected
`video_cloud_*` metadata. Realtek video server or the video-side integration
layer remains responsible for issuing subject-bound credentials and accepting
websocket/MQTT owner transport.

Accepted lifecycle input in the current API:

| Registry category | Current account-manager lifecycle input | Notes |
| --- | --- | --- |
| `ip_camera` | `video_cloud_devid`, `activity_id`, `clip_public_key` | Supported by `POST .../provision` when mapped to a Realtek video identity. The eventual video-side credentials are device-bound to `video_cloud_devid`, not to the account-manager category string. |
| `mqtt_device` | `video_cloud_devid`, `activity_id`, `clip_public_key` when the device maps to Realtek video cloud | The category may describe the product registry entry or preferred transport, but MQTT broker credentials and factory identity parsing remain outside this account-manager API. Video-cloud subject-bound tokens are still issued by the video side. |
| `generic` | `video_cloud_devid`, `activity_id`, `clip_public_key` when the device maps to Realtek video cloud | Generic registry entries can still be bound to a video-cloud device identity. Serial-number, QR-code, activation-code, MAC-address, and future factory-identity claim flows remain out of scope unless a later endpoint explicitly accepts them. |

Ownership consequences:

- `owner` and `admin` members of the target organization may start claim/bind
  provisioning for an existing enabled device; `member` may only read the
  resulting provisioning state.
- Cross-organization binding is rejected by normal organization/device access
  checks. A user cannot bind a device record in an organization where they do
  not have an active membership.
- Account-manager `device_id` remains the canonical owner record. External
  fields such as `serial_number`, `mac_address`, `video_cloud_devid`, QR data,
  or activation codes must not be treated as a server-authoritative ownership
  key by clients.
- Reusing an explicit `operation_id` with the same normalized request is
  idempotent and returns the existing operation. Reusing it with different claim
  material returns `409 Conflict`.
- The current account-manager API does not implement transfer between
  organizations, transfer between users, or account-side factory-reset reclaim.
  A factory reset or transfer intent does not clear account ownership by itself;
  use an explicit future transfer/reclaim endpoint once one exists.
- If the same video-side identity is already claimed or rejected by downstream
  product policy, account manager preserves the lifecycle operation and exposes
  the terminal failure fields projected from the video-side result instead of
  silently rebinding the registry device.
- Registry soft-delete and product-level deactivation remain distinct. Deleting
  the account-manager registry device disables that registry record only; it
  does not transfer ownership, factory-reset the product, or enqueue product
  teardown.

Claim transfer, reclaim, and factory-reset policy:

- Already-claimed Claim Tokens remain rejected by `POST
  /v1/orgs/:orgId/devices/claim/resolve`. This applies to both the same
  organization and a different organization. Same-organization repeat use
  should direct clients to the existing registry device or provisioning state,
  not create another device or claim.
- Cross-organization already-claimed tokens remain rejected. Account manager
  must not infer transfer intent from possession of a token, QR code, serial
  number, MAC address, factory identity, or a product factory reset event.
- Registry soft-delete does not release Claim Token material, clear the
  `device_claims` record, or make the token reusable. Soft-delete only disables
  the account registry row.
- Product-level deactivation does not release Claim Token material by itself.
  It proves product teardown was requested/completed; it does not authorize a
  new account owner.
- Factory reset does not allow automatic reclaim in account manager. Reclaim
  requires an explicit future account-manager operation with platform policy,
  operator authorization, and audit evidence.
- Operator override, transfer, or reclaim must require `platform_admin` unless
  a later product policy deliberately defines a narrower self-service path.
- Every future transfer/reclaim/override operation must emit an audit event
  with actor user id, source organization, target organization when known,
  claim token id or device id, reason, and before/after ownership facts.

Future endpoint proposal:

| Method | Path | Role | Purpose |
| --- | --- | --- | --- |
| `POST` | `/v1/admin/device-claims/:claimId/transfer` | Platform admin | Transfer an already-resolved claim to another organization after product-policy checks. |
| `POST` | `/v1/admin/device-claim-tokens/:tokenId/reclaim` | Platform admin | Mark a claimed token/device eligible for a controlled reclaim flow after factory-reset or support verification. |

Until those endpoints are implemented, the system must continue to reject:

- Same-organization repeated Claim Token resolve after `claimed_at` is set.
- Cross-organization Claim Token resolve after `claimed_at` is set.
- Reclaim based only on registry soft-delete.
- Reclaim based only on product deactivation.
- Reclaim based only on factory reset.

Deactivation request body:

```json
{
  "reason": "user_request",
  "operation_id": "optional-client-idempotency-key"
}
```

Operation response body for `POST .../provision` and `POST .../deactivate`:

```json
{
  "operation": {
    "operation_id": "op-01H...",
    "correlation_id": "op-01H...",
    "message_id": "msg-01H...",
    "device_id": "account-device-uuid",
    "operation_type": "provision",
    "status": "pending",
    "requested_by": "user-uuid",
    "created_at": "2026-04-29T04:00:00Z",
    "updated_at": "2026-04-29T04:00:00Z"
  }
}
```

Provisioning-state response body for `GET .../provisioning`:

```json
{
  "operation": {
    "operation_id": "op-01H...",
    "correlation_id": "op-01H...",
    "message_id": "msg-01H...",
    "device_id": "account-device-uuid",
    "operation_type": "provision",
    "status": "succeeded",
    "requested_by": "user-uuid",
    "created_at": "2026-04-29T04:00:00Z",
    "updated_at": "2026-04-29T04:01:30Z",
    "completed_at": "2026-04-29T04:01:30Z"
  },
  "readiness": {
    "state": "transport_pending",
    "product_state": "activated",
    "sources": {
      "device_enabled": true,
      "device_status": "offline",
      "provisioning_operation_status": "succeeded",
      "video_cloud_activation_status": "activated"
    }
  },
  "video_metadata": {
    "video_cloud_devid": "device-1",
    "video_cloud_activation_status": "activated",
    "video_cloud_activity_id": "activity-1",
    "video_cloud_activated_at": "2026-04-29T04:01:30Z"
  }
}
```

Provisioning rules:

- The API writes the operation row and outbox row transactionally.
- Accepting a provisioning request immediately merges pending `video_cloud_devid`, `video_cloud_activity_id`,
  `video_cloud_clip_public_key`, and `video_cloud_activation_status=pending` into device metadata without implying activation success.
- Disabled devices cannot be provisioned.
- The API must not directly call Realtek video server.
- Product-level deactivation and account registry soft-delete are distinct operations.
- `DELETE /devices/:deviceId` remains an account-registry soft-disable and does not enqueue `DeviceDeactivateRequested`.
- Registry soft-delete does not transfer ownership, release claim material, or
  prove product-level deactivation. Product teardown requires `POST
  .../deactivate` and a terminal video-side deactivation result.
- Omitting the deactivation `reason` defaults the outbox payload to `account_device_disabled`.
- Reusing an explicit `operation_id` returns the existing operation when the normalized request matches and returns `409 Conflict` when it does not.
- Operation responses may also include `error_code`, `error_message`, `retryable`, and `completed_at` once the inbox projection records a terminal result.
- `DeviceProvisionSucceeded` replaces the pending activation metadata with the terminal activation result but does not set account-manager `status=online`.
- `DeviceOnlineChanged` is the only video-side event that may project account-manager `status=online|offline`.

### Product Readiness Contract

`rtk_account_manager` exposes an account-side readiness projection on
`GET /v1/orgs/:orgId/devices/:deviceId/provisioning`. This is not a final
cross-service "product ready" boolean: account manager only composes the facts
it owns locally, while the integrating client or service remains responsible
for adding video-side token and bootstrap inputs that do not live in this
repository.

The response keeps the legacy account-side `readiness.state` for existing
clients and also exposes `readiness.product_state` using the shared
product-readiness vocabulary. The product vocabulary field is still derived
only from account-manager-owned durable source facts; it does not accept direct
writes and it does not imply account manager owns SDK/local onboarding,
credential issuance, or video bootstrap facts.

Current readiness inputs:

| Input | Source of truth | Meaning |
| --- | --- | --- |
| Account registry record exists and is enabled | `rtk_account_manager` device APIs | The organization-scoped device record exists and is not soft-disabled. |
| Provisioning operation accepted | `POST /provision`, `GET /provisioning` | Account side persisted the lifecycle operation and outbox command. |
| Video activation result projected | `GET /provisioning`, device `video_cloud_*` metadata | Realtek video activation succeeded or failed for the mapped device identity. |
| Subject-bound token issuance completed | Video-side or integration-service auth surface | The device or app has the video-cloud scoped credentials required for product use. These credentials are bound to the video-cloud subject such as `video_cloud_devid`; account-manager category names do not define credential scope. |
| Video-side bootstrap prerequisites completed when required | Video-side APIs | Device info/config setup or equivalent downstream bootstrap state is available. |
| Owner transport connected | Account-manager device `status`, projected from `DeviceOnlineChanged` | The mapped device has come online through a supported owner transport, currently websocket or MQTT in the video transport contract. |

Unified product-readiness source ownership:

| Source fact | Owning service | Account-manager responsibility |
| --- | --- | --- |
| Organization membership and role authorization | `rtk_account_manager` | Authorize org-scoped readiness reads for `owner`, `admin`, and `member`. |
| Registry existence, disabled state, category, serial, groups, and tags | `rtk_account_manager` | Return current account registry facts and preserve account-side soft-delete semantics. |
| Claim Token resolution and account/device binding | `rtk_account_manager` | Return claim/bind result facts and reject already-claimed transfer/reclaim without explicit platform-admin workflow. |
| Provision/deactivate operation state | `rtk_account_manager` | Return operation status, idempotency id, failure attribution, retryability, and timestamps from durable operation rows. |
| Projected video activation/deactivation metadata | `rtk_account_manager` from `video.account.events` | Return last projected `video_cloud_*` metadata and last video-side error facts; do not invent video-side state that has not been projected. |
| Subject-bound video token issuance | Video cloud or integration auth service | Account manager may reference externally supplied status in a future composition response, but it must not mint or validate video scoped credentials in this repo. |
| Device info/config/bootstrap prerequisites | Video cloud or product bootstrap service | Account manager may carry projected summary facts only after a contract defines the event/API source. |
| Owner transport/session readiness | Video transport service, currently projected into account device `status` from `DeviceOnlineChanged` | Account manager exposes the projected online/offline account fact, but the final transport/session owner remains the video transport contract. |
| Final product-ready decision | Integrating service, BFF, or future cross-service readiness endpoint | Account manager must either return `unknown` for non-owned facts or require explicit upstream inputs; it must not collapse missing external facts into success. |

Unified readiness composition options:

| Option | Shape | When to choose |
| --- | --- | --- |
| Extend existing `/provisioning` response | Add optional externally supplied source summaries while preserving existing `readiness.state`, `readiness.product_state`, `operation`, and `video_metadata` fields. | Only if existing clients can ignore new fields and account-manager remains the primary read surface. |
| Add `GET /v1/orgs/:orgId/devices/:deviceId/readiness` | Return a composed readiness document with account, video, token, bootstrap, and transport source blocks. | Preferred if account manager becomes the backend aggregator for device readiness. |
| Leave composition to `rtk_cloud_admin` or another integration service | Keep account manager as the account-side facts API and let a BFF compose final product status. | Preferred if WebUI/dashboard or cross-service orchestration owns product-level UX. |

If a future account-manager readiness endpoint is implemented, the minimum
response shape should be explicit about unknown and externally owned facts:

```json
{
  "device_id": "device-uuid",
  "organization_id": "org-uuid",
  "product_ready": false,
  "state": "activation_pending",
  "sources": {
    "account": {
      "owned_by": "rtk_account_manager",
      "state": "registered",
      "updated_at": "2026-05-13T00:00:00Z"
    },
    "video_activation": {
      "owned_by": "rtk_video_cloud",
      "state": "unknown"
    },
    "token": {
      "owned_by": "rtk_video_cloud",
      "state": "unknown"
    },
    "bootstrap": {
      "owned_by": "product_bootstrap",
      "state": "unknown"
    },
    "transport": {
      "owned_by": "video_transport",
      "state": "offline"
    }
  },
  "failure": null
}
```

Unified readiness authorization rules:

- `owner`, `admin`, and `member` may read readiness for devices in their
  organization.
- Platform-admin may read cross-organization readiness only through an explicit
  admin endpoint or support workflow, not by bypassing org-scoped handlers.
- Cross-organization access must return `404 Not Found` or equivalent
  non-disclosing behavior.
- A composed readiness endpoint must not expose raw tokens, secrets, DSNs,
  credential material, or unredacted video service diagnostics.

Unified readiness failure semantics:

- Use source-specific failure attribution. Do not overwrite account-manager
  provisioning or deactivation results with token/bootstrap/transport failures.
- A source may be `unknown`, `pending`, `ready`, `failed`, or `not_applicable`.
- `product_ready=true` requires every required source to be `ready` or
  explicitly `not_applicable`.
- If any required source is `failed`, the response must include the owning
  service, source state, machine-readable error code when available,
  retryability, and occurrence time.
- If any required source is `unknown`, the final product state must not be
  reported as ready.

Account-side readiness projection rules:

- Treat `GET /v1/orgs/:orgId/devices/:deviceId/provisioning` as the
  account-side lifecycle and readiness projection, not as a final full-product
  readiness endpoint.
- Do not treat `DeviceProvisionSucceeded` by itself as product-ready; it only
  proves activation metadata was projected.
- Do not treat account-manager `status=online` by itself as product-ready; it
  does not prove activation or token issuance completed.
- The `readiness.sources` object identifies the local facts used for the
  aggregate state: registry enabled/disabled state, account device status,
  latest provisioning operation status, projected video activation status,
  latest deactivation operation status, and projected video last-error data.
- Compose the final readiness view from this account-manager projection plus
  the required video-side credential, bootstrap, and websocket/MQTT owner
  transport signals.

`readiness.product_state` maps account-side facts into the cross-service
vocabulary as follows:

| Product state | Account-manager projection rule |
| --- | --- |
| `registered` | The account registry record exists but account-manager facts do not show active provisioning, or the registry record is soft-disabled without a terminal product deactivation fact. |
| `cloud_activation_pending` | Provisioning is accepted or not terminal, or activation metadata has not yet projected a terminal result. |
| `activated` | Provisioning succeeded and activation metadata is `activated`, but owner transport is not currently `online`. |
| `online` | Provisioning succeeded, activation metadata is `activated`, and account-manager transport status is `online`. |
| `failed` | Provisioning, deactivation, projected activation metadata, or projected `video_cloud_last_error` records a failure. |
| `deactivation_pending` | Latest deactivation operation is pending, published, or retrying. |
| `deactivated` | Latest deactivation operation succeeded or projected activation status is `deactivated`. |

The shared states `claim_pending` and `local_onboarding_pending` remain outside
the current account-manager projection until this repository owns durable
source facts for those phases.

Account-side readiness states:

| State | Required signals | Meaning |
| --- | --- | --- |
| `activation_pending` | Provisioning operation is `pending`, `published`, or `retrying` | Account side accepted provisioning, but no terminal activation result is projected yet. |
| `activation_failed` | Provisioning operation is `failed` or `dead_lettered`, or projected metadata records `video_cloud_last_error` | Activation did not complete; clients must surface the failure instead of claiming readiness. |
| `transport_pending` | Provisioning operation is `succeeded`, activation metadata is `activated`, and account device status is not `online` | Video activation completed, but the device has not connected through owner transport. |
| `ready` | Provisioning operation is `succeeded`, activation metadata is `activated`, and account device status is `online` | Account-manager-owned readiness facts are complete and the device is currently connected. |
| `deactivation_pending` | Latest deactivation operation is `pending`, `published`, or `retrying` | Product deactivation was requested and has not reached a terminal projection. |
| `deactivation_failed` | Latest deactivation operation is `failed` or `dead_lettered` | Product deactivation did not complete; clients must surface the failure. |
| `deactivated` | Latest deactivation operation is `succeeded` or projected activation status is `deactivated` | Product deactivation completed on the video side. |
| `disabled` | Account-manager registry record is soft-disabled without a newer product deactivation state | The account registry record is disabled; this does not imply product-side video deactivation completed. |

Failure handling rules:

- If activation fails, surface the provisioning operation error fields and
  projected `video_cloud_last_error`; do not silently collapse back to
  `activation_pending`.
- If video-cloud scoped token issuance, device certificate issuance/renewal,
  or other video-side bootstrap fails after activation,
  surface that outside the account-manager readiness projection rather than
  rewriting the account-manager provisioning result.
- If the device never projects `online`, keep the readiness view in
  `transport_pending` or a more specific post-activation state instead of
  claiming full success.
- A future unified readiness endpoint that includes token/bootstrap signals
  would require a separate API/OpenAPI/test change set.

Testing expectations for future unified readiness implementation:

- Account-only tests must continue to prove `/provisioning` compatibility.
- Contract tests must validate any new readiness endpoint against OpenAPI.
- Integration tests must cover missing, unknown, pending, ready, failed, and
  not-applicable source combinations.
- Authorization tests must cover owner/admin/member, platform-admin support
  reads if implemented, disabled users, and cross-organization non-disclosure.
- Test-report correctness gates must distinguish account-side readiness from
  unified product readiness.

## 8. API Conventions

- Request and response bodies use JSON.
- IDs are UUID strings.
- Timestamps use RFC 3339 format.
- List endpoints support pagination with `limit` and `offset`.
- List responses include `pagination.limit`, `pagination.offset`, and `pagination.total`.
- Error responses use a consistent JSON shape:

```json
{
  "error": {
    "code": "string_code",
    "message": "Human readable message"
  }
}
```

Recommended status codes:

- `200 OK` for successful reads and updates.
- `201 Created` for successful creates.
- `204 No Content` for successful deletes.
- `400 Bad Request` for validation errors.
- `401 Unauthorized` for missing or invalid authentication.
- `403 Forbidden` for insufficient role permissions.
- `404 Not Found` for missing resources or inaccessible scoped resources.
- `409 Conflict` for uniqueness conflicts.

## 9. Local Development

The project should provide:

- A Go/Gin API server.
- A Docker Compose file for Postgres.
- SQL migrations for schema setup.
- Environment-based configuration.
- Commands or scripts for:
  - starting Postgres
  - running migrations
  - cleaning expired or revoked refresh tokens
  - starting the API server
  - running tests

Required configuration:

| Variable | Description |
| --- | --- |
| `DATABASE_URL` | Postgres connection string. |
| `JWT_ACCESS_SECRET` | Secret for signing access tokens. |
| `JWT_REFRESH_SECRET` | Secret for signing or validating refresh tokens if applicable. |
| `ACCESS_TOKEN_TTL` | Access token lifetime. |
| `REFRESH_TOKEN_TTL` | Refresh token lifetime. |
| `PORT` | HTTP server port. |
| `AUTH_TOKEN_DELIVERY` | Auth verification/reset token delivery adapter. Use `log` for the local dev/test adapter. |
| `EMAIL_VERIFICATION_TTL` | Email verification token lifetime, default `30m`. |
| `PASSWORD_RESET_TTL` | Password reset token lifetime, default `30m`. |
| `OTP_RESEND_INTERVAL` | Minimum resend interval, default `60s`. |
| `OTP_MAX_ATTEMPTS` | Maximum wrong one-time-token attempts before lockout, default `5`. |
| `SIGNUP_CAPTCHA_REQUIRED` | Whether public signup requires a captcha token. |
| `SIGNUP_DISPOSABLE_DOMAINS` | Comma-separated disposable email denylist override for public signup. |
| `SMTP_HOST` | SMTP host used for quota approval/decline notifications when configured. |
| `SMTP_PORT` | SMTP port, default `587`. |
| `SMTP_USERNAME` | Optional SMTP username. |
| `SMTP_PASSWORD` | Optional SMTP password. |
| `SMTP_FROM` | SMTP sender address used with `SMTP_HOST`. |

V2 cross-service configuration:

| Variable | Description |
| --- | --- |
| `CROSS_SERVICE_BROKER` | Broker adapter. Supported values are `log` and `azure_eventhubs`. |
| `ACCOUNT_VIDEO_COMMANDS_STREAM` | Logical command stream, default `account.video.commands`. |
| `VIDEO_ACCOUNT_EVENTS_STREAM` | Logical event stream, default `video.account.events`. |
| `CROSS_SERVICE_CONSUMER_GROUP` | Consumer group for account-side event projection. |
| `CROSS_SERVICE_MAX_ATTEMPTS` | Retry limit before dead-letter. |
| `CROSS_SERVICE_POLL_INTERVAL` | Worker polling interval. |
| `AZURE_EVENTHUB_CONNECTION_STRING` | Azure Event Hubs connection string when using Azure. |
| `AZURE_EVENTHUB_CHECKPOINT_FILE` | Optional durable checkpoint file for the Azure inbox consumer. Defaults to `.state/azure_eventhubs/<stream>__<consumer-group>.json`. |

## 10. Testing Expectations

Tests should cover:

- User registration creates a user, organization, and owner membership.
- Login succeeds with valid credentials.
- Login fails with invalid credentials.
- Protected routes reject missing or invalid JWTs.
- Organization members can only access organizations they belong to.
- `owner` can manage members and devices.
- `admin` can manage devices but cannot manage members.
- `member` can read devices but cannot modify them.
- Cross-organization device access is rejected.
- Duplicate device `serial_number` in the same organization is rejected.
- Same `serial_number` may be used in different organizations.
- Device status can be updated by `owner` or `admin`.
- Last active organization `owner` cannot be removed, downgraded, or disabled.
- Self-service account deletion is refused while the current user is the last active `owner` of any organization.
- Owner can disable and enable member users.
- Admin and member cannot disable or enable users.
- Refresh token rotation rejects previously used refresh tokens.
- Logout revokes refresh tokens.
- Device CRUD tests cover create, list, get, update, status update, and soft-disable.
- Fleet registry tests cover group CRUD, group assignment idempotency, device tags, owner/admin/member permission differences, cross-organization rejection, and disabled-device selection visibility.
- Invalid device category and status values are rejected.
- Organization and member endpoints reject cross-organization access.
- SQL migrations are idempotent.
- SQL migrations protect organization `owner` invariants at the database layer.
- SQL migrations enforce normalized user email and non-blank organization/device names.
- SQL migrations maintain `updated_at` automatically for mutable tables.
- Disabled users cannot use existing access or refresh tokens.
- List endpoint tests cover pagination metadata.
- OpenAPI schema validation passes.
- Contract tests validate representative API responses against `openapi.yaml`.
- Provisioning creates operation and outbox records transactionally.
- Provisioning rejects disabled devices and cross-organization devices.
- Provisioning enforces `owner`/`admin` write permissions and `member` read-only permissions.
- Duplicate provisioning/deactivation `operation_id` is idempotent for the same payload and conflicts for a different payload.
- Outbox worker publish success, retry, and dead-letter behavior is covered.
- Inbox worker deduplicates by `message_id`.
- Inbox projection is idempotent by `operation_id`.
- Provisioning success/failure projections update video metadata without replacing unrelated metadata.
- `DeviceProvisionSucceeded` does not set account-manager `status=online`.
- `DeviceOnlineChanged` updates account-manager status and `last_seen_at`.
- Unknown message types and invalid schema versions are rejected or dead-lettered.
- The maintained test report maps v2 behavior groups to correctness assertions, not only coverage.

## 11. Acceptance Criteria

The v1 backend is acceptable when:

- The API server can run locally.
- Postgres can run locally through Docker Compose.
- Migrations create the required schema.
- OpenAPI YAML describes the implemented REST API.
- Auth, organization, member, and device APIs are implemented.
- Role-based authorization is enforced.
- Device records are scoped to organizations.
- Automated tests cover the core authorization and device-management scenarios.

The v2 provisioning/event-channel implementation is acceptable when:

- Account-side provisioning and deactivation APIs are documented in OpenAPI.
- Provisioning creates idempotent operation records.
- Account-side lifecycle commands are persisted in an outbox before publication.
- Outbox worker publishes `DeviceProvisionRequested` and `DeviceDeactivateRequested`.
- Inbox worker consumes all v2 video/account event types.
- Duplicate message delivery does not cause duplicate side effects.
- Projection merges metadata safely.
- Activation success does not imply account-manager `online`.
- Online/offline projection updates account-manager status only from `DeviceOnlineChanged`.
- Retry and dead-letter state are inspectable in the database.
- Local development can run without Azure using the `log` broker adapter.
- Automated tests cover the v2 behavior matrix and report correctness evidence.

## 12. Contract Follow-Up Scope

The account-manager implementation owns the account-side API, persistence, outbox, inbox, projection, broker adapter surfaces, Claim Token resolution, and registry-only readiness behavior. The broader product-level provisioning contract still depends on the external video-side lifecycle runtime and other service-owned readiness inputs.

Current external dependency:

- The video-side lifecycle integration worker lives in the separate `rtk_video_cloud` `cmd/crossservice` runtime. Account manager depends on that external service to consume `account.video.commands`, call Realtek video server `POST /activate_camera` and `POST /deactivate_camera`, and publish `video.account.events`. The previously tracked video-side worker hardening in `hkt999rtk/rtk_video_cloud#128`, `hkt999rtk/rtk_video_cloud#129`, `hkt999rtk/rtk_video_cloud#131`, and PR `hkt999rtk/rtk_video_cloud#146` is closed or merged as of this spec update.

Current verified behavior:

- Claim Token persistence and `POST /v1/orgs/:orgId/devices/claim/resolve` are implemented and covered by `openapi.yaml`, `docs/TESTING.md`, and `docs/TEST_REPORT.md`.
- Platform-admin Claim Token issuance/import/list/show/revoke is implemented and covered by OpenAPI and the maintained test report.
- Claim resolve error responses include machine-readable retryability hints.
- `GET /provisioning` returns registry-only readiness for an existing device with no provisioning operation.
- Failed or dead-lettered provisioning and deactivation readiness responses include `readiness.failure` attribution with layer, source state, retryability, error fields, operation id, and occurrence time.
- Platform-admin quota request list/show and audit-event list APIs are implemented.
- These behaviors were merged through PR #92, PR #93, PR #94, PR #104, PR #106, PR #107, PR #108, and their related documentation/test-report updates.
- The maintained test report includes Claim Token, registry-only readiness, failure-attribution, admin quota/audit, and OpenAPI correctness evidence.

Remaining post-v2 follow-up items:

- Implement the documented transfer, reclaim, and factory-reset policy for already-claimed devices.
- Retry and dead-letter rows are inspectable in Postgres, and `cmd/lifecycle-admin` exposes list, inspect, and safe requeue workflows for operators. A future operational visibility surface should summarize queue health, dead-letter counts, and latency without requiring direct SQL.
- Account registry soft-delete and product-level video deactivation remain separate. Product teardown requires explicit `POST /deactivate`; `DELETE /devices/:deviceId` only disables the account-side registry record.
- Account manager exposes an account-side readiness projection on `GET /provisioning`, but it still does not own a final cross-service "product ready" boolean. Any future unified readiness surface must compose account record, video activation, subject-bound token issuance, device info/config, and transport ownership across service boundaries.
