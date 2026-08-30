---
status: proposed
---

# Expose Usage and Quota through rebuildable Metering projections

## Context

ADR 0002 made Usage Ledger and Cache Refresh Usage Ledger immutable execution facts and implemented hard Tenant/key Quota Reservations. The Gateway transactionally updates daily Tenant and API Key Usage Projections, and one internal query can return API Key daily usage. There is no HTTP query interface, Tenant summary, detailed event search, projection rebuild workflow, outbox relay, Metering inbox, or financial correction model.

Usage query traffic and long-range aggregation must not contend with inference transactions. Billing must not become a synchronous dependency of Provider execution. Prompt and Response content must not leak into financial events or general management logs.

## Decision

Keep authoritative Usage Ledger creation inside the Gateway transaction that completes Provider usage and settles its Quota Reservation. Build a separately deployable Metering module that consumes content-free usage events and owns query projections.

The first Metering HTTP interface is:

```text
GET /metering/v1/usage/summary
GET /metering/v1/usage/timeseries
GET /metering/v1/usage/events
GET /metering/v1/usage/exports
GET /metering/v1/tenants/{tenant_id}/usage
GET /metering/v1/tenants/{tenant_id}/gateway-api-keys/{api_key_id}/usage
GET /metering/v1/responses/{response_id}/usage
```

The control plane also exposes current enforcement state:

```text
GET /control/v1/tenants/{tenant_id}/effective-policy
GET /control/v1/tenants/{tenant_id}/quota-snapshot
GET /control/v1/tenants/{tenant_id}/gateway-api-keys/{api_key_id}/effective-policy
GET /control/v1/tenants/{tenant_id}/gateway-api-keys/{api_key_id}/quota-snapshot
```

## Usage event contract

The Gateway outbox publishes a versioned `UsageRecorded` event containing:

```text
event_id
schema_version
tenant_id
api_key_id
response_id
attempt_id
route_id
provider
public_model
provider_model
region
price_snapshot_id
input_tokens
cached_input_tokens
cache_write_input_tokens
output_tokens
amount_micros
currency
outcome
occurred_at
```

Cache refresh and correction events use distinct event types. Events contain no request input, output, metadata, Provider error text, raw Provider credential, raw Gateway API Key, or Human IAM token.

The Metering inbox deduplicates by `event_id` and rejects incompatible schema versions. Projection offsets and lag are observable. At-least-once transport must produce exactly-once projection effects.

## Query semantics

Queries are Tenant scoped unless the verified actor has a platform-wide Metering scope. Stable cursor pagination is required for event lists. Time-series queries accept bounded intervals and explicit granularity. Supported dimensions include Tenant, Gateway API Key, Provider, public model, Provider model, Model Route, outcome, currency, and time.

Currency totals are never combined without an explicit conversion snapshot. Provider cost and future Customer Price are separate measures.

Quota snapshots distinguish committed, reserved, uncertain, and remaining capacity. They include the Tenant and API Key Policy revisions used for the calculation. There is no generic endpoint that resets counters. Limit changes use Policy publication; financial credits use a future append-only Credit Ledger.

## Projection rebuild and correction

Metering projections are disposable and rebuildable from authoritative Usage events or a controlled ledger export. Rebuild runs as a fenced operation with progress and a new projection generation; readers switch generations only after completion.

Original Usage Ledger rows are immutable. Provider reconciliation or operator correction writes an append-only `UsageCorrected` fact that references the original usage identity and reason. Metering computes the net view without rewriting history.

## Exports and retention

Exports are asynchronous, Tenant scoped, immutable for their requested cutoff, and stored in regional object storage. The first format is CSV; Parquet may follow for high-volume customers. Signed download URLs are short-lived.

Financial facts follow the configured legal retention period. Prompt/Response retention settings do not delete immutable content-free billing evidence. Conversely, Usage events must not become a path for retaining content from `store:false` Responses.

## Outbox relay

Implement a reusable, leased outbox relay that claims unpublished rows, publishes with an idempotency identity, and marks success. Publication uncertainty causes safe retry with the same event identity. Poison events become visible and block only their partition or configured aggregate, not inference writes.

## Consequences

- Inference remains authoritative and transactional for usage and quota.
- Metering can scale independently and be rebuilt without altering financial facts.
- Usage dashboards become eventually consistent and must display their data cutoff.
- A transport and schema compatibility discipline become production dependencies.

## Rejected alternatives

### Write synchronously to Billing from the Gateway

Rejected. Billing latency or outage would become Provider execution latency or outage and would create a distributed transaction.

### Query the inference database directly from the Console

Rejected. It bypasses Tenant authorization and lets reporting load contend with inference.

### Edit Usage Ledger rows to correct a bill

Rejected. Corrections must remain attributable and reconstructable.

## Acceptance criteria

- Gateway completion, Usage Ledger, Quota settlement, and outbox insertion remain one PostgreSQL transaction.
- Outbox and inbox tests prove duplicate-safe delivery and recovery after ambiguous publication.
- Metering rebuild produces the same projections as uninterrupted consumption.
- Query tests prove Tenant isolation, stable pagination, explicit currencies, and bounded time ranges.
- Quota snapshots reconcile committed, reserved, uncertain, and remaining values.
- `store:false` black-box tests prove no content enters usage events, projections, or exports.
- Metering outage does not prevent inference while durable outbox capacity remains available.

## Implementation status (2026-08-30)

The proposed design is implemented with a separate `cmd/metering` process,
content-free versioned Response, Stage A capability, and Cache Refresh usage
events, stable logical usage identities, leased outbox claims, inbox
deduplication, append-only corrections with actor and reason attribution, and
generation-fenced daily projections. Tenant-scoped summary, bounded
time-series, stable-cursor event, Tenant/key alias, Response usage, asynchronous
CSV export, signed download, rebuild, and operational-status interfaces are
present. The control plane exposes effective Policy and Quota Snapshot queries
that separate committed, reserved, uncertain, and remaining capacity.

Response, capability, and Cache Refresh completion now commit immutable usage,
the exact typed Quota Reservation settlement, and the usage outbox event in one
PostgreSQL transaction. Controlled ledger bootstrap is a one-shot operator mode;
the least-privilege Metering runtime role is explicitly denied authoritative
Usage Ledger and Response access. Duplicate delivery, ambiguous
acknowledgement, bootstrap/live-event overlap, correction, rebuild equivalence,
Tenant isolation, quota atomicity, role isolation, and content-free export
boundaries have automated coverage. Production exports use a GCS bucket behind
the `ExportStore` port. Metering authenticates with Workload Identity, verifies
the bucket is in the expected single region, and uses the
`ifGenerationMatch=0` precondition so a live object cannot be overwritten.
Idempotent retries compare existing bytes and fail closed on mismatch; the
create-only filesystem adapter is development-only. Metering also emits a
separate content-free, HMAC-authenticated regional soft-state observation.
Central Operations stores it monotonically, exposes projection
cutoff/backlog/heartbeat state, and joins the latest same-region status into
Gateway summaries without adding an inference-time dependency.

This ADR remains proposed until the decision is explicitly accepted.
