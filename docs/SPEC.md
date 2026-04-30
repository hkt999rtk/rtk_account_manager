# Account Manager Backend Specification

## 1. Product Goal

Build a backend account and device manager similar in spirit to Amazon IoT Device Manager. The system manages user accounts, organizations, and device registries for devices such as IP cameras, MQTT devices, and generic connected devices.

The v1 service is a REST API backend only. It stores account and device state in Postgres and provides authentication, organization membership, role-based authorization, and registry-only device management.

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
- OTP/email verification, self-service password recovery/reset, and third-party/social login.
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
- `PATCH /v1/me/password` lets the authenticated current user change their password after presenting the current password and a new password of at least 8 characters.
- Password change revokes all active refresh tokens for the user. Existing access tokens remain valid until their normal expiry.
- `DELETE /v1/me` lets the authenticated current user disable their own account. The operation revokes active refresh tokens and refuses to disable the user while they are the last active owner of any organization.
- Self-service account deletion is account-manager user lifecycle only. It preserves organization memberships and registry/device records, and it does not imply product-level device deletion or deactivation.
- OTP verification, self-service password recovery/reset, and third-party/social login are deferred first-phase lifecycle capabilities and must not be presented as available API behavior until implemented.
- Expired or revoked refresh tokens may be removed by an explicit maintenance command.

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
| `POST` | `/v1/auth/login` | No | Authenticate with email/password. |
| `POST` | `/v1/auth/refresh` | No | Exchange refresh token for new access token. |
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
| `GET` | `/v1/orgs/:orgId/members` | Yes | List organization members. |
| `POST` | `/v1/orgs/:orgId/members` | Yes | Add a user to the organization. |
| `PATCH` | `/v1/orgs/:orgId/members/:userId` | Yes | Update member role. |
| `PATCH` | `/v1/orgs/:orgId/members/:userId/disable` | Yes | Disable a member user account and revoke active refresh tokens. |
| `PATCH` | `/v1/orgs/:orgId/members/:userId/enable` | Yes | Re-enable a disabled member user account. |
| `DELETE` | `/v1/orgs/:orgId/members/:userId` | Yes | Remove member from organization. |

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

The current API does not create a device from raw claim material and does not
provide a standalone claim endpoint. A caller must first address an existing
enabled registry device by `organization_id` and account-manager `device_id`.
The registry device remains owned by that organization; submitted claim material
is stored as provisioning request payload and projected video metadata, not as a
replacement for account-manager ownership.

Raw claim-material endpoint decision:

- `rtk_account_manager` should not expose a broad
  `POST /v1/orgs/:orgId/devices/claim` endpoint until the product onboarding
  owner defines reusable claim lookup, transfer, reset, and already-claimed
  policy across device categories.
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
- A future raw claim endpoint must first resolve claim material to exactly one
  account-manager registry device or create an explicit pending claim record
  before it may start cloud provisioning.

Accepted claim material in the current API:

| Device category | Current account-manager claim material | Notes |
| --- | --- | --- |
| `ip_camera` | `video_cloud_devid`, `activity_id`, `clip_public_key` | Supported by `POST .../provision` for the account/video lifecycle flow. `video_cloud_devid` maps the account registry device to the Realtek video identity; `activity_id` and `clip_public_key` are passed to the video-side lifecycle worker. |
| `mqtt_device` | None beyond an existing registry device id | Category-specific MQTT claim material, broker credentials, and factory identity are out of scope for the current account-manager API. |
| `generic` | None beyond an existing registry device id | Serial-number, QR-code, activation-code, MAC-address, and future factory-identity claim flows are out of scope for the current account-manager API unless a later endpoint explicitly accepts them. |

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
- Accepting a provisioning request immediately merges pending `video_cloud_devid`, `video_cloud_activity_id`, and `video_cloud_activation_status=pending` into device metadata without implying activation success.
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

Current readiness inputs:

| Input | Source of truth | Meaning |
| --- | --- | --- |
| Account registry record exists and is enabled | `rtk_account_manager` device APIs | The organization-scoped device record exists and is not soft-disabled. |
| Provisioning operation accepted | `POST /provision`, `GET /provisioning` | Account side persisted the lifecycle operation and outbox command. |
| Video activation result projected | `GET /provisioning`, device `video_cloud_*` metadata | Realtek video activation succeeded or failed for the mapped device identity. |
| Subject-bound token issuance completed | Video-side or integration-service auth surface | The device or app has the credentials required for product use. |
| Video-side bootstrap prerequisites completed when required | Video-side APIs | Device info/config setup or equivalent downstream bootstrap state is available. |
| Owner transport connected | Account-manager device `status`, projected from `DeviceOnlineChanged` | The device has actually come online through its supported owner transport. |

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
  the required video-side credential and bootstrap signals.

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
- If token issuance or other video-side bootstrap fails after activation,
  surface that outside the account-manager readiness projection rather than
  rewriting the account-manager provisioning result.
- If the device never projects `online`, keep the readiness view in
  `transport_pending` or a more specific post-activation state instead of
  claiming full success.
- A future unified readiness endpoint that includes token/bootstrap signals
  would require a separate API/OpenAPI/test change set.

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

V2 cross-service configuration:

| Variable | Description |
| --- | --- |
| `CROSS_SERVICE_BROKER` | Broker adapter, such as `log`, `file`, or `azure_eventhubs`. |
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
- Local development can run without Azure using a local broker adapter.
- Automated tests cover the v2 behavior matrix and report correctness evidence.

## 12. Contract Follow-Up Scope

The account-manager implementation owns the account-side API, persistence, outbox, inbox, projection, and broker adapter surfaces. The broader product-level provisioning contract still depends on one external runtime plus a small set of follow-up surfaces outside the core account-manager API/store path.

Current external dependency:

- The video-side lifecycle integration worker now lives in the separate `rtk_video_cloud` `cmd/crossservice` runtime. Account manager depends on that external service to consume `account.video.commands`, call Realtek video server `POST /activate_camera` and `POST /deactivate_camera`, and publish `video.account.events`. The remaining worker acceptance hardening is tracked in `hkt999rtk/rtk_video_cloud#128`, `hkt999rtk/rtk_video_cloud#129`, `hkt999rtk/rtk_video_cloud#131`, and draft PR `hkt999rtk/rtk_video_cloud#146`, not in this repository.

Remaining follow-up items:

- Retry and dead-letter rows are inspectable in Postgres today, but an admin maintenance command should expose list, inspect, and safe requeue workflows for operators.
- Account registry soft-delete and product-level video deactivation remain separate. Product teardown requires explicit `POST /deactivate`; `DELETE /devices/:deviceId` only disables the account-side registry record.
- Account manager exposes an account-side readiness projection on `GET /provisioning`, but it still does not own a final cross-service "product ready" boolean. Any future unified readiness surface must compose account record, video activation, subject-bound token issuance, device info/config, and transport ownership across service boundaries.
