---
status: accepted
---

# Persist access principals and reserve hard quotas before Provider side effects

Gateway bearer authentication is backed by PostgreSQL Tenant and Gateway API Key records, not process environment maps. Raw keys are never stored: the gateway looks up a peppered HMAC digest, then derives an Authenticated Principal from active Tenant and key state. Tenant and key policies are independently revisioned and intersect by taking the most restrictive limit; a key can narrow Tenant permission but cannot expand it. This makes revocation, suspension, expiry, policy changes, usage attribution, and delayed Cache Protection decisions consistent across gateway replicas.

Hard rate, token, response-spend, and refresh-spend limits use PostgreSQL Quota Reservations and window counters. Admission reserves the conservative maximum before any Provider side effect, while completion settles the reservation to immutable Usage Ledger evidence and updates rebuildable daily Usage Projections. Tenant scope is locked before key scope so concurrent replicas cannot oversubscribe either hierarchy. If execution becomes ambiguous or a process dies after the external side effect, reconciliation commits the reserved estimate as `uncertain` instead of releasing it; this deliberately prefers temporary under-utilization over exceeding a hard financial limit.

Environment-provided keys remain only an explicit, idempotent bootstrap path and development-memory fallback. Active Cache Protection remains off by default and records its Refresh Sponsor; workers re-read the current Tenant and key policies and reserve refresh spend before calling a Provider. PostgreSQL is authoritative, while in-memory authentication and accounting are development-only.
