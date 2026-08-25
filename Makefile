.PHONY: build test test-race test-integration test-integration-local test-live-providers integration-up integration-down vet run-dev compose-up compose-down

GOCACHE ?= /tmp/llm_gateway-go-cache

build:
	GOCACHE=$(GOCACHE) go build ./cmd/llm-gateway

test:
	GOCACHE=$(GOCACHE) go test ./...

test-race:
	GOCACHE=$(GOCACHE) go test -race ./...

test-integration:
	@if [ -z "$(TEST_DATABASE_URL)" ]; then \
		echo "TEST_DATABASE_URL is required; integration tests must not be skipped" >&2; \
		exit 1; \
	fi
	GOCACHE=$(GOCACHE) go test -count=1 -p=1 -tags=integration ./internal/store ./internal/configuration ./internal/cacheprotection

integration-up:
	docker compose up -d --wait postgres

test-integration-local: integration-up
	TEST_DATABASE_URL='postgres://gateway:gateway-dev-only@127.0.0.1:5432/llm_gateway?sslmode=disable' $(MAKE) test-integration

test-live-providers:
	@test -f .env || { echo ".env is required; copy .env.example and fill the provider keys and models" >&2; exit 1; }
	@set -a; . ./.env; set +a; GOCACHE=$(GOCACHE) go test -tags=live ./internal/provider/live -count=1

integration-down:
	docker compose stop postgres

vet:
	GOCACHE=$(GOCACHE) go vet ./...

run-dev:
	GOCACHE=$(GOCACHE) GATEWAY_DEV_MEMORY_STORE=true GATEWAY_DEV_ECHO=true \
	GATEWAY_API_KEYS_JSON='{"dev-token":"tenant-dev"}' \
	GATEWAY_TENANT_HOME_REGIONS_JSON='{"tenant-dev":"local"}' \
	go run ./cmd/llm-gateway

compose-up:
	docker compose up --build

compose-down:
	docker compose down
