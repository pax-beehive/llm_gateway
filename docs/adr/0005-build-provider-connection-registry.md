---
status: proposed
---

# Build a Provider Connection registry with external secret custody

## Implementation status

Implemented in the control-plane process. PostgreSQL owns Provider Connection,
credential-version, asynchronous operation, observed-health, and model-observation
state. Development uses deterministic in-memory Secret Custody and Provider
operators; production requires GCP Secret Manager through Workload Identity.
Registration, profile changes, enable/disable, and completed rotation are
CAS-protected, idempotent, audited, and published through the transactional
outbox. Probe, discovery, and rotation requests return durable operation
resources and are processed through recoverable leases.

Management resources, audit, outbox, operation results, and observations never
contain secret material or Secret Manager references. Discovery persists a hash
of the bounded raw Provider response and cannot mutate the existing routing
configuration. An internal resolver can retrieve the active credential for an
enabled Provider Connection; ADR 0006 remains responsible for publishing a
Model Route that references the connection. The ADR remains `proposed` until it
is explicitly accepted.

## Context

Provider configuration currently lives inside a process-level route JSON document. A route combines Provider identity, Base URL, API key environment-variable name, credential scope, region, capabilities, health flag, model mapping, price, and Cache Protection configuration. This is sufficient for controlled bootstrap but cannot safely support a management Console, credential rotation, multiple credentials, connection testing, upstream model discovery, or audited changes.

The product uses the domain term Provider for an upstream model vendor or inference platform and Model Route for a concrete execution path. The generic New API term `channel` is not introduced.

## Decision

Create a Provider Connection Registry module in the control plane. A Provider Connection represents one authorized upstream account or credential scope in one operational region. It does not itself make a public model available; Model Routes published under ADR 0006 reference it.

The first HTTP interface is:

```text
POST   /control/v1/provider-connections
GET    /control/v1/provider-connections
GET    /control/v1/provider-connections/{connection_id}
PATCH  /control/v1/provider-connections/{connection_id}
POST   /control/v1/provider-connections/{connection_id}/enable
POST   /control/v1/provider-connections/{connection_id}/disable
POST   /control/v1/provider-connections/{connection_id}/probes
POST   /control/v1/provider-connections/{connection_id}/model-discoveries
POST   /control/v1/provider-connections/{connection_id}/credential-rotations
GET    /control/v1/provider-operations/{operation_id}
```

Provider probes, discovery, and credential rotation are asynchronous operations and return `202 Accepted` with an operation resource.

## Provider Connection model

```text
id
provider
display_name
base_url
region
credential_scope
secret_ref
administrative_status
capability_declaration
revision
created_at
updated_at
```

The initial accepted Provider identities remain `openai`, `deepseek`, `anthropic`, and `gemini`. Adding another identity requires a provider adapter, Capability Profile, offline conformance suite, and explicit publication support; creating a registry row does not bypass that gate.

Administrative status is `enabled` or `disabled`. Observed health is a separate projection derived from probes and real attempts. A transient probe failure never rewrites administrative intent. Routing consumes both configured eligibility and observed health policy.

## Secret custody

Define a narrow Secret Custody port with production and test adapters. Production uses an approved Secret Manager or KMS-backed store; tests use an in-memory adapter. The control-plane database stores only `secret_ref`, secret version metadata, and non-sensitive display information.

Provider secrets are never returned by list or get interfaces, written to audit events, included in outbox payloads, logged, or stored in Model Route documents. Credential creation and rotation accept secret material only over the sensitive mutation path and pass it directly to Secret Custody.

The Gateway receives only the execution credential material or short-lived reference it needs through an authenticated secret-delivery mechanism. The specific cloud adapter is not part of the module interface.

## Probe and model discovery

A probe uses the cheapest provider-supported authenticated operation that establishes credential validity and basic connectivity. It has a strict timeout, bounded response body, redacted error classification, and no automatic retry after an ambiguous billable operation.

Model discovery records the provider-reported inventory as an observation with connection ID, provider model ID, observed capabilities when trustworthy, timestamp, and raw-response hash. It does not automatically publish Model Routes or declare Provider behavior native. An operator or later policy compares discoveries with the routing draft.

Production live probes are explicitly authorized and budgeted. Offline adapter conformance remains the normal test path.

## Multiple credentials

A Provider Connection may have multiple credential versions for rotation. General load balancing across unrelated accounts uses multiple Provider Connections and Model Routes rather than a hidden list of keys. This preserves attribution, health, regional policy, and cost evidence per credential scope.

## Events and audit

Mutations atomically write audit and outbox records. Initial events are:

```text
ProviderConnectionRegistered
ProviderConnectionChanged
ProviderConnectionEnabled
ProviderConnectionDisabled
ProviderCredentialRotated
ProviderModelsObserved
```

Events carry no secret value.

## Consequences

- Provider credentials stop leaking into route configuration and process environment conventions.
- Discovery becomes evidence for route editing rather than implicit publication.
- Health, administrative status, and credential validity remain separate concepts.
- The system needs a Secret Manager adapter and a secure Gateway delivery path before environment credentials can be retired.

## Rejected alternatives

### Continue publishing `api_key_env` in Route JSON

Rejected. It cannot support safe Console operations, rotation, multi-region custody, or auditable secret ownership.

### Publish discovered Provider models automatically

Rejected. Provider inventory does not prove the Gateway's native/translated behavior, pricing, regional eligibility, or Tenant visibility.

### Hide multiple raw keys inside one connection

Rejected. It destroys per-credential health, usage attribution, and routing clarity.

## Acceptance criteria

- The database contains no Provider secret plaintext.
- Create, update, enable, disable, and rotation are CAS-protected and audited.
- Fake Secret Custody and fake Provider adapters cover all control-plane tests without network spend.
- Probe output is bounded and redacted, and live probes require explicit authorization.
- Discovery observations cannot become active Model Routes without an ADR 0006 publication.
- A Gateway can resolve a published Provider Connection without exposing the secret through management interfaces or events.
