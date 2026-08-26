# Operational pitfalls

- A green unit test does not prove PostgreSQL transactions, CAS, quota concurrency, outbox, or HTTP behavior. Run integration gates.
- A clean build does not prove the running binary, deployment revision, traffic routing, or Provider credentials.
- `/healthz` is liveness, not complete readiness or release proof.
- Memory storage is disposable development mode and must not be described as production persistence.
- The normal Go cache may be unwritable under sandboxing; use the Makefile's `/tmp` cache rather than changing product code.
- A missing Python `openai` package is a black-box environment issue. Use a temporary environment rather than adding an unnecessary repository dependency.
- Fixed temporary paths make repeatable Codex black boxes fail after prior runs; use unique `mktemp` templates.
- Every Codex resume must retain `workspace-write`; a resumed run can otherwise become read-only.
- Do not print `.env` to diagnose Provider discovery. Check variable presence without exposing values.
- Direct Provider model discovery can cost or leak account metadata and does not prove behavioral conformance.
- Cache Protection modes are not ordinary feature flags. `shadow` records eligibility without refresh; the only current active path is a bounded Anthropic one-refresh canary.
- A Provider failure after visible output must be persisted and must not trigger transparent fallback.
- A Provider side effect with missing usage is uncertain, not free.
- A transactional outbox row is not published evidence until a relay and consumer receipt prove delivery.
- Do not infer upload, release, or deployment failure from one missing read path; verify the authoritative write/list surface and the active routing surface separately.
