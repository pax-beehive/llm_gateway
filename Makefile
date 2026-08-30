.PHONY: build test test-race test-integration test-integration-local test-tenant-admin-roles test-openai-sdk-blackbox test-stage-a-blackbox test-tenant-admin-blackbox test-provider-connection-blackbox test-routing-catalog-blackbox test-operations-blackbox test-codex-sandbox-blackbox test-live-providers test-live-provider-tools integration-up integration-down vet run-dev run-control-plane-dev bootstrap-access repair-access-projection configure-tenant-admin-roles compose-up compose-down

GOCACHE ?= /tmp/llm_gateway-go-cache

build:
	GOCACHE=$(GOCACHE) go build ./cmd/...

test:
	GOCACHE=$(GOCACHE) go test ./...

test-race:
	GOCACHE=$(GOCACHE) go test -race ./...

test-integration:
	@if [ -z "$(TEST_DATABASE_URL)" ]; then \
		echo "TEST_DATABASE_URL is required; integration tests must not be skipped" >&2; \
		exit 1; \
	fi
	GOCACHE=$(GOCACHE) go test -count=1 -p=1 -tags=integration \
		./internal/access ./internal/store ./internal/configuration ./internal/cacheprotection \
		./internal/quota ./internal/httpapi ./internal/tenantadmin ./internal/credentialadmin \
		./internal/accessprojection ./internal/providerconnection ./internal/routingcatalog \
		./internal/operations ./cmd/llm-gateway
	$(MAKE) test-tenant-admin-roles TEST_DATABASE_URL="$(TEST_DATABASE_URL)"

integration-up:
	docker compose up -d --wait postgres

test-integration-local: integration-up
	TEST_DATABASE_URL='postgres://gateway:gateway-dev-only@127.0.0.1:55433/llm_gateway?sslmode=disable' $(MAKE) test-integration

test-tenant-admin-roles:
	@test -n "$(TEST_DATABASE_URL)" || { echo "TEST_DATABASE_URL is required" >&2; exit 1; }
	psql "$(TEST_DATABASE_URL)" -f tests/sql/tenant_admin_roles_test.sql

test-openai-sdk-blackbox:
	python3 tests/blackbox/openai_sdk.py

test-stage-a-blackbox:
	python3 tests/blackbox/stage_a.py

test-tenant-admin-blackbox:
	python3 tests/blackbox/tenant_admin.py

test-provider-connection-blackbox:
	python3 tests/blackbox/provider_connection.py

test-routing-catalog-blackbox:
	python3 tests/blackbox/routing_catalog.py

test-operations-blackbox:
	python3 tests/blackbox/operations.py

test-codex-sandbox-blackbox:
	bash tests/blackbox/codex_sandbox_multiturn.sh

test-live-providers:
	@test -f .env || { echo ".env is required; copy .env.example and fill the four provider keys" >&2; exit 1; }
	@set -a; . ./.env; set +a; GATEWAY_LIVE_TOOL_CONFORMANCE=false GOCACHE=$(GOCACHE) \
		go test -tags=live ./internal/provider/live -run '^TestLiveProviderTextStreaming$$' -count=1

test-live-provider-tools:
	@test -f .env || { echo ".env is required; copy .env.example and fill the four provider keys" >&2; exit 1; }
	@set -a; . ./.env; set +a; GATEWAY_LIVE_TOOL_CONFORMANCE=true GOCACHE=$(GOCACHE) \
		go test -v -tags=live ./internal/provider/live -run '^TestLiveProviderToolCalling$$' -count=1

integration-down:
	docker compose stop postgres

vet:
	GOCACHE=$(GOCACHE) go vet ./...

run-dev:
	GOCACHE=$(GOCACHE) GATEWAY_DEV_MEMORY_STORE=true GATEWAY_DEV_ECHO=true \
	GATEWAY_API_KEYS_JSON='{"dev-token":"tenant-dev","other-token":"tenant-other"}' \
	GATEWAY_TENANT_HOME_REGIONS_JSON='{"tenant-dev":"local","tenant-other":"local"}' \
	GATEWAY_DEV_ROUTE_TENANT_IDS_JSON='["tenant-dev"]' \
	go run ./cmd/llm-gateway

run-control-plane-dev:
	GOCACHE=$(GOCACHE) CONTROL_PLANE_DEV_MODE=true CONTROL_PLANE_MIGRATE=true \
	CONTROL_PLANE_DEV_TOKEN="$${CONTROL_PLANE_TOKEN:-local-control-admin-token}" \
	CONTROL_API_KEY_CURRENT_DIGEST_VERSION=1 \
	CONTROL_API_KEY_PEPPERS_JSON='{"1":"local-control-api-key-pepper"}' \
	CONTROL_PLANE_DATABASE_URL="$${CONTROL_PLANE_DATABASE_URL:-postgres://gateway:gateway-dev-only@127.0.0.1:55433/llm_gateway?sslmode=disable}" \
	go run ./cmd/control-plane

bootstrap-access:
	@test -n "$(GATEWAY_DATABASE_URL)" || { echo "GATEWAY_DATABASE_URL is required" >&2; exit 1; }
	GOCACHE=$(GOCACHE) go run ./cmd/access-bootstrap

repair-access-projection:
	@test -n "$(ACCESS_PROJECTION_REPAIR_TENANT_ID)" || { echo "ACCESS_PROJECTION_REPAIR_TENANT_ID is required" >&2; exit 1; }
	@test -n "$(CONTROL_PLANE_DATABASE_URL)" || { echo "CONTROL_PLANE_DATABASE_URL is required" >&2; exit 1; }
	@test -n "$(GATEWAY_DATABASE_URL)" || { echo "GATEWAY_DATABASE_URL is required" >&2; exit 1; }
	GOCACHE=$(GOCACHE) go run ./cmd/access-projection-repair

configure-tenant-admin-roles:
	@test -n "$(ADMIN_DATABASE_URL)" || { echo "ADMIN_DATABASE_URL is required" >&2; exit 1; }
	@test -n "$(TENANT_ADMIN_DB_ROLE)" || { echo "TENANT_ADMIN_DB_ROLE is required" >&2; exit 1; }
	@test -n "$(GATEWAY_DB_ROLE)" || { echo "GATEWAY_DB_ROLE is required" >&2; exit 1; }
	psql "$(ADMIN_DATABASE_URL)" \
		-v tenant_admin_role="$(TENANT_ADMIN_DB_ROLE)" \
		-v gateway_role="$(GATEWAY_DB_ROLE)" \
		-f scripts/postgres/configure-tenant-admin-roles.sql

compose-up:
	docker compose up --build

compose-down:
	docker compose down
