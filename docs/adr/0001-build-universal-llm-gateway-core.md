---
status: proposed
date: 2026-08-23
---

# Build a Go universal LLM gateway with a Responses-first core and cache protection

We will build a multi-tenant MaaS gateway in Go for customers operating from China into global markets. It will expose OpenAI-compatible Chat Completions and Responses contracts, route requests across international and Chinese model providers, keep authoritative Responses state in a home region, and offer an opt-in, economically guarded Prompt Cache Protection capability. We will reuse mature open-source provider and compatibility work behind replaceable seams, while owning the domain model, routing policy, state runtime, usage ledger, and customer-facing contract.

## Context

Customers need one integration for models whose request schemas, streaming events, tool behavior, caching rules, prices, regions, and failure modes differ materially. A thin field-mapping proxy cannot make these differences disappear safely, especially for Responses lifecycle state, streaming, retries after the first byte, and provider prompt caching.

The gateway must serve two distinct workload shapes:

- Interactive chat, coding, and agent workloads that require low time-to-first-token, streaming, tool events, and long reusable prompt prefixes.
- Machine-to-machine and background workloads that favor final responses, polling or webhooks, throughput, deterministic accounting, and regional data controls.

The architecture therefore separates correctness-bearing state from discardable acceleration:

```text
Global Edge
    |
    v
OpenAI-compatible Transport
    |
    v
Response Runtime ---- Routing Policy ---- Provider Adapter ---- Provider
    |                       |                    |
    |                       |                    +-- provider-native cache semantics
    |                       +-- capability, health, price, region, affinity
    |
    +-- Response Store          authoritative, quorum-backed
    +-- Event Journal           bounded streaming recovery
    +-- Usage/Savings Ledger    immutable financial evidence
    +-- Regional Cache          optional and discardable
```

## Decision

### 1. Own a Responses-first canonical model

The internal model will represent Responses-style typed input/output items, tool calls, usage, lifecycle status, and causal links. Public transports adapt to and from this canonical model:

- `POST /v1/responses`
- Response retrieve, cancel, delete, and input-item operations in the supported compatibility tier
- `POST /v1/chat/completions`, including SSE streaming

Chat Completions is an adapter, not the internal domain. Provider-native behavior is classified per Capability Profile as:

- `native`: preserved without semantic loss
- `translated`: implemented with documented differences
- `unsupported`: rejected explicitly

Strict mode rejects unsupported or lossy behavior. Best-effort mode permits declared translations. Fields are never silently dropped.

### 2. Put provider complexity behind narrow seams

The core will use small capability-specific interfaces rather than one provider interface containing every endpoint:

```go
type ResponseExecutor interface {
	Execute(context.Context, CanonicalRequest) (EventStream, error)
}

type CacheProtector interface {
	Inspect(context.Context, CacheAnchor) CacheCapability
	Refresh(context.Context, CacheAnchor) (RefreshResult, error)
}
```

Provider adapters own serialization quirks, event translation, usage normalization, cache breakpoints, timeout behavior, and provider errors. The Response Runtime owns lifecycle invariants, attempt history, idempotency, persistence, and the rule that a request cannot transparently fall back after its first visible output event.

The first release accepts exactly four Provider identities: `openai`, `deepseek`, `anthropic`, and `gemini`. OpenAI, DeepSeek, and Gemini initially share the OpenAI-compatible execution seam; Anthropic uses its native Messages API for Claude. This bounds the initial conformance and operations matrix while keeping the Provider seam replaceable. Additional Provider identities are rejected at route publication until they receive an explicit compatibility profile and conformance suite.

### 3. Use home-region single-writer state

A Tenant and each stateful Conversation or Response execution has a Home Region. Stateful writes are routed there and committed to a multi-AZ authoritative database before success is acknowledged.

- Region-local PostgreSQL is the initial Response Store and financial ledger.
- Production PostgreSQL must use synchronous multi-AZ durability, normally a two-of-three availability-zone commit managed by the selected cloud database.
- Cross-region replication is asynchronous for the initial release.
- Redis is allowed for routing hints, leases, rate limits, and cache-aside reads, but never as the authoritative Response Store.
- Large files and event payloads are stored in region-appropriate object storage and referenced by immutable identifiers.
- Transactional outbox records propagate cache invalidations, usage events, and projections after the authoritative transaction commits.

Stateless requests may fail over globally. Stateful requests do not perform opportunistic cross-region writes during a partition. Disaster recovery requires explicit promotion with an execution epoch or fencing token so only one region can write.

### 4. Support streaming as a first-class transport

SSE streaming is required for Chat Completions and Responses because it is the default user experience for chat, coding assistants, long reasoning, and agent tool progress.

The first release will provide live streaming with:

- normalized canonical events and monotonically increasing sequence numbers
- immediate flushing, keepalive, backpressure, and bounded idle timeouts
- final usage reconciliation and complete Response persistence
- provider pinning after the first visible event
- cancellation on client disconnect unless the request is explicitly backgrounded

The first release will not synchronously persist every token or guarantee cross-region replay. A bounded regional Event Journal and `starting_after` recovery can follow once live streaming and final Response durability are stable.

### 5. Separate three kinds of cache

The system treats these as different concepts with different correctness requirements:

1. **Prompt Cache**: provider-owned KV/prefix reuse. It is ephemeral, exact-prefix-sensitive, region/credential/provider scoped, and never authoritative.
2. **Response Store**: authoritative Response and Conversation state. It requires durable quorum-backed commits.
3. **Gateway Response Cache**: optional replay of completed answers. It is disabled by default and limited to explicitly eligible stateless requests.

No Prompt Cache or Gateway Response Cache participates in a correctness quorum. Cache misses, eviction, stale values, and refresh failures must degrade to ordinary provider execution rather than corrupting state.

### 6. Add opt-in Cache Protection

Cache Protection treats a provider Prompt Cache entry as a probabilistic Cache Lease over an exact Cache Anchor. The user enables a policy with explicit spend and duration limits. The gateway schedules a refresh only when expected net savings exceed a safety margin:

```text
expected_net_saving =
    P(cache-compatible continuation within protected window)
    * (predicted cold cost - predicted hit cost)
    - refresh request cost
    - forecast cost
    - route-lock opportunity cost
```

The Continuation Forecast targets the probability and timing of the next cache-compatible request, not the vague question of whether a task is finished. Deterministic signals are evaluated first: active tool work, open background response, recent cadence, idle duration, prior return behavior, prefix size, provider/model continuity, and client state. A small model may inspect a minimized session tail only when cheaper signals are insufficient and tenant policy permits content inspection.

Cache Protection has these invariants:

- Default hard-off at the gateway; the refresh worker and Cache Anchor inspection do not run in this mode.
- Enabling requires all three gates: a non-`off` gateway mode, explicit Tenant permission, and explicit request opt-in. Missing Tenant permission is deny-by-default.
- Finite `max_spend`, `max_refreshes`, and `max_protection_window`.
- Refresh in the Home Region and on the exact provider, model, credential scope, cache key, and compatible prompt serialization.
- Refresh never appends dummy items to the customer's Conversation or Response Chain.
- Hosted tools and other side-effecting capabilities are disabled or the refresh is rejected unless the Provider exposes a proven zero-output/cache-only method.
- One active refresh intent per Cache Lease revision, protected by a lease and fencing token.
- A real customer request cancels a pending refresh.
- Ambiguous provider outcomes are recorded as `uncertain` and are not blindly retried.
- Provider or Tenant policy changes are re-evaluated when durable work is claimed and can disable a refresh adapter immediately without changing callers.

Initial provider rollout:

1. Anthropic direct API: first implementation because it documents reusable prefix breakpoints, five-minute and one-hour policies, detailed cache usage, and zero-output pre-warming.
2. Gemini explicit Context Cache: extend the explicit cache object's TTL instead of issuing dummy inference where supported.
3. OpenAI: canary after validating an isolated stateless refresh recipe, exact breakpoint behavior, output cost, and absence of Conversation mutation.
4. DeepSeek: observe cache hits initially; do not schedule around an exact expiry because the public contract is best-effort and the cleanup window is not deterministic.

### 7. Maintain an auditable Savings Ledger

The gateway will not present all cache discounts as savings caused by Cache Protection. It records three separate measures:

- **Observed cache discount**: provider-reported cached tokens multiplied by the difference between the uncached and cached price snapshots.
- **Estimated protected saving**: a verified Protected Hit after subtracting refresh, forecast, storage, and route-lock costs.
- **Experimentally validated saving**: incremental cost reduction measured against a randomized eligible holdout population.

Every calculation references immutable provider/model/region price snapshots and the original usage payload. If a provider does not report reliable cache reads and writes, attribution is labeled estimated or unavailable.

The first production experiment requires an operator to move the gateway from `off` to `shadow` before issuing refreshes. A later treatment uses a small, stable holdout cohort, limits each Continuation to one refresh, and enables only prefixes large enough to clear a conservative ROI threshold.

## Technical selection

| Concern | Initial choice | Reason |
|---|---|---|
| Language | Go 1.26.x | Efficient streaming and networking, simple deployment, strong concurrency model, and alignment with the repository goal |
| Public wire contract | OpenAI Chat Completions and Responses | Largest compatibility surface for current SDK and agent ecosystems |
| Canonical model | Responses-like typed item/event IR | Preserves tools, multimodality, lifecycle, reasoning metadata, and branching better than chat messages |
| HTTP transport | Go standard `net/http` initially | Correct cancellation/streaming semantics and lower dependency commitment; benchmark before replacing |
| Authoritative store | PostgreSQL | Transactions, CAS revisions, append-only ledgers, outbox, mature multi-AZ offerings |
| Ephemeral coordination | Redis-compatible store | Rate limits, short leases, routing affinity, deduplication, and cache-aside reads |
| Large payloads | S3-compatible regional object storage | Cheap durable blobs without bloating transactional rows |
| Streaming | SSE | Required by OpenAI-compatible Chat and Responses; WebSocket/WebRTC reserved for Realtime |
| Cross-region model | Home Region, single writer, async replication | Avoids global write latency and split brain while preserving an explicit DR path |
| Observability | OpenTelemetry plus immutable usage events | Vendor-neutral traces/metrics and reconstructable cost accounting |
| Configuration | Versioned database configuration projected into atomic in-process snapshots | Fast reads, auditable changes, and safe hot reload |

Technology choices are initial adapters behind seams where two real implementations are expected. The external interfaces must not leak PostgreSQL, Redis, a specific cloud, or the selected provider execution library.

## Open-source reuse

### Reuse or adapt

| Project | License | Intended reuse |
|---|---|---|
| [Bifrost](https://github.com/maximhq/bifrost) | Apache-2.0 | Evaluate its Go core as the first provider-execution adapter; reuse provider schemas, streaming transformations, queue/isolation patterns, tests, and low-allocation techniques. Do not expose its broad provider interface as our domain interface. |
| [AIProxy](https://github.com/labring/aiproxy) | MIT | Port selected China-provider mappings, request/response quirks, body handling, usage normalization, and conformance tests. Avoid importing its application-coupled Gin/database/billing architecture into the core. |
| [OpenAI OpenAPI specification](https://github.com/openai/openai-openapi) | MIT | Generate or validate public wire fixtures and compatibility tests; do not generate the internal canonical model directly from the wire schema. |
| [openai-go](https://github.com/openai/openai-go) | Apache-2.0 | Black-box conformance client for Chat and Responses behavior. |
| [Aider](https://github.com/Aider-AI/aider) | Apache-2.0 | Reference its bounded prompt-cache keepalive lifecycle and cancellation behavior. Reimplement the policy in Go rather than depending on the Python application. |
| [LiteLLM](https://github.com/BerriAI/litellm) | Open source | Use as behavioral reference and interoperability corpus for provider-native `cache_control`, usage normalization, and edge cases; no Python runtime dependency. |

All copied or derived code must preserve required license notices and attribution. Provider test vectors may be recreated from public contracts where copying creates unnecessary coupling.

### Product references, not code dependencies

- [OpenRouter prompt caching and sticky routing](https://openrouter.ai/blog/tutorials/prompt-caching-sticky-routing/) informs session affinity, provider pinning, and per-generation cache-cost reporting.
- [Helicone caching](https://docs.helicone.ai/features/advanced-usage/caching) informs cost and cache observability, while remaining distinct from provider Prompt Cache protection.
- [Cloudflare AI Gateway caching](https://developers.cloudflare.com/ai-gateway/features/caching/) informs exact-response cache controls and explicit cache-status reporting.
- [Keeping the Cache Warm Pays](https://arxiv.org/abs/2607.19214) informs refresh interval economics and benchmark design, but its provider assumptions must be refreshed against current official contracts.
- [New API](https://github.com/QuantumNous/new-api) is a MaaS product and China-provider reference only. Its AGPL license and application coupling make direct core reuse unsuitable for a proprietary offering without separate legal approval.

## Product highlights

1. **China-to-global model access**: first-class adapters and operational knowledge for Chinese and international providers, with per-region credentials and data policies.
2. **One contract without silent degradation**: OpenAI-compatible Chat and Responses backed by explicit native/translated/unsupported Capability Profiles.
3. **Stateful Responses without global split brain**: home-region single-writer state, branchable Response Chains, mutable Conversations, CAS revisions, and fenced disaster recovery.
4. **Correct streaming semantics**: normalized events, usage reconciliation, provider pinning after first byte, and an evolution path to resumable streams.
5. **Cache-aware routing**: preserve provider/model/credential affinity when its expected benefit exceeds price, health, compliance, and latency alternatives.
6. **Predictive Cache Protection**: bounded, provider-aware refresh based on continuation probability and expected value rather than fixed infinite heartbeat.
7. **Provable FinOps**: distinguish observed discounts from estimated and experimentally validated net savings.
8. **Data-control transparency**: `store:false`, retention, content-inspection, home-region, and Cache Protection policies are explicit per Tenant.

## Considered options

### Use Bifrost or AIProxy unchanged as the entire product

Rejected. Bifrost offers a strong Go execution core but does not own our required domain, China-provider depth, global Responses state, or Cache Protection economics. AIProxy has strong China-provider coverage but couples transport, persistence, billing, and provider logic too tightly for the desired core. Both remain valuable behind adapters or as source material.

### Fork New API as the MaaS platform

Rejected. It is feature-rich and China-oriented, but AGPL obligations, product coupling, and incomplete Responses translation make it a poor foundation for this core.

### Implement a lowest-common-denominator chat schema

Rejected. It would silently discard Responses lifecycle, typed items, reasoning metadata, multimodal content, hosted tool events, and branching semantics. Capability-aware translation is more honest and extensible.

### Use a globally active-active Conversation Store

Rejected for the initial release. It creates cross-region write latency, conflict resolution, and split-brain risk for mutable append operations. Home-region ownership gives a simpler correctness model.

### Treat Redis or provider cache as authoritative state

Rejected. Both are subject to eviction, TTL, routing affinity, and best-effort behavior. Correctness belongs in the Response Store.

### Run fixed cache heartbeats for every active session

Rejected. It can spend more than a future cold request, prolong data residency, consume rate limits, generate side effects, and create a cache-residency externality. Refresh must be opt-in, bounded, and economically justified.

## Consequences

- The first release has more internal modeling work than a pass-through proxy, but provider quirks remain local to adapters and public behavior remains testable through the canonical seam.
- Stateful requests may become temporarily unavailable during a Home Region outage rather than accepting conflicting writes elsewhere.
- Provider capabilities and pricing must be versioned data, not hard-coded assumptions.
- Streaming retry policy becomes stricter after the first visible event.
- Cache Protection can improve cost and latency, but it also creates forecast error, scheduling, privacy, and financial-attribution responsibilities.
- Exact compatibility is a continuously tested product property rather than a one-time schema conversion.

## Delivery sequence

1. Canonical items/events, provider capability registry, OpenAI-compatible transports, and conformance fixtures.
2. Stateless execution with streaming, usage normalization, provider attempts, and strict post-first-byte behavior.
3. Durable Responses, Response Chains, Conversations, Home Region routing, idempotency, CAS, and deletion tombstones.
4. Tenant policy, quotas, pricing snapshots, immutable usage ledger, and routing by capability/health/price/region.
5. Prompt Cache observability and cache-aware provider affinity without proactive refresh.
6. Cache Protection shadow mode and offline ROI evaluation.
7. Anthropic one-refresh canary with randomized holdout and Savings Ledger.
8. Additional refresh adapters or Provider identities only after provider-specific conformance and economic tests pass.

## Initial non-goals

- Universalizing every hosted tool across providers.
- Realtime audio or bidirectional WebRTC/WebSocket protocols.
- Global active-active writes for Conversations.
- Guaranteed token-by-token cross-region replay.
- Semantic response caching by default.
- Transparent fallback after streamed content has begun.
- Exposing private reasoning content when the Provider does not support portable replay.

## Source notes

Provider cache behavior changes frequently. Implementations must use versioned capability data and periodically verify these official sources:

- [OpenAI Prompt Caching](https://developers.openai.com/api/docs/guides/prompt-caching)
- [Anthropic Prompt Caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)
- [Gemini Context Caching](https://ai.google.dev/gemini-api/docs/caching)
- [DeepSeek Context Caching](https://api-docs.deepseek.com/guides/kv_cache/)
