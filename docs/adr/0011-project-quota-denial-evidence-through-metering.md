---
status: proposed
---

# Project quota denial evidence through Metering

## Context

Quota snapshots explain current committed, reserved, uncertain, and remaining
capacity, but they do not explain an individual admission rejection. Operators
need content-free evidence keyed by Tenant, Gateway API Key, response or
capability operation, Model Route, policy revision, quota scope, and dimension.

Admission remains on the inference path. A remote call to Control Plane or
Metering while rejecting a request would add a new synchronous availability
dependency and could lose evidence whenever that dependency is unavailable.
Quota denials are operational facts, not billable Usage Ledger rows.

## Decision

The Gateway records a versioned `QuotaAdmissionDenied` event in its local
transactional outbox after quota admission fails and before returning the
rejection. The event is content-free and includes only operational identity,
quota scope and dimension, relevant policy revisions, and occurrence time.

Metering consumes `quota.denied` outbox rows with the existing leased,
at-least-once relay. It deduplicates by stable event identity, stores a separate
`metering_quota_denials` projection, and exposes:

```text
GET /metering/v1/quota-denials
```

The query supports Tenant, Gateway API Key, response, public model, Model Route,
scope, dimension, and time filters with stable descending cursor pagination.
Tenant actors are forced to their verified Tenant. Platform Metering actors may
omit `tenant_id` to query across Tenants.

Recording failures are surfaced together with the quota error so the Gateway
does not claim durable denial evidence that it failed to write. Metering remains
asynchronous and is never called by quota admission.

## Event contract

The event includes:

```text
event_id
schema_version
type
tenant_id
api_key_id
response_id
attempt_id
operation_id
capability
public_model
route_id
region
scope
dimension
currency
tenant_policy_revision
api_key_policy_revision
occurred_at
```

It has no prompt, response body, request metadata, provider error, credential,
raw Gateway API Key, or human identity token.

## Consequences

- Admission stays local to the Gateway and PostgreSQL authority.
- Operators can correlate rejection evidence with the exact policy revisions
  used at admission time.
- The denial view is eventually consistent and reports its data cutoff.
- A denial is not usage and never changes billing or quota counters.
- Static policy and concurrency rejections use the same evidence contract when
  the active quota controller implements the denial recorder interface.

## Rejected alternatives

### Write denial records synchronously to Control Plane or Metering

Rejected because an operations service outage would become an inference-path
dependency.

### Infer denial history from quota snapshots

Rejected because snapshots contain current aggregate state and cannot prove
which request, route, dimension, or policy revision caused a rejection.

### Store denials in the Usage Ledger

Rejected because a rejected admission has no billable provider usage and would
blur immutable financial facts with operational evidence.

## Acceptance criteria

- Every supported quota admission rejection writes a content-free stable event.
- Relay replay produces one denial projection row.
- Tenant authorization and platform aggregation tests prove scope isolation.
- Cursor tests prove deterministic ordering for equal timestamps.
- Inference performs no synchronous network call to Metering or Control Plane.
