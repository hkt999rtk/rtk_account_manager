# Local Provisioning And Worker Runbook

This runbook covers the current v2 local flow on `main` without Azure Event Hubs.

The local `log` broker adapter is intentionally simple:

- `cmd/outbox-worker` writes `account.video.commands` records to `stdout` as JSON lines.
- `cmd/inbox-worker` reads `video.account.events` records from `stdin` as JSON lines.
- No local process in this repo simulates the Realtek video server. For local end-to-end testing, you create API requests here, observe the outbox command, then inject a matching video-side event back into the inbox worker.

## Prerequisites

1. Copy the development environment file:

   ```sh
   cp .env.example .env
   ```

2. Use the local broker by keeping this setting in `.env`:

   ```sh
   CROSS_SERVICE_BROKER=log
   ```

3. Start Postgres and apply migrations:

   ```sh
   make db-up
   make migrate
   ```

## Environment Variables

| Variable | Used by | Notes |
| --- | --- | --- |
| `DATABASE_URL` | API, workers, migrations | Defaults to the local Docker Compose Postgres instance. |
| `JWT_ACCESS_SECRET` | API | Required for `make run`. |
| `JWT_REFRESH_SECRET` | API | Required for `make run`. |
| `PORT` | API | Defaults to `8080`. |
| `CROSS_SERVICE_BROKER` | workers | Use `log` locally; set `azure_eventhubs` only when validating Azure integration. |
| `ACCOUNT_VIDEO_COMMANDS_STREAM` | outbox worker | Defaults to `account.video.commands`. |
| `VIDEO_ACCOUNT_EVENTS_STREAM` | inbox worker | Defaults to `video.account.events`. |
| `CROSS_SERVICE_CONSUMER_GROUP` | inbox worker | Used only by the Azure adapter. |
| `CROSS_SERVICE_MAX_ATTEMPTS` | workers | Retry / dead-letter threshold. |
| `CROSS_SERVICE_POLL_INTERVAL` | workers | Poll interval and local retry delay. |
| `AZURE_EVENTHUB_CONNECTION_STRING` | workers | Leave empty for the local `log` adapter. |
| `AZURE_EVENTHUB_CHECKPOINT_FILE` | inbox worker | Optional Azure-only override for the durable checkpoint file. Defaults to `.state/azure_eventhubs/<stream>__<consumer-group>.json` so restarts resume after the last acknowledged sequence number. |

This runbook uses `CROSS_SERVICE_BROKER=log`, so no Azure checkpoint file is needed for the local FIFO-based flow. If you switch the inbox worker to `azure_eventhubs`, keep the default checkpoint path on persistent storage or set `AZURE_EVENTHUB_CHECKPOINT_FILE` explicitly before restarting workers.

## Start The Local Stack

Run the API and workers in separate terminals, then keep one extra shell open as the event writer.

Terminal 1:

```sh
make run
```

Terminal 2:

```sh
mkdir -p tmp
make run-outbox-worker | tee tmp/account-video-commands.log
```

Terminal 3:

```sh
mkdir -p tmp
[ -p tmp/video-account-events.pipe ] || mkfifo tmp/video-account-events.pipe
make run-inbox-worker < tmp/video-account-events.pipe
```

Terminal 4:

```sh
mkdir -p tmp
[ -p tmp/video-account-events.pipe ] || mkfifo tmp/video-account-events.pipe
exec 3> tmp/video-account-events.pipe
```

`tmp/account-video-commands.log` becomes the local command-stream audit log. The named pipe lets you push video-side events into the inbox worker on demand, but the writer must stay open for the whole session because the local `log` consumer exits after `stdin` reaches EOF. Every event injection below writes to file descriptor `3` from Terminal 4.

## Create A User, Organization, And Device

```sh
REGISTER_RESPONSE=$(curl -sS -X POST http://localhost:8080/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{
    "email": "owner@example.com",
    "password": "password123",
    "display_name": "Owner",
    "organization_name": "Owner Org"
  }')

ACCESS_TOKEN=$(echo "$REGISTER_RESPONSE" | jq -r '.tokens.access_token')
ORG_ID=$(echo "$REGISTER_RESPONSE" | jq -r '.organization.id')

DEVICE_RESPONSE=$(curl -sS -X POST "http://localhost:8080/v1/orgs/$ORG_ID/devices" \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "Lab Camera",
    "category": "ip_camera",
    "serial_number": "CAM-001",
    "metadata": {"location": "lab"}
  }')

DEVICE_ID=$(echo "$DEVICE_RESPONSE" | jq -r '.device.id')
```

## Run A Provisioning Flow

1. Create the provisioning operation:

   ```sh
   PROVISION_RESPONSE=$(curl -sS -X POST "http://localhost:8080/v1/orgs/$ORG_ID/devices/$DEVICE_ID/provision" \
     -H "Authorization: Bearer $ACCESS_TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{
       "video_cloud_devid": "video-device-1",
       "activity_id": "activity-1",
       "clip_public_key": "clip-public-key-1",
       "operation_id": "provision-op-1"
     }')

   PROVISION_OPERATION_ID=$(echo "$PROVISION_RESPONSE" | jq -r '.operation.operation_id')
   PROVISION_MESSAGE_ID=$(echo "$PROVISION_RESPONSE" | jq -r '.operation.message_id')
   ```

2. Confirm the outbox command was emitted:

   ```sh
   tail -n 1 tmp/account-video-commands.log | jq .
   ```

3. Inspect the persisted operation and outbox row:

   ```sh
   docker compose exec postgres psql -U rtk -d rtk_account_manager -c "
   SELECT operation_id, operation_type, status, retryable, error_code, created_at, completed_at
   FROM device_operations
   ORDER BY created_at DESC
   LIMIT 5;"

   docker compose exec postgres psql -U rtk -d rtk_account_manager -c "
   SELECT message_id, operation_id, status, attempt_count, last_error, available_at, published_at
   FROM device_message_outbox
   ORDER BY created_at DESC
   LIMIT 5;"
   ```

4. Confirm the accepted request already exposes pending video metadata:

   ```sh
   curl -sS "http://localhost:8080/v1/orgs/$ORG_ID/devices/$DEVICE_ID/provisioning" \
     -H "Authorization: Bearer $ACCESS_TOKEN" | jq .
   ```

   The `video_metadata` block should already show `video_cloud_devid`, `video_cloud_activity_id`, and `video_cloud_activation_status=pending` even before any video-side success or failure event arrives.

5. Inject a matching video-side success event into the inbox worker:

   ```sh
   cat <<EOF >&3
   {"stream":"video.account.events","envelope":{"message_id":"evt-provision-succeeded-1","correlation_id":"$PROVISION_OPERATION_ID","causation_id":"$PROVISION_MESSAGE_ID","operation_id":"$PROVISION_OPERATION_ID","source_service":"realtek_video_server","target_service":"rtk_account_manager","message_type":"DeviceProvisionSucceeded","schema_version":"1.0","partition_key":"$DEVICE_ID","occurred_at":"2026-04-29T10:00:00Z","payload":{"org_id":"$ORG_ID","account_device_id":"$DEVICE_ID","video_cloud_devid":"video-device-1","activity_id":"activity-1","activated_at":"2026-04-29T10:00:00Z"}}}
   EOF
   ```

6. Verify projected state:

   ```sh
   curl -sS "http://localhost:8080/v1/orgs/$ORG_ID/devices/$DEVICE_ID/provisioning" \
     -H "Authorization: Bearer $ACCESS_TOKEN" | jq .

   docker compose exec postgres psql -U rtk -d rtk_account_manager -c "
   SELECT message_id, operation_id, status, attempt_count, last_error, processed_at
   FROM device_message_inbox
   ORDER BY created_at DESC
   LIMIT 5;"

   docker compose exec postgres psql -U rtk -d rtk_account_manager -c "
   SELECT status, last_seen_at, metadata
   FROM devices
   WHERE id = '$DEVICE_ID';"
   ```

Provision success updates `video_cloud_*` metadata and the provisioning operation state, but it does not set the account-manager device `status` to `online`. `DeviceOnlineChanged` events own that status transition.

## Optional Online-State Projection

Inject an online event if you want to verify `status` and `last_seen_at` changes separately from provisioning:

```sh
cat <<EOF >&3
{"stream":"video.account.events","envelope":{"message_id":"evt-online-1","correlation_id":"$PROVISION_OPERATION_ID","operation_id":"$PROVISION_OPERATION_ID","source_service":"realtek_video_server","target_service":"rtk_account_manager","message_type":"DeviceOnlineChanged","schema_version":"1.0","partition_key":"$DEVICE_ID","occurred_at":"2026-04-29T10:05:00Z","payload":{"org_id":"$ORG_ID","account_device_id":"$DEVICE_ID","video_cloud_devid":"video-device-1","status":"online","last_seen_at":"2026-04-29T10:05:00Z"}}}
EOF
```

Re-run the device query above and confirm `status=online`.

## Run A Deactivation Flow

1. Create the deactivation operation:

   ```sh
   DEACTIVATE_RESPONSE=$(curl -sS -X POST "http://localhost:8080/v1/orgs/$ORG_ID/devices/$DEVICE_ID/deactivate" \
     -H "Authorization: Bearer $ACCESS_TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{
       "operation_id": "deactivate-op-1",
       "reason": "user_request"
     }')

   DEACTIVATE_OPERATION_ID=$(echo "$DEACTIVATE_RESPONSE" | jq -r '.operation.operation_id')
   DEACTIVATE_MESSAGE_ID=$(echo "$DEACTIVATE_RESPONSE" | jq -r '.operation.message_id')
   ```

2. Inject a matching success event:

   ```sh
   cat <<EOF >&3
   {"stream":"video.account.events","envelope":{"message_id":"evt-deactivate-succeeded-1","correlation_id":"$DEACTIVATE_OPERATION_ID","causation_id":"$DEACTIVATE_MESSAGE_ID","operation_id":"$DEACTIVATE_OPERATION_ID","source_service":"realtek_video_server","target_service":"rtk_account_manager","message_type":"DeviceDeactivateSucceeded","schema_version":"1.0","partition_key":"$DEVICE_ID","occurred_at":"2026-04-29T11:00:00Z","payload":{"org_id":"$ORG_ID","account_device_id":"$DEVICE_ID","video_cloud_devid":"video-device-1","deactivated_at":"2026-04-29T11:00:00Z"}}}
   EOF
   ```

3. Re-run the operation, outbox, inbox, and device inspection queries. You should now see deactivation metadata such as `video_cloud_activation_status=deactivated`.

## Inspect Retries And Dead Letters

Use these queries while debugging worker failures:

```sh
docker compose exec postgres psql -U rtk -d rtk_account_manager -c "
SELECT operation_id, status, retryable, error_code, error_message, updated_at
FROM device_operations
WHERE status IN ('retrying', 'dead_lettered', 'failed')
ORDER BY updated_at DESC;"

docker compose exec postgres psql -U rtk -d rtk_account_manager -c "
SELECT message_id, status, attempt_count, last_error, available_at, published_at
FROM device_message_outbox
WHERE status IN ('retrying', 'dead_lettered')
ORDER BY updated_at DESC;"

docker compose exec postgres psql -U rtk -d rtk_account_manager -c "
SELECT message_id, status, attempt_count, last_error, processed_at
FROM device_message_inbox
WHERE status IN ('retrying', 'dead_lettered')
ORDER BY updated_at DESC;"
```

To force a local inbox dead-letter row, inject a structurally valid event with an invalid `DeviceOnlineChanged.status` value:

```sh
cat <<EOF >&3
{"stream":"video.account.events","envelope":{"message_id":"evt-bad-online-1","correlation_id":"$PROVISION_OPERATION_ID","operation_id":"$PROVISION_OPERATION_ID","source_service":"realtek_video_server","target_service":"rtk_account_manager","message_type":"DeviceOnlineChanged","schema_version":"1.0","partition_key":"$DEVICE_ID","occurred_at":"2026-04-29T12:00:00Z","payload":{"org_id":"$ORG_ID","account_device_id":"$DEVICE_ID","video_cloud_devid":"video-device-1","status":"sleeping","last_seen_at":"2026-04-29T12:00:00Z"}}}
EOF
```

That record persists an inbox row first, then dead-letters during payload validation with `last_error` explaining that `payload.status` must be `online` or `offline`.

The local `log` publisher itself is deterministic and does not manufacture transient broker failures. Retry paths are primarily exercised by automated tests, but the queries above are the same ones to use when a real worker failure leaves rows in `retrying` or `dead_lettered`.

## Delete Versus Deactivate

- `POST /v1/orgs/:orgId/devices/:deviceId/deactivate` is the product-side teardown path. It creates a lifecycle operation and emits `DeviceDeactivateRequested` for the downstream video service.
- `DELETE /v1/orgs/:orgId/devices/:deviceId` is the account-manager registry delete path. It disables the account-side device record; it does not send a cross-service deactivation command by itself.

If the product-side device should be torn down, run deactivation first and confirm the matching video-side result event. Use the registry delete endpoint only for account-manager record lifecycle.

## Shutdown

Stop the API and workers with `Ctrl-C`, then remove local services:

```sh
exec 3>&-
make db-down
rm -f tmp/video-account-events.pipe
```
