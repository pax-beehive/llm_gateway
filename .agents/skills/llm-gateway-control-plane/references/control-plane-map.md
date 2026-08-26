# Control-plane map

## Deployables and modules

Do not create one network service per menu item. The initial topology is:

```text
cmd/control-plane
  Tenant Administration
  Gateway Credential Administration
  Provider Connection Registry
  Routing Catalog Publication
  Control Audit
  Control Outbox Relay

cmd/llm-gateway
  Public inference HTTP
  Gateway Access Projection
  Routing Catalog Projection
  Response Runtime and Store
  Quota, Usage Ledger, cache, retention

cmd/metering (introduced by ADR 0007)
  Usage inbox
  Query projections
  Exports and rebuild
```

The processes may initially share one PostgreSQL instance. Enforce schema/table write ownership with database roles before physical database separation.

## Slice routing

| Work | Owner | ADR | Existing foundation |
| --- | --- | --- | --- |
| Tenant list/profile/lifecycle/Policy | Tenant Administration | 0003 | `internal/access` Tenant create/get/transition/Policy |
| Key issue/list/rotate/revoke/Policy | Gateway Credential Administration | 0004 | `internal/access` import/get/revoke/Policy and HMAC lookup |
| Local workload authorization | Gateway Access Projection | 0004 | `access.Principal` and current authenticator |
| Provider account and secret | Provider Connection Registry | 0005 | Provider adapters and model discovery client |
| Route draft/validate/publish/rollout | Routing Catalog Publication | 0006 | versioned configuration repository and atomic router snapshot |
| Quota status and usage queries | Gateway plus Metering | 0007 | Quota Controller, Usage Ledger, daily projections |
| Readiness, revision, lag, jobs | Operations projection | 0008 | healthz, OpenTelemetry, workers, outbox tables |
| Non-text inference capability | Gateway capability module | 0009 | Provider seams and Capability Profile vocabulary |

## Cross-process ports

Introduce a port only where production and test adapters both exist.

- `IdentityAssertionVerifier`: production JWT/JWKS adapter and deterministic test signer.
- `SecretCustody`: production Secret Manager adapter and in-memory test adapter.
- `ControlEventPublisher`: production transport adapter and in-memory test adapter; source remains transactional outbox.
- `ControlEventConsumer`: production transport adapter and in-memory test adapter; effects remain inbox-deduplicated.
- Capability executor ports from ADR 0009: one production Provider adapter and one deterministic test adapter before public exposure.

Do not expose PostgreSQL, a message broker, or a cloud Secret Manager in a domain interface.

## Mutation envelope

Every control mutation carries:

```text
actor_id
actor_type
acting_tenant_id
scopes
request_id
reason
idempotency_key
expected_revision (when updating)
```

Persist only the fields needed for audit and idempotency. Never persist the Human IAM bearer token.

## Event envelope

```text
event_id
schema_version
aggregate_type
aggregate_id
aggregate_revision
tenant_id (when scoped)
event_type
occurred_at
payload
```

Consumers deduplicate by `event_id`, apply aggregate revisions monotonically, and expose gaps and lag to Operations.
