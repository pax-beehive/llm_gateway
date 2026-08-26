---
name: llm-gateway-architecture
description: Design or review the Universal LLM Gateway architecture, domain model, ADRs, module seams, and cross-service contracts. Use for architecture decisions and ADR work; do not use for routine implementation or local operations alone.
---

# LLM Gateway Architecture

Preserve the project's Responses-first domain and make architecture changes through explicit, reviewable decisions.

## Start here

1. Read [`CONTEXT.md`](../../../CONTEXT.md) before naming or changing a domain concept.
2. Read [the ADR index](references/adr-index.md) and then only the ADRs relevant to the request.
3. Read [project decisions](references/project-decisions.md) when the task changes a seam, data owner, consistency rule, or deployment relationship.

## Working rules

- Treat Chat Completions as an adapter over the canonical Responses runtime.
- Use `Conversation`, `Response Chain`, and `Continuation`; do not rename them to Session. `Realtime Connection` is allowed only for the Realtime transport described by ADR 0009.
- Keep PostgreSQL authoritative for Response state, access state, quota reservations, and immutable financial facts. Treat in-memory storage as explicit development/test mode.
- Put Provider behavior behind narrow capability-specific interfaces. Require native, translated, or unsupported behavior to be explicit.
- Preserve Home Region single-writer semantics, execution-epoch fencing, CAS revisions, and the rule that no transparent fallback occurs after the first visible Provider event.
- Keep metadata descriptive. Permissions, limits, lifecycle, Home Region, and execution epoch are typed behavior.
- Separate Human IAM, Gateway API Key authentication, control-plane writes, inference execution, Metering projections, and future Billing ownership.
- Prefer versioned publication and immutable history over mutable active configuration.
- Record a rejected alternative when it is plausible enough that a future maintainer may otherwise repeat the debate.

## ADR changes

- Continue numeric filenames under `docs/adr/`.
- Use `status: proposed` until the decision is explicitly accepted; implementation alone does not silently accept a decision.
- Include context, decision, invariants, consequences, rejected alternatives, and acceptance criteria when they materially apply.
- Link dependent ADRs and state whether the new decision extends, supersedes, or leaves them unchanged.
- Verify current source before describing an implementation as present. Distinguish designed, implemented, locally verified, and deployed.

Do not copy secrets, short-lived credentials, local `.env` values, or production identifiers into ADRs or skill references.
