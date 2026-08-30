# Operations runbook

## Production invariants

- PostgreSQL is authoritative. Production requires `GATEWAY_DURABILITY_ATTESTATION=sync-multi-az`.
- Stateful writes execute in the Tenant Home Region and are fenced by execution epoch.
- Raw Gateway API Keys and Provider credentials are never stored in normal application state or emitted in logs.
- Routing Catalog and Access Projection revisions advance monotonically; a Gateway retains its last valid snapshot.
- Usage Ledger, Capability Usage Ledger, and Cache Refresh Usage Ledger are immutable. Quota Reservations settle from durable evidence.
- Cache Protection is hard-off by default.

## Schema rollout

Migrations are embedded but execute only with `GATEWAY_MIGRATE=true`. Prefer a dedicated migration deployment gate:

1. Verify backup and rollback posture appropriate to the environment.
2. Inspect the current schema and binary compatibility range.
3. Run one migration job.
4. Verify the resulting schema version.
5. Deploy compatible processes.
6. Confirm readiness and representative requests.

Do not run destructive rollback SQL merely to make an older binary start. Use a forward repair migration unless an approved recovery plan says otherwise.

## Configuration rollout

Before ADR 0006 is implemented, PostgreSQL `model_routes` publication is initiated through explicit bootstrap/publish environment flags and CAS revisions. After ADR 0006, verify:

```text
publication validation report
published revision
regional rollout receipts
Gateway active routing revision
representative /v1/models output
representative Response execution
```

Configured route health and observed Provider health are separate. Do not delete a route to drain it.

## Access incident

For a leaked Gateway API Key:

1. Resolve the exact Tenant and key identity without printing the secret.
2. Revoke the key through the authoritative access mutation path.
3. Verify the authoritative revision.
4. Verify Gateway Access Projection revision and revocation propagation lag when ADR 0004 is implemented.
5. Confirm a sanitized authentication probe is rejected.
6. Inspect Usage Ledger by key identity for the authorized incident window.

Do not mutate digest rows manually or purge historical usage attribution.

## Quota and usage incident

- Inspect Policy revisions used by the request.
- Separate committed, reserved, uncertain, and remaining capacity.
- Check reconciliation backlog before assuming counters are corrupt.
- Preserve uncertain capacity when a Provider may have started work.
- For Stage A, inspect capability, Route, Provider, normalized input/document units, immutable Price Snapshot, and `capability_usage_ledger`; never copy vectors or moderated/reranked content into incident notes.
- Rebuild projections from immutable facts; do not edit Usage Ledger.
- Keep different currencies separate unless an explicit conversion snapshot exists.

## Worker and backlog incident

Inspect oldest pending age and count for outbox, consumer inbox, quota reconciliation, Cache Protection, and retention. Determine whether the worker is stopped, fenced, processing a poison event, or waiting on a true external dependency. Prefer repairing the blocked item or consumer offset with an audited procedure over deleting backlog.

For Metering, query `/control/v1/operations/metering` before inspecting the
service directly. Separate heartbeat staleness, projection cutoff, oldest
pending usage event, poison-event count, and queued exports. A missing
observation is `unavailable`; a stale heartbeat or poison event is `degraded`.
Do not infer a Metering cutoff from the Gateway Usage Ledger head.

Production CSV exports use a single-region GCS bucket. Verify the configured
region and grant the Workload Identity only `storage.buckets.get`,
`storage.objects.create`, and `storage.objects.get`. `ifGenerationMatch=0`
prevents overwrite; a retry that finds different bytes is an integrity
incident, not permission to replace the object.

## Health and readiness

`/healthz` proves the process can answer. ADR 0008 defines `/readyz`, heartbeats, revision receipts, and lag. A control-plane, Metering, or Human IAM outage should not make a Gateway unready when its local access/routing snapshots, authoritative store, Home Region configuration, and outbox capacity remain healthy.
