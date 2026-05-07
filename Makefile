REPORT_DIR ?= reports
UNIT_TEST_PACKAGES := ./internal/api ./internal/auth ./internal/broker ./internal/channel ./internal/config ./internal/database ./internal/openapi ./internal/readiness ./internal/store ./internal/worker/inbox ./internal/worker/outbox
RACE_TEST_PACKAGES := ./internal/channel ./internal/broker ./internal/worker/... ./internal/auth ./internal/config ./internal/readiness
FUZZ_SMOKE_TIME ?= 2s

.PHONY: tidy test integration-test test-report test-race test-repeat fuzz-smoke readiness-smoke run run-outbox-worker run-inbox-worker db-up db-down migrate cleanup-tokens

tidy:
	go mod tidy

test:
	go test ./...

integration-test:
	TEST_DATABASE_URL='postgres://rtk:rtk_password@localhost:5432/rtk_account_manager?sslmode=disable' go test ./...

test-report:
	./scripts/test-report.sh

test-race:
	@mkdir -p $(REPORT_DIR)
	TEST_DATABASE_URL= go test -race $(RACE_TEST_PACKAGES) | tee $(REPORT_DIR)/test-race.txt

test-repeat:
	@mkdir -p $(REPORT_DIR)
	TEST_DATABASE_URL= go test -shuffle=on -count=3 $(UNIT_TEST_PACKAGES) | tee $(REPORT_DIR)/test-repeat.txt

fuzz-smoke:
	@mkdir -p $(REPORT_DIR)
	TEST_DATABASE_URL= go test ./internal/channel -run=^$$ -fuzz=FuzzEnvelopeStrictJSONAndValidation -fuzztime=$(FUZZ_SMOKE_TIME) | tee $(REPORT_DIR)/fuzz-smoke-channel.txt
	TEST_DATABASE_URL= go test ./internal/api -run=^$$ -fuzz=FuzzBindStrictRequestShape -fuzztime=$(FUZZ_SMOKE_TIME) | tee $(REPORT_DIR)/fuzz-smoke-api.txt

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
