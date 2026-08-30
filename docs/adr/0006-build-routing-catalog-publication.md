---
status: proposed
---

# Publish the Routing Catalog through validated immutable revisions

## Context

The Gateway already stores versioned `model_routes` configuration and atomically swaps an in-process routing snapshot. Publications use compare-and-swap revisions, but operators can publish only through startup environment variables. The stored value is one JSON blob with no draft, validation report, Provider probe, rollout receipt, Tenant visibility rule, or safe restore workflow.

Direct CRUD on active Model Routes would make partial configuration visible and would spread cross-route validation across callers. A Routing Catalog is one coherent publication because model aliases, Provider Connections, capability declarations, prices, regions, and fallback order must agree at one revision.

## Decision

Create a Routing Catalog Publication module in the control plane. Its small interface accepts a complete candidate catalog, validates it, and publishes an immutable revision. Draft editing is a control-plane convenience; the Gateway consumes only published snapshots.

The first HTTP interface is:

```text
GET    /control/v1/routing-catalog
GET    /control/v1/routing-catalog/revisions
GET    /control/v1/routing-catalog/revisions/{revision}
POST   /control/v1/routing-catalog/drafts
GET    /control/v1/routing-catalog/drafts/{draft_id}
PUT    /control/v1/routing-catalog/drafts/{draft_id}
POST   /control/v1/routing-catalog/drafts/{draft_id}/validate
POST   /control/v1/routing-catalog/drafts/{draft_id}/probe
POST   /control/v1/routing-catalog/drafts/{draft_id}/publish
GET    /control/v1/routing-publications/{publication_id}
POST   /control/v1/routing-catalog/revisions/{revision}/restore
```

## Publication lifecycle

```text
draft -> validated -> probed(optional) -> published -> rolling_out -> active
                                                \-> partially_applied
                                                \-> failed
```

Validation is deterministic and side-effect free. Probe is optional, separately authorized, asynchronous, and budgeted. Publish requires the draft's base revision to equal the current head and creates exactly one new immutable revision. A stale base returns `409 revision_conflict` with the active revision.

Restoring a prior catalog copies its content into a new revision. History never moves backward and no published revision is mutated.

## Catalog content

Each Model Route references a Provider Connection rather than embedding a raw Base URL or secret environment variable. It includes:

```text
route_id
public_model
provider_connection_id
provider_model
execution_region
home_region
capability_profile_revision
provider_cost_snapshot
administrative_status
selection_policy
tenant_visibility_policy
cache_usage_reliability
cache_protection_policy
```

Selection policy may declare priority, weight within a priority, maximum concurrency, and sticky-routing eligibility. Cost remains an input to routing but is not the only ordering rule. Tenant visibility is explicit; an authenticated Tenant never sees a public model solely because some unrelated Tenant has a valid route.

Administrative status and observed health remain separate. `draining` stops new assignments but lets pinned or already visible Responses finish. After the first visible Provider event, the existing rule still prohibits transparent fallback.

Provider Connection enabled/disabled state, observed health, and immutable
credential rotation are operational overlays owned by ADR 0005. Their events may
recompile the executor/eligibility projection at the same Catalog revision, but
they do not mutate the published Catalog document or its validation evidence.

## Validation report

Validation checks at least:

- unique Route IDs and unambiguous public model mappings;
- existing enabled Provider Connections;
- accepted Provider identities and conformance-tested adapters;
- native/translated/unsupported Capability Profile values;
- Home Region and execution-region compatibility;
- finite non-negative immutable Provider prices;
- credential scope and cache-anchor consistency;
- no active Cache Protection on an unapproved adapter;
- Tenant visibility and Limit Policy references;
- at least one eligible route for every advertised public model.

Warnings and errors are distinct. Publish rejects any error and records the exact validation report hash.

## Distribution and rollout receipts

The control plane writes `RoutingCatalogPublished` to its outbox. Gateways apply revisions monotonically, persist an inbox receipt, validate the snapshot locally, and atomically swap only after full success. They report:

```text
gateway_id
region
publication_id
catalog_revision
status
applied_at
lag_milliseconds
error_code
```

The control plane declares a publication active only after its configured regional quorum reports the revision. Gateway operation continues on the last valid snapshot while a newer publication is missing or invalid.

The initial implementation uses the shared PostgreSQL outbox/inbox transport and
keys inbox delivery by `gateway_id,event_id`, so multiple Gateway instances do
not consume one another's receipt. A Gateway writes only its projection inbox;
a control-plane collector validates and promotes that observation into the
authoritative rollout receipt and publication status. ADR 0008 replaces this
transport and Gateway-identity boundary before physical control-plane and
regional databases are separated.

## Provider prices

Provider Cost Snapshot remains immutable execution evidence. Customer Price Books, margins, subscriptions, and invoices are separate billing concerns and are not inferred from Provider cost.

## Consequences

- Operators publish coherent routing state instead of partially editing active rows.
- Gateways remain available when the control plane or distribution transport is unavailable.
- Rollout state becomes observable per region.
- Draft and validation storage adds workflow complexity but contains it behind one publication interface.

## Rejected alternatives

### Immediate CRUD on active routes

Rejected. Cross-route invariants and rollback would become caller responsibilities.

### Let each Gateway independently discover and publish models

Rejected. It creates regional drift and bypasses capability, price, Tenant, and audit decisions.

### Treat restore as moving the head to an old revision

Rejected. It makes change history non-monotonic and complicates consumer fencing.

## Acceptance criteria

- Deterministic tests cover all validation rules without Provider network calls.
- Concurrent publication from the same base permits exactly one winner.
- Restore creates a new revision with traceable source revision.
- Gateway consumers are duplicate-safe, reject stale or invalid snapshots, and continue on the last valid revision.
- Rollout receipts expose regional success, failure, and lag.
- Tenant model listing honors the published visibility and Home Region rules.
- Streaming tests retain provider pinning after the first visible event.

## Implementation status (2026-08-29)

The proposed design is implemented behind `GATEWAY_ROUTING_CATALOG=true` with
control-plane drafts, deterministic validation, delegated asynchronous probes,
immutable CAS publication and restore, Gateway compilation through Provider
Connection Secret Custody, monotonic per-Gateway inboxes, regional rollout
receipts, and last-valid-snapshot behavior. Selection priority, stable weighted
ordering, maximum concurrency, explicit visibility, active/draining/disabled
administrative state, and observed health are enforced at runtime. Unit,
PostgreSQL integration, and control-plane black-box coverage exercise these
boundaries. The ADR remains proposed until the decision is explicitly accepted;
the external distribution/readiness work remains owned by ADR 0008.
