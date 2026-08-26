# Universal LLM Gateway

A Responses-first, multi-tenant Go gateway implementing [ADR 0001](docs/adr/0001-build-universal-llm-gateway-core.md). It exposes OpenAI-compatible Responses and Chat Completions APIs plus the Stage A capability interfaces from [ADR 0009](docs/adr/0009-expand-capability-specific-inference-interfaces.md): embeddings, moderations, and reranking. Provider behavior stays behind capability-aware Model Routes, and lifecycle state uses tenant-scoped CAS revisions. The first release is intentionally limited to OpenAI, DeepSeek, Anthropic (Claude), and Gemini.

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

List the authenticated Tenant's currently routable public models:

```sh
curl -sS http://localhost:8080/v1/models \
  -H 'Authorization: Bearer dev-token'
```

The deterministic development route also supports the Stage A interfaces:

```sh
curl -sS http://localhost:8080/v1/embeddings \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"model":"echo-v1","input":["first","second"],"dimensions":8}'

curl -sS http://localhost:8080/v1/moderations \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"model":"echo-v1","input":"ordinary text"}'

curl -sS http://localhost:8080/v1/rerank \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"model":"echo-v1","query":"red apple","documents":["ocean","red apple tree"],"top_n":1}'

curl -sS http://localhost:8080/v1/capabilities \
  -H 'Authorization: Bearer dev-token'
```

The returned model IDs are the same public IDs accepted by each request's `model` field. The catalog exposes healthy, Home Region-compatible Model Routes with native text capability; it does not expose internal Route IDs or raw Provider model IDs. Each OpenAI-compatible model object's `created` timestamp is the durable publication time of the active Model Route catalog revision.

Use `docker compose up --build` for the PostgreSQL-backed development stack. Its development-only bearer token is `dev-token-llm-gateway-local`; `make run-dev` continues to use the shorter in-memory `dev-token`.

## Production configuration

Production requires PostgreSQL and refuses to start without an explicit synchronous multi-AZ durability attestation:

```text
DATABASE_URL=postgres://...
GATEWAY_ENV=production
GATEWAY_DURABILITY_ATTESTATION=sync-multi-az
GATEWAY_API_KEY_PEPPER=<stable-secret-at-least-16-bytes>
GATEWAY_LOCAL_REGION=us-west
GATEWAY_HOME_REGION_URLS_JSON={"us-west":"https://us-west.gateway.example","eu-west":"https://eu-west.gateway.example"}
GATEWAY_CACHE_PROTECTION_MODE=off
GATEWAY_ROUTES_JSON=[...]
```

In PostgreSQL mode, `tenants`, `tenant_policy_revisions`, `api_keys`, and `api_key_policy_revisions` are authoritative. Requests authenticate by a peppered HMAC digest; the raw Gateway API Key is never persisted, and a caller-supplied Tenant header cannot change the authenticated Tenant. Keep `GATEWAY_API_KEY_PEPPER` stable and secret. Rotating it currently requires reissuing or explicitly reimporting keys.

Environment key maps are only for an explicit, idempotent first bootstrap. A raw bootstrap key must contain at least 24 characters. After a successful bootstrap, remove the raw-key variables and disable the flag; subsequent replicas load access state only from PostgreSQL:

```text
GATEWAY_BOOTSTRAP_ACCESS=true
GATEWAY_API_KEYS_JSON={"long-secret-gateway-token":"tenant-id"}
GATEWAY_TENANT_HOME_REGIONS_JSON={"tenant-id":"us-west"}
GATEWAY_TENANT_EXECUTION_EPOCHS_JSON={"tenant-id":1}
GATEWAY_TENANT_POLICIES_JSON={"tenant-id":{"revision":1,"max_concurrent_responses":32,"max_input_items":256,"retention_seconds":2592000,"allow_stored_responses":true,"allow_cache_protection":false,"allow_content_inspection":false,"limits":{"requests_per_minute":1000,"tokens_per_minute":5000000,"monthly_spend_micros":500000000,"refresh_monthly_spend_micros":0,"currency":"USD"}}}
GATEWAY_API_KEY_POLICIES_JSON={"long-secret-gateway-token":{"revision":1,"allow_cache_protection":false,"limits":{"requests_per_minute":100,"monthly_spend_micros":50000000,"refresh_monthly_spend_micros":0,"currency":"USD"}}}
GATEWAY_API_KEY_METADATA_JSON={"long-secret-gateway-token":{"environment":"production","owner":"platform"}}
```

Tenant and key policy publication uses compare-and-swap revisions and immutable policy history. Key metadata uses a separate row revision. An active key can be revoked, a Tenant can be suspended and reactivated, and a closed Tenant cannot be reopened. Authentication and delayed refresh work read current state, so these changes do not wait for a process restart. `null`/absent limits inherit from the other scope; explicit `0` denies. A Gateway API Key can only narrow a Tenant limit or permission.

Each route declares its public model, provider model, execution/home region, credential scope, versioned Capability Profile, prices, and secret environment-variable reference. Credentials are read server-side and are never returned by the API. The `provider` value must be one of `openai`, `deepseek`, `anthropic`, or `gemini`; route publication fails for any other value.

OpenAI uses its native Responses API so Codex fields, reasoning replay, namespace tools, and client-owned tool loops remain lossless. DeepSeek and Gemini use their OpenAI-compatible Chat Completions surfaces behind the shared adapter. Anthropic uses its native Messages API so Claude-specific streaming, usage, and prompt-cache evidence remain explicit.

With PostgreSQL, `model_routes` is loaded from immutable `configuration_history` revisions and projected into an atomic in-process snapshot. Set `GATEWAY_BOOTSTRAP_ROUTES=true` only for the first revision. Later publications require `GATEWAY_PUBLISH_ROUTES=true`, explicit expected/new revisions, and an actor; stale CAS publications fail without replacing the live snapshot.

Example route:

```json
[
  {
    "id": "openai-us-primary",
    "provider": "openai",
    "public_model": "gateway-model",
    "provider_model": "provider-model-id",
    "base_url": "https://api.openai.com/v1",
    "api_key_env": "OPENAI_API_KEY",
    "region": "us-west",
    "home_region": "us-west",
    "tenant_ids": ["tenant-a"],
    "credential_scope": "tenant-primary",
    "healthy": true,
    "capability_revision": 1,
    "capabilities": {
      "text": "native",
      "streaming": "native",
      "sampling": "native",
      "tools": "translated",
      "embeddings": "native",
      "moderation": "native",
      "rerank": "translated"
    },
    "embedding_path": "/embeddings",
    "moderation_path": "/moderations",
    "rerank_path": "/rerank",
    "embedding_dimensions": 1536,
    "embedding_input_cost_per_million": 0.02,
    "moderation_input_cost_per_million": 0,
    "rerank_document_cost_per_thousand": 0.5,
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

Stage A uses narrow `EmbeddingExecutor`, `ModerationExecutor`, and `RerankExecutor` seams. Route configuration never infers one capability from another: `/v1/capabilities` publishes only healthy, Home Region-compatible declarations usable by the authenticated Tenant. `tenant_ids` is an optional route allowlist; omitting it makes the route available to every Tenant in that Home Region. The production HTTP adapter uses the configured Provider model and paths while returning the public model ID, with a bounded upstream request timeout. Offline conformance tests cover bearer/header forwarding, wire shapes, normalized usage, timeout/error classification, and content-free evidence.

Capability requests reserve RPM and spend before the Provider call. Limit Policies can additionally set `embedding_input_units`, `rerank_documents`, and `capability_spend_micros`; Tenant/key intersection remains most restrictive. `capability_usage_ledger` is an immutable, content-free financial ledger with a separate daily projection. It stores counts, dimensions, document totals, raw Provider usage, price snapshot identity, and spend—but never vectors, moderation inputs/results, rerank queries, or documents. Ambiguous Provider outcomes keep their reservation `uncertain`.

Tenant policies bound concurrent Responses and input-item counts, set an explicit content-retention period, and can independently allow stored Responses, Cache Protection, and content inspection. Tenant and Gateway API Key Limit Policies can additionally cap input/output tokens, per-request cost, requests and tokens per minute, daily/monthly Response spend, and daily/monthly refresh spend. PostgreSQL reserves these hard limits before Provider execution under Tenant-then-key locks, then settles them to actual usage. A crashed or ambiguous execution keeps its conservative reservation as `uncertain`; a reconciliation worker settles durable usage facts and never releases an externally ambiguous spend estimate. Stateful writes received outside the Home Region are forwarded to its configured gateway URL, or rejected with HTTP 421 when that route is unavailable. Model Routes can be drained with `"healthy": false` without deleting their versioned configuration.

## Verification

```sh
make test
make test-race
make vet
make build
make test-integration-local
```

With `make run-dev` running in another terminal, exercise the public HTTP contract through the official OpenAI Python SDK:

```sh
make test-openai-sdk-blackbox
```

Exercise the Stage A capability contract with the dependency-free HTTP black box:

```sh
make test-stage-a-blackbox
```

It covers the authenticated capability catalog, embeddings, moderations, reranking, and deterministic result shapes without Provider spend.

The black-box suite covers model discovery, Responses create/retrieve/input-items/delete, `previous_response_id`, Responses streaming, Chat Completions, Chat streaming, and Conversations. Override `GATEWAY_BASE_URL`, `GATEWAY_API_KEY`, and `GATEWAY_MODEL` to target another deployment. It requires the `openai` Python package and defaults to the zero-cost local `echo-v1` route.

Run the deterministic Codex CLI black box separately:

```sh
make test-codex-sandbox-blackbox
```

It starts a local native-Responses mock and gateway, then runs one Codex `exec` plus two `exec resume` turns under `workspace-write`. The test proves model discovery, Responses SSE lifecycle, tool-call/result round trips, sandboxed file writes, and cross-turn state without spending Provider tokens. It requires `codex`, `jq`, and Python 3.

`make test-integration` requires `TEST_DATABASE_URL` and fails when it is absent, so PostgreSQL coverage cannot silently pass by skipping every integration test. `make test-integration-local` starts the Compose PostgreSQL service and supplies its development-only connection string. CI runs the same unit, race, vet, build, and PostgreSQL gates.

Provider adapters have offline conformance tests that use fake credentials and local HTTP transports; they do not call Provider APIs or incur model charges:

| Provider | Execution surface | Offline contract covered | Proactive refresh |
| --- | --- | --- | --- |
| OpenAI | native Responses | bearer auth, Codex request fields, SSE text/reasoning/tools/usage | off |
| DeepSeek | OpenAI-compatible Chat Completions | `max_tokens`, `user_id`, SSE text/tools/usage | off |
| Anthropic | native Messages | `x-api-key`, API version, request fields, SSE text/usage, no cache breakpoint while disabled | off by default |
| Gemini | OpenAI-compatible Chat Completions | bearer auth, client identification, request fields, SSE text/tools/usage | off |

Real Provider verification is isolated behind the `live` build tag. Fill the four key variables in the ignored local `.env`, then explicitly run:

```sh
make test-live-providers
```

This target first calls each Provider's authenticated model-list endpoint, selects a conservative low-cost text model, and sends one small streaming request. Missing keys, discovery failures, or the absence of a safe smoke model are hard failures rather than skips. `.env.example` documents the required names; `.env` is ignored by Git and must never be committed. Production requests choose a public model per request; model selection is not a process-level environment setting.

Deeper live function-call conformance is separately budget-gated. It discovers the same conservative models and sends exactly one forced, side-effect-free `ping` tool request per Provider, with a 64-token output cap:

```sh
make test-live-provider-tools
```

Ordinary live smoke does not run these four additional paid requests. Provider status failures, deadlines, redirects, disconnects, and cancellation are covered offline rather than manufactured against paid APIs.

The core migration is embedded in the binary and runs only when `GATEWAY_MIGRATE=true`. Production schema rollout should normally happen as a separate deployment gate.

## Stateful operations

Responses can continue a branch with `previous_response_id`, or use a mutable Conversation with `conversation`; these modes are intentionally mutually exclusive. Conversation create/get/delete and CAS item append are available under `/v1/conversations`. A Response transaction appends its new input while acquiring the Conversation, then appends terminal output and releases it during the same transaction that records final usage.

Every successful execution records the normalized usage, original provider usage payload, immutable price snapshot, calculated amount, authenticated Gateway API Key, and transactional outbox events. `usage_ledger`, `capability_usage_ledger`, and `cache_refresh_usage_ledger` are immutable facts; their daily tables are transactionally maintained reporting projections and are not the billing source of truth. Provider cache discounts are not labeled as Cache Protection savings unless separate Protected Hit evidence exists.

Responses accepts typed text, image, immutable file-reference, and reasoning items plus `instructions`, client function `tools`, `tool_choice`, sampling controls, and streaming/background lifecycle controls. Inline `file_data` is rejected so large payloads cannot fall through into PostgreSQL; clients must upload to regional object storage and pass an immutable `file_id`. Chat Completions translates text/image content arrays and preserves assistant tool-call history in both streaming and non-streaming forms. Reasoning summaries, encrypted reasoning, provider metadata, and end-user identity remain typed capability fields; adapters must declare support and reject unsupported replay rather than dropping them.

`store:false` uses a redacted transactional lifecycle so provider execution and financial usage can still complete atomically, then deletes the Response tombstone. Input, output, metadata, and provider error text are never written to Response or outbox payloads for ephemeral requests; `store:false` is rejected for mutable Conversations.

Cache refresh intents are durable PostgreSQL work, not process-local timers. Each intent records the Gateway API Key that sponsored it. Due work is claimed with `FOR UPDATE SKIP LOCKED`; the worker revalidates the current active Tenant and key, both Cache Protection permissions, and refresh-spend quota before the Provider call. Lease revision and fencing tokens are checked on every transition, successful refreshes advance both values, and ambiguous outcomes become terminal `uncertain` intents rather than automatic retries.

Requests opt in with a bounded `cache_protection` object (`enabled`, `max_spend_micros`, `max_refreshes`, `max_protection_window_seconds`, and `safety_margin_micros`). Missing or excessive bounds are rejected. Cache Protection runs only when the gateway mode is not `off`, both the Tenant and Gateway API Key policies explicitly allow it, and the request explicitly sets `cache_protection.enabled=true`. Absent permission is deny-by-default. The worker re-evaluates current principal permission, expiry, ROI, and refresh quota when it claims durable work, so revocation or delay cannot spend against a stale decision. The request field is the API seam for a future user-facing switch.

Production Cache Protection defaults to `GATEWAY_CACHE_PROTECTION_MODE=off`. In `off`, the process does not start the refresh worker or inspect Cache Anchors. Operators can explicitly select `shadow` to record eligible intents without Provider refresh calls. The only active treatment mode is `anthropic-one-refresh-canary`; it caps a Cache Lease at one refresh and assigns a stable holdout using `GATEWAY_CACHE_PROTECTION_HOLDOUT_PERCENT` (default 10). A durable shadow intent can be promoted in place when the same Cache Lease revision enters treatment. Successful refreshes write their own immutable usage/price record, actual refresh cost, raw Provider usage, and cohort before any later Protected Hit can be attributed. Treatment and holdout Response costs are persisted on observed ledger entries; once both cohorts exist, immutable `experimentally_validated_saving` snapshots record the per-Response incremental comparison.

## OpenTelemetry

Set `OTEL_EXPORTER_OTLP_ENDPOINT` (or the signal-specific traces/metrics endpoint variables) to export W3C-propagated HTTP and provider traces plus Response, token, cost, provider-duration, cache-outcome, and Stage A capability operation/error/latency/usage/spend metrics over OTLP/HTTP. Capability metric attributes are bounded to capability and Provider. With no endpoint, instrumentation is a no-op. `OTEL_SERVICE_NAME` defaults to `llm-gateway`.
