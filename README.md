# RTK Account Manager

Backend account and device manager for organization-scoped users and registry-only IoT devices.

## Local Development

1. Copy environment defaults:

   ```sh
   cp .env.example .env
   ```

   The server and maintenance commands load `.env` automatically when it is present.

2. Start Postgres:

   ```sh
   make db-up
   ```

3. Run migrations:

   ```sh
   make migrate
   ```

4. Start the API:

   ```sh
   make run
   ```

5. Clean expired or revoked refresh tokens when needed:

   ```sh
   make cleanup-tokens
   ```

6. Run tests:

   ```sh
   make test
   ```

7. Run Postgres-backed integration tests:

   ```sh
   make integration-test
   ```

   These tests require the Docker Compose Postgres service to be running.

8. Stop local services:

   ```sh
   make db-down
   ```

The API listens on `http://localhost:8080` by default. The OpenAPI contract is in `openapi.yaml`.
List endpoints accept `limit` and `offset` query parameters and return pagination metadata.

## Smoke Test

After starting Postgres and the API, create a user, organization, and device:

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

curl -sS -X POST "http://localhost:8080/v1/orgs/$ORG_ID/devices" \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "Lab Camera",
    "category": "ip_camera",
    "serial_number": "CAM-001",
    "metadata": {"location": "lab"}
  }'

curl -sS "http://localhost:8080/v1/orgs/$ORG_ID/devices" \
  -H "Authorization: Bearer $ACCESS_TOKEN"
```

Run migrations manually with explicit environment variables:

```sh
DATABASE_URL='postgres://rtk:rtk_password@localhost:5432/rtk_account_manager?sslmode=disable' \
JWT_ACCESS_SECRET='dev-access-secret' \
JWT_REFRESH_SECRET='dev-refresh-secret' \
go run ./cmd/migrate
```
