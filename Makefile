REPORT_DIR ?= reports
VERSION ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo dev)
PYTHON ?= python3
ADMIN_REPO ?= ../rtk_cloud_admin
UNIT_TEST_PACKAGES := ./internal/api ./internal/auth ./internal/broker ./internal/channel ./internal/config ./internal/database ./internal/emaildelivery ./internal/openapi ./internal/readiness ./internal/store ./internal/worker/clouddeletion ./internal/worker/emailoutbox ./internal/worker/inbox ./internal/worker/outbox
RACE_TEST_PACKAGES := ./internal/channel ./internal/broker ./internal/worker/... ./internal/auth ./internal/config ./internal/readiness
FUZZ_SMOKE_TIME ?= 2s

.PHONY: tidy test test-email-signup-helper integration-test test-report check-report-candidates test-race test-repeat fuzz-smoke build release check-release readiness-smoke run run-outbox-worker run-inbox-worker run-email-worker db-up db-down migrate cleanup-tokens

tidy:
	go mod tidy

test:
	go test ./...
	$(PYTHON) ./scripts/email_signup_imap_test.py

test-email-signup-helper:
	$(PYTHON) ./scripts/email_signup_imap_test.py

integration-test:
	TEST_DATABASE_URL='postgres://rtk:rtk_password@localhost:5432/rtk_account_manager?sslmode=disable' go test ./...

test-report:
	./scripts/test-report.sh

check-report-candidates:
	./scripts/report-candidate-tests.sh

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

build:
	@mkdir -p dist
	go build -trimpath -o dist/rtk-account-manager ./cmd/server
	go build -trimpath -o dist/rtk-account-manager-migrate ./cmd/migrate
	go build -trimpath -o dist/rtk-account-manager-outbox-worker ./cmd/outbox-worker
	go build -trimpath -o dist/rtk-account-manager-inbox-worker ./cmd/inbox-worker
	go build -trimpath -o dist/rtk-account-manager-email-worker ./cmd/email-worker
	go build -trimpath -o dist/rtk-account-manager-email-outbox-admin ./cmd/email-outbox-admin
	go build -trimpath -o dist/rtk-account-manager-cleanup-tokens ./cmd/cleanup-tokens
	go build -trimpath -o dist/rtk-account-manager-cloud-deletion-worker ./cmd/cloud-deletion-worker

release:
	@rm -rf "dist/rtk_account_manager-$(VERSION)" "dist/rtk_account_manager-$(VERSION).tar.gz"
	@mkdir -p "dist/rtk_account_manager-$(VERSION)/bin" "dist/rtk_account_manager-$(VERSION)/deploy"
	go build -trimpath -o "dist/rtk_account_manager-$(VERSION)/bin/rtk-account-manager" ./cmd/server
	go build -trimpath -o "dist/rtk_account_manager-$(VERSION)/bin/rtk-account-manager-migrate" ./cmd/migrate
	go build -trimpath -o "dist/rtk_account_manager-$(VERSION)/bin/rtk-account-manager-outbox-worker" ./cmd/outbox-worker
	go build -trimpath -o "dist/rtk_account_manager-$(VERSION)/bin/rtk-account-manager-inbox-worker" ./cmd/inbox-worker
	go build -trimpath -o "dist/rtk_account_manager-$(VERSION)/bin/rtk-account-manager-email-worker" ./cmd/email-worker
	go build -trimpath -o "dist/rtk_account_manager-$(VERSION)/bin/rtk-account-manager-email-outbox-admin" ./cmd/email-outbox-admin
	go build -trimpath -o "dist/rtk_account_manager-$(VERSION)/bin/rtk-account-manager-cleanup-tokens" ./cmd/cleanup-tokens
	go build -trimpath -o "dist/rtk_account_manager-$(VERSION)/bin/rtk-account-manager-cloud-deletion-worker" ./cmd/cloud-deletion-worker
	cp -R migrations "dist/rtk_account_manager-$(VERSION)/migrations"
	cp -R deploy/systemd "dist/rtk_account_manager-$(VERSION)/deploy/systemd"
	cp deploy/account-manager.env.example "dist/rtk_account_manager-$(VERSION)/deploy/account-manager.env.example"
	cp deploy/install.sh deploy/verify.sh "dist/rtk_account_manager-$(VERSION)/deploy/"
	find "dist/rtk_account_manager-$(VERSION)" -name '._*' -delete
	{ \
		echo "version=$(VERSION)"; \
		echo "git_sha=$$(git rev-parse HEAD 2>/dev/null || echo unknown)"; \
		echo "built_at=$$(date -u +%Y-%m-%dT%H:%M:%SZ)"; \
		echo "module=$$(go list -m)"; \
	} > "dist/rtk_account_manager-$(VERSION)/release-manifest.txt"
	./deploy/check-release.sh "dist/rtk_account_manager-$(VERSION)"
	COPYFILE_DISABLE=1 tar -C dist -czf "dist/rtk_account_manager-$(VERSION).tar.gz" "rtk_account_manager-$(VERSION)"

check-release: release

readiness-smoke:
	go run ./cmd/readiness-smoke

run:
	go run ./cmd/server

run-outbox-worker:
	go run ./cmd/outbox-worker

run-inbox-worker:
	go run ./cmd/inbox-worker

run-email-worker:
	go run ./cmd/email-worker

db-up:
	docker compose up -d postgres

db-down:
	docker compose down

migrate:
	go run ./cmd/migrate

cleanup-tokens:
	go run ./cmd/cleanup-tokens
