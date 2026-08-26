# Project decisions

## Stable domain

- A **Tenant** is the customer organization and isolation identity.
- A **Gateway API Key** is a revocable workload credential owned by one Tenant.
- An **Authenticated Principal** contains current Tenant/key identity, Policy revisions, Home Region, and execution epoch.
- A **Limit Policy** is typed and revisioned; Tenant/key intersection always chooses the most restrictive value.
- A **Quota Reservation** is made before a Provider side effect and later settled from actual evidence.
- A **Usage Ledger** row is an immutable financial fact. A **Usage Projection** is rebuildable reporting state.
- A **Provider Connection** identifies one authorized Provider account or credential scope in a region.
- A **Model Route** is one concrete Provider/model/credential/region execution path.
- A **Routing Catalog** is one coherent versioned publication of Model Routes and related policy.
- Embeddings, moderation, and reranking are capability-specific interfaces. They share Authenticated Principal, Model Route, quota, price, and audit rules without becoming Response subtypes.
- Stage A capability execution holds a shared Tenant writer fence across each bounded Provider attempt and checks the epoch again before durable settlement; Home Region promotion takes the conflicting row lock and waits. Each Provider attempt owns a distinct durable reservation while one public operation ID joins the final usage fact.

## Ownership

```text
Human IAM
  Human identity, login, factors, login sessions, membership, coarse role/scope claims

Control Plane
  Tenant administration, Gateway API Key administration, Provider Connections,
  Routing Catalog publication, control audit, configuration outbox

Gateway Data Plane
  Local key verification projection, Responses, Conversations, Model routing,
  quota admission, Provider execution, Usage Ledger, cache and retention workers

Metering
  Content-free event inbox, query projections, exports, projection rebuild

Billing (future and optional)
  Customer Price Books, plans, subscriptions, credits, invoices, payments, redemption
```

Do not place Human login on the inference hot path. Do not make the Gateway synchronously call the control plane or Billing for each request.

## Consistency and failure rules

- Stateful writes execute in the Tenant's Home Region.
- Execution epoch and fencing prevent two regions from becoming valid writers.
- CAS protects mutable heads; restoration creates a new revision.
- Transactional outbox is written in the same transaction as the authoritative mutation.
- Consumers use inbox deduplication and monotonic aggregate revisions.
- A consumer continues with its last valid snapshot when a newer snapshot is absent or invalid.
- After visible Provider output, persist failure rather than silently choosing another route.
- A possibly started Provider side effect without complete usage evidence is `uncertain`; do not release its reserved financial capacity blindly.
- A Model Route advertises Stage A support only through explicit `native`, `translated`, or `unsupported` declarations. Provider inventory or text support never implies another capability.

## Security and evidence

- Raw Gateway API Keys are returned once and never persisted.
- Provider secrets live behind Secret Custody; management state stores a secret reference.
- Audit and operational events contain no prompt/Response content or raw credentials.
- `store:false` must not leak content through outbox, Metering, logs, or exports.
- Capability Usage Ledger records counts, dimensions, document totals, Provider usage, price, and spend only. It never stores vectors, moderation inputs/results, rerank queries, or documents.
- Local tests, remote commit state, CI, deployment version, traffic, health, and representative endpoint behavior are separate evidence surfaces.
