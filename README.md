# RTK Account Manager

Backend account and device manager for organization-scoped users and registry-only IoT devices.

## Local Development

1. Copy environment defaults:

   ```sh
   cp .env.example .env
   export $(grep -v '^#' .env | xargs)
   ```

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

5. Run tests:

   ```sh
   make test
   ```

The API listens on `http://localhost:8080` by default. The OpenAPI contract is in `openapi.yaml`.
