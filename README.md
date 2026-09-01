# Universal LLM Gateway

A Responses-first, multi-tenant Go gateway implementing [ADR 0001](docs/adr/0001-build-universal-llm-gateway-core.md). It exposes OpenAI-compatible Responses and Chat Completions APIs plus the Stage A capability interfaces from [ADR 0009](docs/adr/0009-expand-capability-specific-inference-interfaces.md): embeddings, moderations, and reranking. Provider behavior stays behind capability-aware Model Routes, and lifecycle state uses tenant-scoped CAS revisions. The first release is intentionally limited to OpenAI, DeepSeek, Anthropic (Claude), and Gemini. The accepted Cloudflare-edge/GCP-core deployment target and staged rollout gates are documented in [ADR 0010](docs/adr/0010-deploy-cloudflare-edge-gcp-core.md); that target is not itself deployment evidence.

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
GATEWAY_DATABASE_TRANSPORT_ATTESTATION=authenticated-encrypted
GATEWAY_ACCESS_PROJECTION=true
GATEWAY_API_KEY_CURRENT_DIGEST_VERSION=2
GATEWAY_API_KEY_PEPPERS_JSON={"1":"old...","2":"current..."}
GATEWAY_LOCAL_REGION=us-west
GATEWAY_HOME_REGION_URLS_JSON={"us-west":"https://us-west.gateway.example","eu-west":"https://eu-west.gateway.example"}
GATEWAY_TRUSTED_PROXY_CIDRS=10.0.0.0/8,2001:db8::/32
GATEWAY_CACHE_PROTECTION_MODE=off
GATEWAY_ROUTING_CATALOG=true
GATEWAY_ID=gateway-us-west-1
GATEWAY_CONTROL_RELAY_URL=https://control.example
GATEWAY_CONTROL_RELAY_HMAC_KEY=at-least-32-byte-machine-key
GATEWAY_OPERATIONS_URL=https://control.example
GATEWAY_OPERATIONS_HMAC_KEY=at-least-32-byte-machine-key
```

The Tenant Administration control plane is a separate process and requires a
separate runtime database role plus Human IAM JWKS verification:

```text
CONTROL_PLANE_DATABASE_URL=postgres://tenant_admin_runtime@...
CONTROL_PLANE_DB_ROLE=tenant_admin_runtime
CONTROL_PLANE_DATABASE_TRANSPORT_ATTESTATION=authenticated-encrypted
CONTROL_IAM_JWKS_URL=https://iam.example/.well-known/jwks.json
CONTROL_IAM_ISSUER=https://iam.example
CONTROL_IAM_AUDIENCE=llm-gateway-control-plane
CONTROL_API_KEY_CURRENT_DIGEST_VERSION=2
CONTROL_API_KEY_PEPPERS_JSON={"1":"old...","2":"current..."}
CONTROL_SECRET_CUSTODY_BACKEND=gcp-secret-manager
CONTROL_GCP_SECRET_PROJECT=provider-secret-project
CONTROL_GATEWAY_HMAC_KEYS_JSON={"gateway-us-west-1":"at-least-32-byte-machine-key"}
CONTROL_GATEWAY_REGIONS_JSON={"gateway-us-west-1":"us-west"}
CONTROL_METERING_HMAC_KEYS_JSON={"metering-us-west-1":"different-at-least-32-byte-machine-key"}
CONTROL_METERING_REGIONS_JSON={"metering-us-west-1":"us-west1"}
```

Metering runs as a third, independently deployable process. The first
deployment may share PostgreSQL physically, but it uses a separate runtime role
and owns only its inbox, immutable facts, projection generations, and export
jobs:

```text
METERING_DATABASE_URL=postgres://metering_runtime@...
METERING_DB_ROLE=metering_runtime
METERING_DATABASE_TRANSPORT_ATTESTATION=authenticated-encrypted
METERING_IAM_JWKS_URL=https://iam.example/.well-known/jwks.json
METERING_IAM_ISSUER=https://iam.example
METERING_IAM_AUDIENCE=llm-gateway-metering
METERING_EXPORT_BACKEND=gcs
METERING_EXPORT_GCS_BUCKET=regional-immutable-metering-exports
METERING_EXPORT_GCS_PREFIX=exports
METERING_EXPORT_GCS_REGION=us-west1
METERING_EXPORT_SIGNING_KEY=at-least-32-byte-download-signing-key
METERING_WORKER_ID=metering-us-west-1
METERING_ID=metering-us-west-1
METERING_REGION=us-west1
METERING_OPERATIONS_URL=https://control.example
METERING_OPERATIONS_HMAC_KEY=different-at-least-32-byte-machine-key
```

The Gateway transaction that completes billable work now writes its immutable
Usage Ledger fact, settles the exact Quota Reservation, and enqueues a stable,
content-free usage event atomically. Metering consumes those events with leased
claims and inbox deduplication; inference does not call Metering synchronously.
`GET /metering/v1/operations/status` exposes pending/poison event counts,
oldest pending time, active projection generation/cutoff, and queued exports.
Tenant and platform identities can query summaries, bounded hour/day series,
stable-cursor events, Tenant/key aliases, and Response usage. CSV exports are
asynchronous, fixed to their requested cutoff, integrity checked, and exposed
through five-minute HMAC-signed downloads. Production uses a native GCS
`ExportStore` with Workload Identity, verifies the configured bucket is in the
expected single region, and creates every object with `ifGenerationMatch=0`.
An idempotent retry must read the same bytes; different existing bytes fail
closed. Grant the Metering identity only `storage.buckets.get`,
`storage.objects.create`, and `storage.objects.get` on that bucket. The
filesystem adapter and `METERING_EXPORT_DIRECTORY` are development-only.

Platform Metering identities may omit `tenant_id` from usage summary and
time-series requests to receive an all-Tenant aggregate. Quota admission
failures are emitted by the Gateway as content-free outbox evidence and queried
through `GET /metering/v1/quota-denials`; Tenant identities remain forced to
their verified Tenant and platform identities may query across Tenants.

Run the Metering schema as an owner job, then apply its runtime boundary with
`make configure-metering-role ADMIN_DATABASE_URL=... METERING_DB_ROLE=...`.
That role can consume usage outbox rows but cannot read `responses` or any
authoritative Usage Ledger. Historical initialization is a separate one-shot
operator invocation using a time-bounded bootstrap role with ledger read
access: set `METERING_BOOTSTRAP_LEDGER=true`; the process backfills bounded
content-free facts, rebuilds a fenced generation, and exits before serving.
Production refuses in-process migration, plaintext database transport, a role
mismatch, missing JWKS/Operations machine identity, non-HTTPS Operations, a
non-regional or wrong-region export bucket, or a short export signing key.

Run schema migration as a separate owner job, then apply least-privilege grants
with `make configure-tenant-admin-roles ADMIN_DATABASE_URL=... TENANT_ADMIN_DB_ROLE=... GATEWAY_DB_ROLE=...`.
The control-plane runtime refuses in-process production migration and verifies
its connected role. The Gateway role owns only its local Access, Provider
Connection execution, and Routing Catalog projection tables, relay cursor, and
rollout receipts. It has no read access to `control_outbox`, the control-plane
Provider Connection view, or authoritative Tenant and Gateway API Key tables.
Production refuses to
start unless `GATEWAY_ACCESS_PROJECTION=true`. Both production processes require
the transport attestation and reject PostgreSQL configurations that are not
certificate-verified TLS; the repair command enforces the same rule on both its
source and destination connections.
`/healthz` is liveness only. `/readyz` performs bounded local dependency,
schema, projection, Routing Catalog, fence, and durable-outbox checks without
calling Providers, Human IAM, Billing, Metering, or the remote control plane.
Tenant, credential, Provider Connection, and Routing Catalog mutations
atomically append `control_outbox`; that is durable enqueue, not delivery proof.
Production Gateways pull a bounded, globally ordered, region-scoped stream from
`/internal/v1/control-events`. The persisted cursor advances only after every
local projection accepts the batch, so retry is idempotent and revision gaps
fail closed. Control-plane outbox inserts are serialized at the transaction
boundary so a later delivery sequence cannot commit before an earlier one, and
each batch reads its floor, source head, and rows from one repeatable-read
snapshot. Each fetch also records the control-plane source head and last
successful fetch time; Operations exposes their difference as relay backlog.
Fetch and projection failures durably record a stable error code; transient
execution-secret delivery failures leave the batch cursor unchanged even though
the newly observed source head remains visible. Startup fails unless the bounded
catch-up reaches `cursor == source_head`. A Gateway behind the retained-history
floor fetches the authenticated, `no-store` `/internal/v1/control-bootstrap`
bundle, replaces its regional Access and secret-free Provider Connection
projections plus the global Routing Catalog, and advances the relay cursor only
after all three succeed. The bundle contains Gateway API Key digests needed for
local verification, but never raw Gateway API Keys, Provider secrets, or Secret
Manager references. Only then does the Gateway compile and install the persisted
Routing Catalog. HTTPS supplies transport encryption and
the Gateway HMAC binds its
identity to the timestamp, method, exact path, and query. The original shared
PostgreSQL consumers remain development compatibility adapters only.
The same recovery runs if a live Gateway falls behind the floor: readiness is
withdrawn until an authoritative bootstrap succeeds, then incremental delivery
resumes. Durable per-region Access heads preserve desired-versus-applied
Operations evidence after retained outbox rows are deleted.

Control Event deletion is an explicit operator action, not a control-plane
runtime worker. `make prune-control-events` requires a target cursor, a typed
confirmation, authenticated database transport, and a dedicated retention role
with `SELECT`/`DELETE` on `control_outbox`, `SELECT`/`UPDATE` on
`control_event_history`, and `SELECT` on `operations_gateway_heartbeats`. Each
bounded batch stops at the lowest recently reported Gateway relay cursor;
Gateways whose heartbeat is older than the configured window must bootstrap if
they later return:

```sh
CONTROL_EVENT_RETENTION_DATABASE_URL='postgres://...' \
CONTROL_EVENT_RETENTION_THROUGH=123456 \
CONTROL_EVENT_RETENTION_LIMIT=1000 \
CONTROL_EVENT_RETENTION_GATEWAY_STALE_AFTER=15m \
make prune-control-events
```

Gateways send signed soft-state heartbeats plus durable Routing Catalog and
Access Projection rollout observations to
`/internal/v1/operations/gateway-observations`. The HMAC binds
the configured Gateway ID and region to the timestamp, HTTP method, path, and
exact body. Production control-plane processes do not promote the legacy shared
Gateway inbox; that collector remains development compatibility only. Configure
`GATEWAY_OPERATIONS_URL`, `GATEWAY_OPERATIONS_HMAC_KEY`, `GATEWAY_ID`,
`GATEWAY_LOCAL_REGION`, and `GATEWAY_BUILD_SHA`. Operations queries live under
`/control/v1/operations/{gateways,metering,publications,outbox,consumers,jobs}`.
Metering separately sends content-free HMAC-signed regional observations to
`/internal/v1/operations/metering-observations`; these expose projection
generation/cutoff, pending age/count, poison count, export backlog, and
heartbeat health. The control plane joins the latest same-region observation
into Gateway summaries without making Metering an inference dependency.
Operations queries expose
desired/applied revisions, heartbeat and consumer lag, outbox age, quota/cache/
retention backlog, revocation propagation, explicit Metering cutoff/status,
build SHA, and schema version. Desired Access revisions are computed per region,
while the applied relay cursor advances across the globally ordered source.

Provider Connection Administration stores upstream credentials in GCP Secret
Manager through Workload Identity. PostgreSQL stores only the Secret Manager
reference and non-sensitive credential-version metadata. Production does not
permit the in-memory custody adapter. Register/update/enable/disable use CAS and
idempotency; probe, discovery, and credential rotation return `202 Accepted`
with `/control/v1/provider-operations/{operation_id}`. Probe and discovery are
authorized by deployment configuration, never a caller-supplied boolean.
Production keeps them disabled unless `CONTROL_PROVIDER_LIVE_OPERATIONS=explicitly-authorized`,
`CONTROL_PROVIDER_LIVE_AUTHORIZATION_ID`, and a bounded
`CONTROL_PROVIDER_DISCOVERY_MAX_REQUESTS` are configured. The persisted budget
has a zero-spend ceiling; the worker applies a strict timeout and stores
only bounded, redacted status. A failed probe updates observed health without
changing enabled/disabled administrative intent. Discovery records Provider
inventory observations and their raw-response hash, but never publishes Model
Routes. Credentials can only be delivered to the exact built-in endpoint for
the declared Provider identity.

Provider Connection schema-3 events carry a complete secret-free execution
projection: Provider identity, validated Base URL, region, credential scope,
capability declaration, administrative status, connection revision, and
credential version. A Gateway compiles routes from that local projection. It
fetches the exact immutable credential only from
`/internal/v1/provider-connection-secrets/{connection_id}` over the same
authenticated HTTPS machine boundary, with both connection revision and
credential version required. The response is `no-store`; neither events nor the
Gateway projection contain secret material or Secret Manager references.

Repair a specific Tenant after a detected revision gap with a bounded operator
job that has read access to the authoritative control database and write access
to the target regional projection database:

```sh
ACCESS_PROJECTION_REPAIR_TENANT_ID=tenant-id \
CONTROL_PLANE_DATABASE_URL=postgres://control-reader@... \
GATEWAY_DATABASE_URL=postgres://projection-writer@... \
CONTROL_API_KEY_CURRENT_DIGEST_VERSION=2 \
CONTROL_API_KEY_PEPPERS_JSON='{"1":"old...","2":"current..."}' \
make repair-access-projection
```

The command never prints digests or raw keys, rejects stale or incomplete
snapshots, preserves active concurrency leases, and applies the replacement in
one regional transaction.

In PostgreSQL mode, `tenants`, `tenant_policy_revisions`, `api_keys`, and `api_key_policy_revisions` are authoritative, while inference authentication reads only `gateway_access_projection`. Requests authenticate by a peppered HMAC digest; the raw Gateway API Key is never persisted, and a caller-supplied Tenant header cannot change the authenticated Tenant. The single `GATEWAY_API_KEY_PEPPER` variable is the version-1 compatibility form for development. During a bounded rotation, configure `GATEWAY_API_KEY_PEPPERS_JSON={"1":"old...","2":"current..."}` and `GATEWAY_API_KEY_CURRENT_DIGEST_VERSION=2`; at most eight versions may be active. New issuance/import uses the current version, while configured prior versions remain verifiable. Remove an old pepper only after both the authoritative control-plane count and every regional projection count prove no active key still references that digest version.

Environment key maps are only for an explicit, idempotent first development bootstrap. A raw bootstrap key must contain at least 24 characters. Run the dedicated one-shot `make bootstrap-access` process; the Gateway data plane rejects bootstrap flags and never writes authoritative Tenant or key state. The command atomically seeds its Access Projection when enabled; then remove the raw-key variables. Existing production Tenants are seeded through a control-plane snapshot before the Gateway role is restricted to projection tables:

```text
GATEWAY_DATABASE_URL=postgres://...
GATEWAY_API_KEYS_JSON={"long-secret-gateway-token":"tenant-id"}
GATEWAY_TENANT_HOME_REGIONS_JSON={"tenant-id":"us-west"}
GATEWAY_TENANT_EXECUTION_EPOCHS_JSON={"tenant-id":1}
GATEWAY_TENANT_POLICIES_JSON={"tenant-id":{"revision":1,"max_concurrent_responses":32,"max_input_items":256,"retention_seconds":2592000,"allow_stored_responses":true,"allow_cache_protection":false,"allow_content_inspection":false,"limits":{"requests_per_minute":1000,"tokens_per_minute":5000000,"monthly_spend_micros":500000000,"refresh_monthly_spend_micros":0,"currency":"USD"}}}
GATEWAY_API_KEY_POLICIES_JSON={"long-secret-gateway-token":{"revision":1,"allow_cache_protection":false,"limits":{"requests_per_minute":100,"monthly_spend_micros":50000000,"refresh_monthly_spend_micros":0,"currency":"USD"}}}
GATEWAY_API_KEY_METADATA_JSON={"long-secret-gateway-token":{"environment":"production","owner":"platform"}}
```

Tenant and key policy publication uses compare-and-swap revisions and immutable policy history. Key metadata uses a separate row revision. The control API supports server-generated issuance, list/get, metadata and expiry update, terminal revoke, immediate or grace-period rotation, policy history, and effective-policy inspection. A secret is returned only for the first successful issue or rotation response; idempotent replay returns metadata without plaintext. An active key can be revoked, a Tenant can be suspended and reactivated, and a closed Tenant cannot be reopened. A one-second grace reconciler terminates expired rotation predecessors. Healthy regional projection polling propagates revocation without an inference-time control-plane call. `null`/absent limits inherit from the other scope; explicit `0` or an explicit empty restriction denies. A Gateway API Key can only narrow a Tenant limit or permission.

Gateway API Key Policy can restrict public models, operations, source CIDRs,
regions, and concurrent billable requests. CIDR evaluation trusts forwarded
addresses only from canonical `GATEWAY_TRUSTED_PROXY_CIDRS`. Concurrency uses
renewable PostgreSQL leases under a per-key advisory lock, so the limit is hard
across Gateway replicas rather than process-local.

Successful authentication is coalesced in memory and flushed asynchronously to
the regional projection's coarse `last_used_at`; it is operational activity
metadata, not financial evidence. Projection status logs include aggregate
revision, pending delivery lag, and revocation-specific apply time/lag. Production
startup fails when either authoritative state or a regional projection still has
an active key whose digest pepper version is absent from the configured ring.

Each managed route declares its public/provider model, Provider Connection,
execution/Home Region, versioned Capability Profile, immutable Provider Cost
Snapshot, administrative status, selection policy, explicit Tenant visibility,
and cache policy. Creating or discovering a Provider Connection never makes a
model routable. Credentials are resolved from Secret Custody at the Gateway and
are never embedded in a Catalog or returned by the management API. The Provider
identity must be one of `openai`, `deepseek`, `anthropic`, or `gemini`.

Routing Catalog changes use complete drafts instead of CRUD against active
routes. `validate` is side-effect free; optional `probe` delegates one
deduplicated operation per referenced Provider Connection to the separately
authorized, budgeted Provider operation queue. Publish uses the draft base
revision as CAS, creates an immutable revision, and emits
`RoutingCatalogPublished`. Restore copies an old document into a new
monotonically increasing revision. Gateways compile a complete snapshot before
atomic replacement, retain the last valid revision on rejection, and write a
per-Gateway regional inbox observation. A control-plane collector is the only
component that promotes those observations into authoritative rollout receipts
and publication status. Within a priority, weight uses stable weighted
ordering; `max_concurrency` shares one permit pool across the route's Response
and capability executors. `draining` rejects new assignments while preserving
explicit pinned execution.

Example managed Catalog document:

```json
{
  "routes": [{
    "route_id": "openai-us-primary",
    "public_model": "gateway-model",
    "provider_connection_id": "pc-openai-us",
    "provider_model": "provider-model-id",
    "execution_region": "us-west",
    "home_region": "us-west",
    "capability_profile_revision": 1,
    "capabilities": {"text": "native", "streaming": "native"},
    "provider_cost_snapshot": {
      "id": "provider-model-usd-2026-08-29",
      "provider": "openai",
      "model": "provider-model-id",
      "region": "us-west",
      "currency": "USD",
      "input_per_million_micros": 1000000,
      "cached_input_per_million_micros": 100000,
      "cache_write_per_million_micros": 0,
      "output_per_million_micros": 4000000,
      "effective_at": 1787961600,
      "source": "provider-contract-2026-08-29"
    },
    "administrative_status": "active",
    "selection_policy": {"priority": 10, "weight": 100, "max_concurrency": 32, "sticky_routing_eligible": true},
    "tenant_visibility_policy": {"tenant_ids": ["tenant-a"], "limit_policy_revisions": {"tenant-a": 1}},
    "cache_usage_reliable": true,
    "cache_protection_policy": {"enabled": false}
  }]
}
```

Visibility must explicitly declare either `tenant_ids` or `all_tenants: true`;
each listed Tenant also pins an immutable `limit_policy_revisions` entry.
Publish revalidates current Provider Connection state so a connection changed
after draft validation cannot slip into an active revision.

OpenAI uses its native Responses API so Codex fields, reasoning replay, namespace tools, and client-owned tool loops remain lossless. DeepSeek and Gemini use their OpenAI-compatible Chat Completions surfaces behind the shared adapter. Anthropic uses its native Messages API so Claude-specific streaming, usage, and prompt-cache evidence remain explicit.

When `GATEWAY_ROUTING_CATALOG` is not enabled, the legacy PostgreSQL bootstrap
still loads `model_routes` from `configuration_history` and uses `api_key_env`.
Set `GATEWAY_BOOTSTRAP_ROUTES=true` only for its first revision. Later legacy
publications require `GATEWAY_PUBLISH_ROUTES=true`, explicit expected/new
revisions, and an actor. This compatibility path is not the managed production
credential flow.

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

Stage A uses narrow `EmbeddingExecutor`, `ModerationExecutor`, and `RerankExecutor` seams. Route configuration never infers one capability from another: `/v1/capabilities` publishes only healthy, Home Region-compatible declarations usable by the authenticated Tenant. Managed Catalog visibility is always explicit; only the legacy route format treats omitted `tenant_ids` as global. The production HTTP adapter uses the configured Provider model and paths while returning the public model ID, with a bounded upstream request timeout. Offline conformance tests cover bearer/header forwarding, wire shapes, normalized usage, timeout/error classification, and content-free evidence.

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

With `make run-control-plane-dev` running in another terminal, exercise the
complete Tenant and Gateway API Key administration lifecycle:

```sh
make test-tenant-admin-blackbox
```

Exercise Provider Connection registration, listing, enable/disable, probe,
model discovery, credential rotation, operation polling, and secret non-echo:

```sh
make test-provider-connection-blackbox
```

Exercise Routing Catalog draft, deterministic validation, delegated probe,
immutable publish/history, and restore:

```sh
make test-routing-catalog-blackbox
```

The control-plane development process uses deterministic Secret Custody and
Provider adapters, so this black box performs no Provider network calls and
incurs no Provider cost.

With Compose PostgreSQL and `make run-metering-dev` running, exercise Metering
readiness, Tenant summary isolation, asynchronous immutable CSV export, signed
download, integrity/content-free headers, and projection status:

```sh
make test-metering-blackbox
```

The OpenAI SDK black-box suite covers public model listing, Responses
create/retrieve/input-items/delete, `previous_response_id`, Responses streaming,
Chat Completions, Chat streaming, and Conversations. Override `GATEWAY_BASE_URL`,
`GATEWAY_API_KEY`, and `GATEWAY_MODEL` to target another deployment. It requires
the `openai` Python package and defaults to the zero-cost local `echo-v1` route.

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
