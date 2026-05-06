.PHONY: tidy test integration-test test-report readiness-smoke run run-outbox-worker run-inbox-worker db-up db-down migrate cleanup-tokens

tidy:
	go mod tidy

test:
	go test ./...

integration-test:
	TEST_DATABASE_URL='postgres://rtk:rtk_password@localhost:5432/rtk_account_manager?sslmode=disable' go test ./...

test-report:
	./scripts/test-report.sh

readiness-smoke:
	go run ./cmd/readiness-smoke

run:
	go run ./cmd/server

run-outbox-worker:
	go run ./cmd/outbox-worker

run-inbox-worker:
	go run ./cmd/inbox-worker

db-up:
	docker compose up -d postgres

db-down:
	docker compose down

migrate:
	go run ./cmd/migrate

cleanup-tokens:
	go run ./cmd/cleanup-tokens
