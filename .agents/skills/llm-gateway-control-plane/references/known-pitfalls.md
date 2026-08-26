# Known control-plane pitfalls

## Access and secrets

- Do not confuse Human IAM tokens with Gateway API Keys. Human assertions authorize control-plane actors; Gateway API Keys authenticate inference workloads.
- Do not add synchronous control-plane introspection to the inference hot path. Publish a local Gateway Access Projection.
- Do not persist or later reveal raw Gateway API Keys. Ambiguous issuance is repaired by rotation.
- Do not put Provider secrets in Route JSON, events, audit, logs, test fixtures, or ordinary database columns. Store a Secret Custody reference.
- `digest_version` is not pepper rotation by itself. Authentication needs explicit multi-version support and a safe retirement gate.
- `last_used_at` is an operational projection, not an immutable Usage fact; avoid a hot write for every request.

## Configuration and routing

- Provider inventory does not prove Gateway capability. Discovery never auto-publishes a Model Route.
- Configured administrative state and observed health are different. A failed probe must not erase operator intent.
- Avoid active Route CRUD. Publish a complete validated Routing Catalog revision and restore by creating another revision.
- Preserve Tenant visibility and Home Region filtering when listing models.
- Preserve provider pinning after the first visible event; fallback is allowed only before visibility and within declared Policy.

## Transactions and events

- An outbox table without a relay, inbox deduplication, schema versioning, lag, and repair path is not event delivery.
- Write mutation, audit, and outbox atomically. Never publish to the broker before commit.
- Retry ambiguous publication using the same event identity.
- Projection consumers must reject stale revisions, detect gaps, and retain the last valid snapshot.

## Usage and money

- Usage Ledger is authoritative; dashboards and daily aggregates are projections.
- Do not synchronously write Billing from Provider execution.
- Do not edit Usage Ledger rows for corrections; append a correction fact.
- Do not sum different currencies without a conversion snapshot.
- Provider cost is not Customer price. Preserve separate immutable evidence.
- A possibly started Provider side effect stays financially uncertain; do not release its reservation blindly.
- Capability fallback attempts need distinct Quota Reservation operation identities. Reusing the public operation ID conflicts with durable uniqueness and prevents safe pre-side-effect fallback; link the successful Usage Ledger fact by reservation ID.
- A paid capability result without Provider usage evidence is uncertain, not zero-cost. Reranking also retains content-free Provider token evidence even when document count is the billing unit.
- Capability catalogs and route selection must apply the same Tenant allowlist and Home Region filters. Filtering only the catalog or only execution either leaks inventory or creates a time-of-check mismatch.
- A plain epoch read is not a concurrent fence. Stage A holds a shared Tenant row lock across the bounded Provider attempt, and Home Region promotion must update that same row so it cannot overtake in-flight work.
- Do not add Stage B or later capabilities by copying the full Stage A reserve, fence, execute, classify, evidence, ledger, and settle pipeline again. Extract an attempt coordinator with capability-specific estimate, validation, and pricing hooks first.

## Domain language

- Metadata is descriptive and cannot carry limits, permissions, Home Region, lifecycle, or execution epoch.
- Avoid generic New API terms when the project has a precise term: use Provider Connection instead of channel, Tenant instead of account, and Plan/Entitlement or Routing Policy instead of group.
- Conversation and Response Chain are not Session. Realtime Connection is a separate transport lifecycle.
