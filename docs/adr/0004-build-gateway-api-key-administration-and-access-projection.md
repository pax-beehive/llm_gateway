---
status: proposed
---

# Build Gateway API Key administration and a local Gateway Access Projection

## Implementation status

Implemented locally as of 2026-08-27 while this ADR remains proposed pending
explicit architectural acceptance. The control plane implements the complete
HTTP lifecycle and policy surface, transactional audit/outbox writes, one-time
issue/rotation secrets, grace reconciliation, snapshot construction, and digest
retirement counts. Gateways consume schema-version-2 events into a local
PostgreSQL projection with inbox deduplication, monotonic aggregate revisions,
gap detection, atomic snapshot replacement, structured lag/gap status, and
cross-replica API-key concurrency leases. Production authentication is gated on
the projection and does not read authoritative access tables. A bounded repair
command copies a repeatable-read authoritative Tenant snapshot into one regional
projection without exposing key material or deleting active concurrency leases.
Successful authentication is coalesced into an asynchronous regional
`last_used_at` projection. Production startup gates missing active digest pepper
versions and requires an authenticated-encrypted database transport attestation.

Production delivery now uses ADR 0008's authenticated HTTPS Control Event relay
and a durable Gateway-local cursor; the shared immutable-outbox reader remains a
development compatibility adapter. A Gateway behind retained history replaces
the complete regional Access Projection from ADR 0008's authenticated bootstrap
before acknowledging its source cursor. Durable apply/reject receipts, alerts,
and readiness are owned by ADR 0008.

## Context

ADR 0002 persists peppered HMAC digests for Gateway API Keys and supports import, lookup, expiry, revocation, metadata CAS, and revisioned API Key Policy. The current path is an explicit environment bootstrap: it accepts caller-generated raw keys and exposes no administration interface. It has no key list, server-side issuance, rotation workflow, effective-policy query, digest-pepper migration, or distributed access projection.

Moving control-plane writes away from the Gateway must not add a synchronous remote authorization call to every inference request. Human IAM credentials and Gateway API Keys are distinct: Human IAM authenticates people using the Console, while Gateway API Keys authenticate workloads invoking `/v1/*` inference interfaces.

## Decision

Create a Gateway Credential Administration module in the control plane and a read-optimized Gateway Access Projection in every Gateway deployment.

The control-plane HTTP interface is:

```text
POST   /control/v1/tenants/{tenant_id}/gateway-api-keys
GET    /control/v1/tenants/{tenant_id}/gateway-api-keys
GET    /control/v1/tenants/{tenant_id}/gateway-api-keys/{api_key_id}
PATCH  /control/v1/tenants/{tenant_id}/gateway-api-keys/{api_key_id}
POST   /control/v1/tenants/{tenant_id}/gateway-api-keys/{api_key_id}/revoke
POST   /control/v1/tenants/{tenant_id}/gateway-api-keys/{api_key_id}/rotate
GET    /control/v1/tenants/{tenant_id}/gateway-api-keys/{api_key_id}/policy
PUT    /control/v1/tenants/{tenant_id}/gateway-api-keys/{api_key_id}/policy
GET    /control/v1/tenants/{tenant_id}/gateway-api-keys/{api_key_id}/policy-revisions
GET    /control/v1/tenants/{tenant_id}/gateway-api-keys/{api_key_id}/effective-policy
```

## Key issuance

The control plane generates at least 256 bits of cryptographically secure random secret material. The response returns the raw key exactly once. PostgreSQL stores only its display prefix, peppered digest, digest version, status, expiry, metadata, and Policy identity.

Issuance is idempotent. Replaying the same `Idempotency-Key` returns the same resource metadata but never returns the raw secret again after the original successful response. If delivery of the original response is ambiguous, the operator issues or rotates another key rather than retrieving stored plaintext.

`PATCH` may change name, descriptive metadata, and expiry using row CAS. It cannot reactivate a revoked key or expand a key beyond the current Tenant Policy.

## Rotation and revocation

Rotation is one command that creates a replacement key linked to its predecessor. The caller chooses immediate revocation or a finite grace deadline. The raw replacement key is returned exactly once. A reconciliation worker revokes a predecessor whose grace deadline expires.

Revocation is terminal and idempotent. Administrative deletion is not exposed because historical Usage Ledger and audit evidence continue to reference the key identity.

## Key Policy

Extend API Key Policy with explicit optional restrictions:

```text
allowed_public_models
allowed_operations
allowed_cidrs
allowed_regions
max_concurrent_responses
```

Missing restrictions inherit from the Tenant. Explicit empty lists deny the corresponding capability. Existing quota and Cache Protection rules retain the most-restrictive Tenant/key intersection. CIDR checks use a trusted client-address policy configured at the Gateway edge; arbitrary forwarded headers never establish the source address.

## Access Projection

The control plane publishes versioned access changes through its transactional outbox:

```text
GatewayAPIKeyIssued
GatewayAPIKeyChanged
GatewayAPIKeyRevoked
GatewayAPIKeyPolicyPublished
```

Each Gateway maintains `gateway_access_projection` state sufficient to derive the current Authenticated Principal locally. Projection messages are encrypted in transit and authenticated between owned systems. They may contain a secret digest needed for local verification but never the raw key.

Consumers use an inbox table and aggregate revision to make delivery idempotent and monotonic. A gap prevents advancement past the missing revision and makes projection lag visible to Operations. Snapshot replacement is available for bootstrap and repair.

Inference authentication performs no synchronous call to Human IAM or the control plane. The initial revocation-propagation objective is five seconds inside a healthy region; the measured lag is an operational signal rather than an unverified promise.

## Digest-pepper rotation

Support multiple active digest versions. New issuance uses the current version; authentication can verify explicitly configured prior versions during a bounded migration. Re-digesting requires presentation of a raw key or key reissuance because the original plaintext is not recoverable. Removing an old pepper is a gated operation that first proves no active key depends on its digest version.

## Usage attribution

Successful authentication records API Key identity on the Response and Usage Ledger. `last_used_at` is a coarse, asynchronous projection so inference does not create a hot write on every request. It is not financial evidence.

## Consequences

- Gateway availability and first-token latency no longer depend on a remote auth hop.
- Revocation becomes eventually propagated, so its SLO and lag must be observable.
- One-time secret delivery prevents later key recovery and requires an explicit rotation UX.
- Policy restrictions become durable behavior rather than descriptive metadata.

## Rejected alternatives

### Call the control plane for every inference request

Rejected. Control-plane latency and outages would become data-plane latency and outages.

### Store encrypted raw Gateway API Keys for retrieval

Rejected. Operators need rotation, not secret recovery; retaining retrievable keys expands the breach surface.

### Use long-lived self-contained JWTs as Gateway API Keys

Rejected. Long-lived embedded authorization state makes revocation and Policy changes stale by construction.

## Acceptance criteria

- Issuance uses a cryptographically secure generator and persists no raw key.
- The raw secret is observable only in the first successful issuance or rotation response.
- List, metadata update, expiry, revoke, rotate, and Policy history have PostgreSQL integration coverage.
- Projection consumers tolerate duplicates, reject stale revisions, detect gaps, and recover from a snapshot.
- Gateway black-box tests prove local authentication continues while the control plane is unavailable.
- Revocation propagation and projection revision/lag are exposed to Operations.
- Tests cover allowed model, operation, CIDR, region, and concurrency restrictions without weakening Tenant Policy.
