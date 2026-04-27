.PHONY: tidy test integration-test run db-up db-down migrate

tidy:
	go mod tidy

test:
	go test ./...

integration-test:
	TEST_DATABASE_URL='postgres://rtk:rtk_password@localhost:5432/rtk_account_manager?sslmode=disable' go test ./...

run:
	go run ./cmd/server

db-up:
	docker compose up -d postgres

db-down:
	docker compose down

migrate:
	go run ./cmd/migrate
