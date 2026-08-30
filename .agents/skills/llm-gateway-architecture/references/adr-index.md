# ADR index

Read only the decisions relevant to the current task.

| ADR | Status | Decision | Read when |
| --- | --- | --- | --- |
| `docs/adr/0001-build-universal-llm-gateway-core.md` | accepted | Responses-first gateway core, Provider seams, Home Region, streaming, Cache Protection, Usage/Savings evidence | Changing canonical execution, routing, storage, streaming, cache, or Provider behavior |
| `docs/adr/0002-persistent-access-and-hard-quota-admission.md` | accepted | Persistent Tenant/Gateway API Key principals and hard hierarchical quota admission | Changing authentication, access Policy, quota, usage attribution, or refresh sponsorship |
| `docs/adr/0003-build-tenant-administration-control-plane.md` | proposed | Tenant Administration HTTP interface, audit, idempotency, and events | Implementing or reviewing Tenant control-plane work |
| `docs/adr/0004-build-gateway-api-key-administration-and-access-projection.md` | proposed | One-time key issuance, rotation, revocation, Policy, and local Gateway Access Projection | Implementing workload credential administration or splitting access storage |
| `docs/adr/0005-build-provider-connection-registry.md` | proposed | Provider Connection registry, external secret custody, probes, and discovery | Managing Provider accounts, credentials, health, or upstream inventory |
| `docs/adr/0006-build-routing-catalog-publication.md` | proposed | Draft, validate, publish, distribute, and observe immutable Routing Catalog revisions | Changing Model Routes, selection policy, Tenant visibility, or route rollout |
| `docs/adr/0007-build-usage-quota-and-metering-interfaces.md` | proposed | Content-free usage events, outbox/inbox, Metering projections, quota queries, and corrections | Building reporting, exports, Metering, quota status, or future Billing inputs |
| `docs/adr/0008-build-control-plane-operations-and-rollout-observability.md` | accepted | Liveness/readiness, heartbeats, rollout receipts, lag, jobs, and deployment gates | Building or diagnosing operations and release proof |
| `docs/adr/0009-expand-capability-specific-inference-interfaces.md` | proposed | Staged embeddings, moderation, rerank, files, batches, media, Realtime, and video interfaces | Adding a non-Responses inference capability |

When two ADRs overlap, preserve the stronger correctness invariant. Propose an explicit superseding ADR instead of quietly weakening an accepted decision.
