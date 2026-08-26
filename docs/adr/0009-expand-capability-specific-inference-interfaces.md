---
status: proposed
---

# Expand inference through capability-specific interfaces

## Context

The first release intentionally exposes Responses, Chat Completions, Models, and Conversations for OpenAI, DeepSeek, Anthropic, and Gemini. A broader MaaS product may also require embeddings, moderation, reranking, image, audio, files, batches, Realtime, and video generation.

These capabilities have different request shapes, payload sizes, streaming transports, lifecycle models, safety policies, usage units, and Provider support. Forcing all of them through the canonical Response model would make the core shallow and leaky. Adding an HTTP path without capability declarations, normalized usage, pricing, quota admission, and conformance tests would advertise compatibility the Gateway cannot prove.

## Decision

Keep Responses as the canonical model for text/agent execution and add narrow capability-specific modules only when at least one production Provider adapter and one deterministic test adapter exist. Every new interface uses the existing Tenant, Authenticated Principal, Home Region, Model Route, Capability Profile, quota, usage, price, and audit rules where applicable.

The target compatibility surface is staged.

## Stage A: embeddings, moderation, and reranking

```text
POST /v1/embeddings
POST /v1/moderations
POST /v1/rerank
```

Define separate executor seams:

```go
type EmbeddingExecutor interface {
    Embed(context.Context, EmbeddingRequest) (EmbeddingResult, error)
}

type ModerationExecutor interface {
    Moderate(context.Context, ModerationRequest) (ModerationResult, error)
}

type RerankExecutor interface {
    Rerank(context.Context, RerankRequest) (RerankResult, error)
}
```

Embedding usage records input units and dimensions; reranking records document count and Provider token evidence; moderation records usage only when the Provider reports or bills it. Vector payloads and moderation results are not stored unless the request and Tenant Policy explicitly allow it.

## Stage B: Files and Batches

```text
POST   /v1/files
GET    /v1/files
GET    /v1/files/{file_id}
DELETE /v1/files/{file_id}
GET    /v1/files/{file_id}/content

POST   /v1/batches
GET    /v1/batches
GET    /v1/batches/{batch_id}
POST   /v1/batches/{batch_id}/cancel
```

Files use regional object storage with immutable object identity, checksum, media type, size bounds, malware/content-policy hooks, retention, and Tenant isolation. Database rows store metadata and object references, not large inline payloads.

Batch is a durable Gateway lifecycle, not a thin passthrough to one Provider's batch ID. It records individual item identity, idempotency, status, Usage Ledger facts, cancellation limits, partial failure, output files, and Provider provenance. A batch cannot silently retry an item after an ambiguous Provider side effect.

## Stage C: images and audio

```text
POST /v1/images/generations
POST /v1/images/edits
POST /v1/images/variations

POST /v1/audio/transcriptions
POST /v1/audio/translations
POST /v1/audio/speech
```

Multipart parsing, upload limits, temporary-file handling, object storage, output retention, and content policy are explicit parts of the interface. Generated media is represented by durable Media Artifact metadata and regional object references rather than unbounded base64 persisted in PostgreSQL.

Usage normalization supports capability-specific units such as image count, size, quality, duration, characters, or audio seconds while preserving raw Provider usage and immutable Price Snapshot evidence.

## Stage D: Realtime and video

```text
GET|CONNECT /v1/realtime
POST        /v1/videos
GET         /v1/videos/{video_id}
POST        /v1/videos/{video_id}/cancel
GET         /v1/videos/{video_id}/content
```

Realtime requires a separate WebSocket/WebRTC transport, connection admission, ephemeral credentials, session-duration quota, incremental usage settlement, backpressure, and disconnect semantics. The term Realtime Connection is used; it is not a Conversation and does not weaken the project's rule against using Session for Conversation/Response Chain state.

Video is an asynchronous Media Generation lifecycle with queued, running, succeeded, failed, cancelled, and uncertain states. Provider polling and webhooks are adapters behind one Gateway lifecycle. Large outputs live in regional object storage.

The exact video wire contract remains Gateway versioned until a stable compatibility target is deliberately selected.

## Capability and routing rules

Each Model Route declares capability support as `native`, `translated`, or `unsupported`. Model listing or a capability catalog returns only routes usable by the authenticated Tenant in its Home Region. Strict compatibility rejects lossy translation. Best-effort mode permits only documented translations.

Adding a Provider to one capability does not imply support for another. Execution, streaming, usage normalization, errors, cancellation, and storage behavior require a capability-specific conformance suite.

## Quota and pricing

Extend Limit Policy with typed capability limits rather than overloading text tokens:

```text
embedding_input_units
rerank_documents
image_count
audio_seconds
realtime_connection_seconds
video_seconds
file_bytes
batch_items
capability_spend_micros
```

All billable operations reserve a conservative maximum before the first Provider side effect and settle against immutable usage evidence. When the Provider may have started work but usage is unknown, the reservation becomes uncertain rather than being released.

## Scope and sequencing

Acceptance of this ADR defines the target, not immediate support. A stage becomes public only after its storage, policy, quota, price, Provider adapter, offline conformance, HTTP black-box, retention, and Operations work are complete. Stage A is first because it adds bounded non-streaming interfaces without requiring object storage. Files precede media and batches that depend on durable blobs. Realtime and video remain last due to their distinct lifecycle and operational cost.

## Implementation status

Stage A is implemented in the local source tree:

- `/v1/embeddings`, `/v1/moderations`, `/v1/rerank`, and the authenticated `/v1/capabilities` catalog;
- narrow capability executors, a deterministic adapter, and an OpenAI-style production HTTP adapter with configurable paths;
- typed Limit Policy intersection, durable typed Quota Reservations per Provider attempt, uncertain side-effect handling, execution-epoch fencing before and after Provider execution, immutable content-free capability usage, and daily projections;
- Tenant-scoped route visibility, strict field-result validation, a bounded upstream timeout, offline Provider failure/cancellation conformance, HTTP Tenant-isolation behavior, PostgreSQL migration/integration coverage, and bounded capability metrics.

Stages B through D remain design scope and are not advertised. This implementation note does not change the ADR from `proposed` to `accepted`.

## Rejected alternatives

### Put every capability inside `POST /v1/responses`

Rejected. Embeddings, reranking, file storage, media generation, and Realtime have incompatible lifecycle and usage semantics.

### Add proxy endpoints before normalized usage and Policy

Rejected. It would create unaccounted Provider side effects and unverifiable Tenant limits.

### Claim Provider-wide support from model discovery

Rejected. Inventory does not prove wire compatibility or behavior.

## Acceptance criteria

- Each public capability has a narrow executor interface, deterministic adapter, production adapter, and Capability Profile entries.
- Every billable path reserves and settles typed quota and writes immutable Usage Ledger evidence.
- Large payloads and media never inflate PostgreSQL rows; regional object references are immutable and Tenant scoped.
- Strict mode rejects unsupported or lossy behavior without silently dropping fields.
- Cancellation, timeout, ambiguous outcome, and Provider failure have offline conformance coverage.
- HTTP black-box tests cover the advertised wire contract and Tenant isolation.
- Operations exposes capability-specific latency, error, spend, backlog, and storage signals with bounded metric cardinality.
