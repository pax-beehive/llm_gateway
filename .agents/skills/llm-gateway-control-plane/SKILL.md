---
name: llm-gateway-control-plane
description: Implement or review LLM Gateway Tenant, Gateway API Key, Provider Connection, Routing Catalog, Metering, audit, outbox, and projection work. Use for control-plane code and tests; exclude Human IAM implementation and routine operations-only tasks.
---

# LLM Gateway Control Plane

Implement control-plane work as complete vertical slices while keeping inference independent of remote control-plane availability.

## Select the slice

Read [`CONTEXT.md`](../../../CONTEXT.md), then use [the control-plane map](references/control-plane-map.md) to select the owning module and ADR. Read [known pitfalls](references/known-pitfalls.md) before changing access, event delivery, routes, usage, or secrets.

## Implementation rules

- Treat the relevant ADR acceptance criteria as the minimum slice, not optional follow-up.
- Keep commands idempotent and CAS-protected. Return stable domain error codes such as `revision_conflict`, `idempotency_conflict`, `not_found`, and `policy_denied`.
- Derive Actor Envelope from verified Human IAM claims, but do not implement passwords, OAuth, 2FA, Passkeys, or login sessions here.
- Write the authoritative mutation, append-only control audit, and control outbox in one PostgreSQL transaction.
- Use stable cursor pagination for lists. Do not expose unbounded scans or arbitrary metadata queries.
- Never return raw Gateway API Keys after issuance or Provider secrets from any read interface.
- Keep Gateway API Key verification local to the Gateway through a revisioned Access Projection. Do not add a per-request remote introspection hop.
- Publish coherent Routing Catalog snapshots; do not mutate active Model Routes one row at a time.
- Keep Usage Ledger creation and Quota settlement inside the Gateway transaction. Metering consumes content-free events asynchronously.
- Preserve explicit zero-denies and missing-inherits Limit Policy semantics.
- Treat Home Region and execution epoch as fenced behavior, never generic metadata.

## Testing

Use vertical red-green-refactor slices. Test through the owning module's interface and HTTP adapter, with PostgreSQL integration for transactions, CAS, outbox/inbox, quota, and projections.

Use deterministic adapters for Secret Custody, Provider probes, event transport, and clocks. Live Provider calls require explicit scope and should be narrow, low-cost smoke checks only.

For a completed slice, run the relevant unit and PostgreSQL integration packages, `make test-race`, `make vet`, `make build`, and a black-box HTTP scenario. Do not claim deployment or live Provider proof from local tests.

Do not add generic `channel`, `group`, `account`, or `session` concepts when Provider Connection, Plan/Entitlement, Tenant, Conversation, Response Chain, or Realtime Connection is the precise domain term.
