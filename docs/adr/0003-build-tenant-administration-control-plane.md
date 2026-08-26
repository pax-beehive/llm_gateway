---
status: proposed
---

# Build Tenant administration as a versioned control-plane module

## Context

ADR 0002 made PostgreSQL authoritative for Tenant lifecycle, Home Region, execution epoch, metadata, and revisioned Tenant Policy. The current `internal/access` implementation can create and retrieve one Tenant, transition its lifecycle, and publish a new Policy with compare-and-swap semantics. It does not provide a control-plane HTTP interface, list/search queries, general Tenant updates, durable command idempotency, complete audit evidence, or control-plane events.

Human authentication and login sessions belong to a separate Human IAM system. This ADR assumes that an authenticated control-plane request supplies a verified actor identity, active Tenant context, and scopes. It does not define passwords, OAuth, 2FA, Passkeys, or Human IAM membership storage.

## Decision

Create a Tenant Administration module in a separately deployable control-plane process. Keep it deep: callers submit Tenant commands and queries while the module owns lifecycle validation, CAS, idempotency, audit evidence, and event publication.

The first HTTP interface is:

```text
POST   /control/v1/tenants
GET    /control/v1/tenants
GET    /control/v1/tenants/{tenant_id}
PATCH  /control/v1/tenants/{tenant_id}
POST   /control/v1/tenants/{tenant_id}/transitions
GET    /control/v1/tenants/{tenant_id}/policy
PUT    /control/v1/tenants/{tenant_id}/policy
GET    /control/v1/tenants/{tenant_id}/policy-revisions
```

Every mutation accepts an `Idempotency-Key`, a verified Actor Envelope, and either an expected row revision or expected Policy revision. HTTP exposes row revisions through `ETag`; stale writes return `409 revision_conflict`. Creation replays the original result for the same actor, operation, idempotency key, and request hash, and rejects reuse with a different request hash.

`PATCH /tenants/{tenant_id}` may change descriptive fields such as display name and metadata. Permissions, limits, lifecycle, Home Region, and execution epoch are not metadata and cannot be changed through this endpoint.

Lifecycle transitions are explicit commands:

```json
{
  "target": "suspended",
  "expected_revision": 7,
  "reason": "operator-request"
}
```

Allowed transitions remain:

```text
active -> suspended
suspended -> active
active|suspended -> closed
```

A closed Tenant cannot be reopened. Closing or suspending a Tenant immediately prevents new Gateway API Key authentication and delayed Cache Protection work after the change reaches the Gateway Access Projection.

Home Region migration and execution-epoch promotion are not ordinary Tenant updates. They require a later fenced migration workflow and are deliberately excluded from this interface.

## Actor, audit, and events

The control plane derives an Actor Envelope from the Human IAM assertion:

```text
actor_type
actor_id
acting_tenant_id
scopes
request_id
reason
```

Every successful mutation writes the Tenant change, an append-only `control_audit_events` record, and a `control_outbox` record in one PostgreSQL transaction. A mutation fails if its required audit evidence cannot be stored.

Initial event types are:

```text
TenantProvisioned
TenantProfileChanged
TenantStatusChanged
TenantPolicyPublished
```

Events contain identifiers, revisions, lifecycle state, Policy or Policy digest as required by the consumer, and timestamps. They never contain Human IAM tokens, Gateway API Key secrets, Provider credentials, prompt content, or Response content.

## Query behavior

Tenant lists use stable cursor pagination and support exact ID/slug lookup plus filters for status and Home Region. Free-form metadata search is not part of the first release. Default pages exclude closed Tenants unless explicitly requested.

Policy history is immutable and ordered by revision. Restoring an earlier Policy creates a new revision whose content is based on the earlier revision; it never moves the Policy head backward.

## Storage and deployment

The initial control-plane process may share the existing PostgreSQL instance, but only the Tenant Administration database role may write Tenant profile, lifecycle, Policy history, audit, idempotency, and control outbox tables. The Gateway receives Tenant state through a local projection or a temporary read-only adapter while the projection migration is incomplete.

## Consequences

- Human IAM can evolve without owning the Gateway Tenant domain.
- Tenant changes become safe to automate and expose to a Console.
- Audit and event publication cannot drift from the authoritative change.
- Home Region migration remains an explicit future decision rather than an unsafe field update.
- The control plane requires an outbox relay and Gateway projection before physical database separation.

## Rejected alternatives

### Let the Console write Tenant tables

Rejected. It would duplicate lifecycle, CAS, audit, and event invariants in every caller.

### Put Tenant ownership in Human IAM

Rejected. Human membership and login are IAM concerns; Tenant traffic isolation, Home Region, execution epoch, Policy, Gateway credentials, and financial attribution are gateway control-plane concerns.

### Allow generic JSON patch for all fields

Rejected. It would make metadata capable of mutating behavior and would bypass lifecycle and fencing rules.

## Acceptance criteria

- PostgreSQL integration tests cover idempotent create, list pagination, metadata CAS, every legal and illegal lifecycle transition, Policy publication, and restoration as a new revision.
- Every mutation atomically creates one audit event and one deduplicated outbox event.
- Authorization tests prove that platform and Tenant scopes cannot cross Tenant identity accidentally.
- No endpoint can update Home Region or execution epoch as ordinary metadata.
- A black-box test provisions a Tenant, publishes a Policy, suspends it, and observes the versioned changes without direct database writes.
