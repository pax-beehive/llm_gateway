# Universal LLM Gateway

A Responses-first, multi-tenant Go gateway implementing [ADR 0001](docs/adr/0001-build-universal-llm-gateway-core.md). It exposes OpenAI-compatible Responses and Chat Completions APIs, normalizes provider events behind capability-aware Model Routes, and persists lifecycle state with tenant-scoped CAS revisions.

## Run locally

The smallest local mode uses the deterministic echo Provider and an in-memory Response Store:

```sh
make run-dev
```

Create a Response:

```sh
curl -sS http://localhost:8080/v1/responses \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"model":"echo-v1","input":"hello gateway"}'
```

Stream Chat Completions:

```sh
curl -N http://localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"model":"echo-v1","stream":true,"messages":[{"role":"user","content":"hello stream"}]}'
```

Use `docker compose up --build` for the PostgreSQL-backed development stack. The Compose credentials and bearer token are development-only.

## Production configuration

Production requires PostgreSQL and refuses to start without an explicit synchronous multi-AZ durability attestation:

```text
DATABASE_URL=postgres://...
GATEWAY_ENV=production
GATEWAY_DURABILITY_ATTESTATION=sync-multi-az
GATEWAY_API_KEYS_JSON={"secret-token":"tenant-id"}
GATEWAY_TENANT_HOME_REGIONS_JSON={"tenant-id":"us-west"}
GATEWAY_ROUTES_JSON=[...]
```

Each route declares its public model, provider model, execution/home region, credential scope, versioned Capability Profile, prices, and secret environment-variable reference. Credentials are read server-side and are never returned by the API.

With PostgreSQL, `model_routes` is loaded from immutable `configuration_history` revisions and projected into an atomic in-process snapshot. Set `GATEWAY_BOOTSTRAP_ROUTES=true` only for the first revision. Later publications require `GATEWAY_PUBLISH_ROUTES=true`, explicit expected/new revisions, and an actor; stale CAS publications fail without replacing the live snapshot.

Example route:

```json
[
  {
    "id": "provider-us-primary",
    "provider": "provider-name",
    "public_model": "gateway-model",
    "provider_model": "provider-model-id",
    "base_url": "https://provider.example/v1",
    "api_key_env": "PROVIDER_API_KEY",
    "region": "us-west",
    "home_region": "us-west",
    "credential_scope": "tenant-primary",
    "capability_revision": 1,
    "capabilities": {
      "text": "native",
      "streaming": "native",
      "sampling": "native",
      "tools": "translated"
    },
    "price_snapshot_id": "provider-model-usd-2026-08-24",
    "price_effective_at": "2026-08-24T00:00:00Z",
    "price_source": "provider-price-contract-2026-08-24",
    "currency": "USD",
    "input_cost_per_million": 1.0,
    "cached_input_cost_per_million": 0.1,
    "cache_write_cost_per_million": 1.25,
    "cache_usage_reliable": true,
    "output_cost_per_million": 4.0
  }
]
```

Direct Anthropic routes use the native Messages API rather than the generic OpenAI-compatible adapter. To enable prompt caching and proactive Cache Protection, attach a conformance-gated refresh policy to that same route and credential:

```json
{
  "id": "anthropic-us-primary",
  "provider": "anthropic",
  "public_model": "claude",
  "provider_model": "claude-model-id",
  "base_url": "https://api.anthropic.com/v1",
  "api_key_env": "ANTHROPIC_API_KEY",
  "region": "us-west",
  "home_region": "us-west",
  "credential_scope": "tenant-primary",
  "input_cost_per_million": 1.0,
  "cached_input_cost_per_million": 0.1,
  "cache_write_cost_per_million": 1.25,
  "output_cost_per_million": 4.0,
  "price_snapshot_id": "anthropic-model-usd-2026-08-24",
  "price_effective_at": "2026-08-24T00:00:00Z",
  "price_source": "provider-price-contract-2026-08-24",
  "currency": "USD",
  "cache_usage_reliable": true,
  "capabilities": {"text":"native","streaming":"native","tools":"translated","sampling":"translated"},
  "cache_refresh": {"kind":"anthropic","ttl_seconds":300,"write_cost_per_million":1.25}
}
```

The direct adapter uses one serializer for live execution, Cache Anchors, and zero-output refreshes. It protects only stable system/tool prefixes and creates a Cache Lease only after Anthropic reports cache creation or reuse. Gemini TTL update support exists behind the provider seam, but proactive Gemini/OpenAI route configuration is deliberately rejected until an execution/anchor conformance adapter is enabled; DeepSeek remains observation-only.

Strict compatibility is the default. A feature classified as `translated` is considered only when the request sets `compatibility_mode` to `best_effort`; `unsupported` or undeclared features are rejected instead of silently dropped.

## Verification

```sh
make test
make test-race
make vet
```

The core migration is embedded in the binary and runs only when `GATEWAY_MIGRATE=true`. Production schema rollout should normally happen as a separate deployment gate.

## Stateful operations

Responses can continue a branch with `previous_response_id`, or use a mutable Conversation with `conversation`; these modes are intentionally mutually exclusive. Conversation create/get/delete and CAS item append are available under `/v1/conversations`. A Response transaction appends its new input while acquiring the Conversation, then appends terminal output and releases it during the same transaction that records final usage.

Every successful execution records the normalized token counts, original provider usage payload, immutable price snapshot, calculated amount, observed cache-discount evidence, and transactional outbox events. Provider cache discounts are not labeled as Cache Protection savings unless separate Protected Hit evidence exists.

Responses accepts typed input plus `instructions`, client function `tools`, `tool_choice`, sampling controls, and streaming/background lifecycle controls. Chat Completions preserves assistant tool-call history and returns tool calls in both streaming and non-streaming forms. Unsupported fields and undeclared Capability Profile behavior are rejected rather than discarded.

Cache refresh intents are durable PostgreSQL work, not process-local timers. Due work is claimed with `FOR UPDATE SKIP LOCKED`; lease revision and fencing tokens are checked on every transition, successful refreshes advance both values, and ambiguous outcomes become terminal `uncertain` intents rather than automatic retries.

Requests opt in with a bounded `cache_protection` object (`enabled`, `max_spend_micros`, `max_refreshes`, `max_protection_window_seconds`, and `safety_margin_micros`). Missing or excessive bounds are rejected. The worker re-evaluates expiry and ROI when work is claimed, so delayed work is not refreshed after its lease expires.

## OpenTelemetry

Set `OTEL_EXPORTER_OTLP_ENDPOINT` (or the signal-specific traces/metrics endpoint variables) to export W3C-propagated HTTP and provider traces plus Response, token, cost, provider-duration, and cache-outcome metrics over OTLP/HTTP. With no endpoint, instrumentation is a no-op. `OTEL_SERVICE_NAME` defaults to `llm-gateway`.
