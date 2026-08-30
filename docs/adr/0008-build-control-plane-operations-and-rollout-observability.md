---
status: proposed
---

# Build control-plane Operations around readiness, revision, and lag

## Context

The current process exposes only `/healthz`, starts workers inside the Gateway, and logs degraded coordination. Versioned configuration, transactional outbox rows, Quota reconciliation, Cache Protection work, retention scrubbing, and OpenTelemetry exist, but operators cannot query readiness, active revisions, propagation lag, worker progress, regional rollout state, or schema compatibility through a stable interface.

Microservice separation adds failure modes that a liveness endpoint cannot explain. Operations must distinguish a process that is alive from one that is ready for new traffic, and it must prove whether control-plane changes have reached each Gateway region.

## Decision

This ADR extends ADR 0004 and ADR 0006 with readiness, machine identity,
durable receipt ingestion, external control-event distribution, and operator
query surfaces. Their assignment of the external relay to ADR 0008 remains
unchanged. It does not change Tenant, credential, Routing Catalog, Provider
Connection, or Metering ownership.

Create an Operations module that projects operational state from owned heartbeats, rollout receipts, outbox/inbox offsets, worker leases, and database checks. It is read-oriented and does not become a generic workflow engine.

Process-local probes are:

```text
GET /healthz
GET /readyz
```

Authenticated control-plane queries are:

```text
GET /control/v1/operations/gateways
GET /control/v1/operations/gateways/{gateway_id}
GET /control/v1/operations/publications
GET /control/v1/operations/publications/{publication_id}
GET /control/v1/operations/outbox
GET /control/v1/operations/consumers
GET /control/v1/operations/jobs
GET /control/v1/operations/jobs/{job_id}
```

## Liveness and readiness

`/healthz` proves only that the process event loop can answer HTTP. It performs no remote dependency checks.

`/readyz` is bounded and proves the process can safely accept its intended traffic. Gateway readiness requires:

- authoritative store connectivity;
- supported schema version;
- a valid Routing Catalog snapshot;
- an initialized Gateway Access Projection;
- Home Region configuration for stateful writes;
- no known execution-epoch fence violation.

Readiness does not call Providers, Human IAM, Billing, or the control plane. Their outages must not create a readiness cascade.

Control-plane readiness requires its database, Secret Custody configuration for secret mutations, outbox capacity, and supported schema. Metering readiness requires its inbox and active projection generation.

## Gateway heartbeat and rollout receipt

Each Gateway periodically reports through an authenticated owned-system interface:

```text
gateway_id
region
build_sha
schema_version
routing_catalog_revision
access_projection_revision
execution_epoch_floor
last_usage_outbox_id
started_at
observed_at
```

Heartbeats are soft state and never authorize execution. A missing heartbeat marks the instance unknown or stale; it does not mutate Tenant or route configuration.

Routing and access consumers emit durable rollout receipts when they apply or reject a revision. Operations reports desired revision, applied revision, last success, error code, and lag per region and instance.

## Lag and backlog signals

Expose at least:

```text
oldest_unpublished_outbox_age
outbox_pending_count
consumer_last_event_id
consumer_lag_seconds
routing_revision_lag
access_revision_lag
key_revocation_propagation_seconds
quota_reconciliation_backlog
cache_refresh_due_backlog
retention_scrub_backlog
metering_projection_cutoff
```

Cardinality-heavy detail belongs in bounded queries or logs, not unrestricted metric labels.

## Asynchronous operation resources

Provider probes, model discovery, Metering rebuilds, exports, and controlled cleanup use a shared operation resource shape:

```text
id
kind
requested_by
tenant_id(optional)
status
progress
result_ref
error_code
created_at
started_at
finished_at
```

The shape is shared, but each owning module implements its workflow and retry rules. Operations lists and observes jobs; it does not execute arbitrary user-defined graphs.

Retries are explicit and bounded. Ambiguous Provider or payment side effects retain their domain-specific uncertain state and are never retried merely because a generic job runner timed out.

## Logging and telemetry

Structured logs include request ID, trace ID, Tenant ID when authorized, aggregate identity, operation kind, and stable error code. They never include prompt content by default, raw Gateway API Keys, Provider credentials, Human IAM tokens, or unredacted upstream bodies.

OpenTelemetry remains the metric and trace export seam. The Operations interface provides authoritative revision and backlog state that cannot be inferred safely from traces alone.

## Deployment gates

Schema migration is a separate deployment gate. A binary reports the schema range it supports and refuses readiness when the database is outside that range. Rolling deployments prove that old and new consumers can process the event schema versions present during the rollout.

Observation-schema rollout is control-plane first: deploy a consumer that
accepts both the old and new versions, prove both over the authenticated HTTP
boundary, and only then deploy Gateways that emit the new version. Gateway-first
rollout is rejected because old strict decoders cannot safely ignore new fields.

## Consequences

- Operators can distinguish process health, dependency readiness, and propagation progress.
- Control-plane outages do not automatically evict healthy Gateways.
- Operational state needs bounded retention and authenticated machine identity.
- A shared job resource improves observability without centralizing domain retry logic.

## Rejected alternatives

### Make `/healthz` check every dependency

Rejected. A Provider, IAM, Billing, or control-plane outage would create cascading restarts and hide the actual dependency failure.

### Use logs as proof that a revision deployed

Rejected. Logs are observations; rollout receipts and active revision state are the proof surface.

### Build a universal workflow engine now

Rejected. The current operations need bounded domain workers, leases, and receipts, not arbitrary orchestration.

## Acceptance criteria

- Liveness and readiness have deterministic tests for every dependency state and strict time bounds.
- Gateway inference remains ready during a control-plane or Metering outage when local snapshots and durable outbox capacity are healthy.
- Operations queries show desired and applied routing/access revisions per region.
- Duplicate heartbeats and rollout receipts are idempotent; stale observations cannot move a revision backward.
- Outbox, consumer, reconciliation, refresh, and retention lag have both query and OpenTelemetry coverage.
- Logs and operation results pass secret/content redaction tests.
- A rolling-version integration test exercises compatible schema and event-version transitions.

## Implementation status (2026-08-29)

The proposed design is implemented with bounded `/readyz` probes, schema-range
gates, machine-authenticated Gateway observations, monotonic heartbeats,
externally promoted Routing Catalog receipts, durable Access apply/reject
receipts with a global delivery sequence, and authenticated control-plane
queries for Gateways, publications, outbox state, consumers, and Provider-owned
jobs. Gateway observations report build/schema identity, desired-versus-applied
routing and access revisions, pending-work consumer lag, revocation propagation, durable
outbox age, and quota/cache/retention backlogs. The production control plane no
longer trusts the shared PostgreSQL Gateway inbox as rollout authority; its
collector is development compatibility only. OpenTelemetry histograms cover
heartbeat, revision, consumer, outbox-age, revocation, and bounded backlog
signals. Metering cutoff is explicitly reported as unavailable until ADR 0007
has an active projection; Operations does not manufacture a cutoff from Usage
Ledger writes.

The existing shared-PostgreSQL control-event reader remains a development
compatibility transport for Access and Routing Catalog delivery. The encrypted
external relay and local Provider Connection execution projection are still
required before this ADR is implementation-complete and before physical
control/data-plane database separation; Operations does not turn its heartbeat
interface into a generic event bus. This ADR remains proposed until those gates
pass and the decision is explicitly accepted.
