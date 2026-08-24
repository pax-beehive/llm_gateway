.PHONY: build test test-race vet run-dev compose-up compose-down

GOCACHE ?= /tmp/llm_gateway-go-cache

build:
	GOCACHE=$(GOCACHE) go build ./cmd/llm-gateway

test:
	GOCACHE=$(GOCACHE) go test ./...

test-race:
	GOCACHE=$(GOCACHE) go test -race ./...

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
