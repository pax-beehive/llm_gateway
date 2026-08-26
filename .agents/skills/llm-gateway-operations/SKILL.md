---
name: llm-gateway-operations
description: Run, verify, diagnose, migrate, or operate the Universal LLM Gateway locally and in deployment workflows. Use for tests, Compose, health/readiness, Provider smoke checks, release evidence, and operational incidents; do not use for architecture-only work.
---

# LLM Gateway Operations

Operate the Gateway with explicit evidence and without exposing credentials or spending Provider budget accidentally.

## Route the task

- Read [the local verification runbook](references/local-verification.md) for build, test, Compose, SDK, Codex, or live Provider checks.
- Read [the operations runbook](references/operations-runbook.md) for migrations, readiness, configuration, lag, rollout, retention, quota reconciliation, or incident work.
- Read [operational pitfalls](references/operational-pitfalls.md) when a command fails or evidence appears contradictory.

## Operating rules

- Report local source SHA and worktree state separately from CI, deployment version, traffic, health, and representative endpoint results.
- Use PostgreSQL integration tests for persistence, quota, access, configuration, cache, and HTTP behavior. Memory mode is deterministic development/test mode only.
- Use `/healthz` only as liveness proof. Once ADR 0008 is implemented, use `/readyz` and active revision/lag queries for traffic readiness.
- Treat migrations as a deployment gate. Do not casually enable `GATEWAY_MIGRATE=true` on every production replica.
- Keep `GATEWAY_CACHE_PROTECTION_MODE=off` unless an explicitly scoped shadow or Anthropic canary operation is requested and its Policy/budget gates are present.
- Prefer offline Provider conformance. Run paid live smoke or tool tests only when explicitly requested and required credentials are provisioned.
- Never print `.env`, Provider keys, Gateway keys, bearer tokens, DSNs containing passwords, or short-lived secret-delivery material.
- Do not manufacture success by skipping missing integration prerequisites. `make test-integration` must fail when `TEST_DATABASE_URL` is absent.

When diagnosing, preserve the authoritative state and gather read-only evidence before restarting workers, changing Policy, republishing routes, or touching quota/usage records.
