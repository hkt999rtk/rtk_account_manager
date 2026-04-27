.PHONY: tidy test run db-up db-down migrate

tidy:
	go mod tidy

test:
	go test ./...

run:
	go run ./cmd/server

db-up:
	docker compose up -d postgres

db-down:
	docker compose down

migrate:
	go run ./cmd/migrate
