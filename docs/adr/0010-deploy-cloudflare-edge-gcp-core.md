---
status: accepted
---

# Deploy with a Cloudflare edge and a GCP application core

## Context

The Gateway, control plane, and Metering are independently deployable Go
processes with PostgreSQL as their authoritative store. Production also needs
public inference, a human-facing management console, protected management APIs,
regional secret custody, immutable Metering exports, migrations, and observable
rollout gates.

Cloudflare already owns the public DNS and is useful for TLS termination, DDoS
protection, coarse WAF controls, and workforce access. GCP is the better boundary
for the stateful application, regional networking, Cloud Run, Cloud SQL, Secret
Manager, and GCS. Sending inference through a Cloudflare Worker would add an
unnecessary execution and billing hop to the highest-volume streaming path.

This ADR records the accepted target architecture and rollout plan. It is not
evidence that any cloud resources or production services have been deployed.

## Decision

Use Cloudflare as the public edge and GCP as the application and data core. The
first Home Region is `us-west`, implemented in GCP region `us-west1`.

All public hostnames enter through Cloudflare, but their edge behavior differs:

```text
Inference client
  -> llm-api.paxtech.net
  -> Cloudflare proxied DNS + TLS + DDoS/WAF (no Worker, no cache, no transform)
  -> GCP External Application Load Balancer
  -> Gateway Cloud Run service in us-west1

Operator browser
  -> llm-console.paxtech.net
  -> Cloudflare static/SSR frontend and optional thin BFF
  -> llm-control.paxtech.net or llm-metering.paxtech.net
  -> Cloudflare Access
  -> GCP External Application Load Balancer
  -> Control Plane or Metering Cloud Run service in us-west1

Gateway, Control Plane, and Metering machine traffic
  -> authenticated private GCP service endpoints
  -> never routed back through Cloudflare
```

Phase 1 uses one GCP global External Application Load Balancer with separate
host-routed backend services and Cloud Armor policies. Each backend uses a
serverless NEG for its Cloud Run service. A later region can add a backend and
explicit Home Region routing without changing the public contract.

## Public hostnames and certificates

Use flat, first-level hostnames below the zone apex:

| Hostname | Purpose | Edge behavior | GCP target |
| --- | --- | --- | --- |
| `llm-api.paxtech.net` | Tenant inference API | Proxied; no Worker, cache, or content transform | Gateway |
| `llm-console.paxtech.net` | Human management UI | Static assets or Worker/Pages BFF | Cloudflare-hosted frontend |
| `llm-control.paxtech.net` | Human management API | Cloudflare Access; no cache | Control Plane |
| `llm-metering.paxtech.net` | Usage, export, and operations API | Cloudflare Access; no cache | Metering |

Do not use deeper names such as `api.llm.paxtech.net` in phase 1. For a full
setup zone, Cloudflare Universal SSL covers the apex and first-level subdomains;
deeper names require a different certificate arrangement such as Advanced
Certificate Manager/Total TLS or a custom certificate. The flat names therefore
avoid a certificate add-on solely for hostname depth.

The GCP load balancer also terminates authenticated TLS. Provision its
certificates through Certificate Manager with DNS authorization, and use
Cloudflare Full (strict) origin validation. Cloud Run ingress is restricted to
internal traffic and Cloud Load Balancing so the default `run.app` URL cannot
bypass the load balancer.

## Cloudflare responsibilities

Cloudflare owns only edge concerns:

- authoritative DNS and public TLS;
- proxied ingress, DDoS protection, and coarse WAF/rate controls;
- body-size, bot, IP, ASN, and obvious abuse filtering where the selected plan
  supports it;
- Access authentication for the console and management APIs;
- static console delivery and, only if needed, a thin Worker/Pages BFF.

Cloudflare does not own Tenant authentication, Gateway API Key validation,
Limit Policy, Quota Reservation, Provider routing, usage accounting, or durable
application data. Those controls remain in the Go services.

For `llm-api.paxtech.net`:

- bypass cache for all `/v1/*` requests and responses;
- preserve `Authorization`, idempotency, request IDs, SSE framing, and streaming
  flush behavior;
- do not rewrite request or response bodies;
- do not put a Worker on the inference path;
- verify long-running and streamed requests against Cloudflare, load-balancer,
  and Cloud Run timeout limits before production traffic.

Cloudflare Load Balancing is not enabled in phase 1. GCP owns origin load
balancing and health-based traffic distribution. This avoids paying for a
second load-balancing product before a multi-origin requirement exists.

## GCP foundation

Create dedicated projects for each environment rather than deploying into an
unrelated existing project:

```text
pax-llm-gateway-stg
pax-llm-gateway-prod
```

Organization-approved equivalents are acceptable, but staging and production
must not share a project, database, service accounts, secrets, buckets, or
deployment identity.

Provision the following per environment:

- Artifact Registry for immutable container images;
- one External Application Load Balancer with host-based backends;
- Cloud Run services for Gateway, Control Plane, and Metering;
- one Cloud Run migration Job using the schema-owner identity;
- Cloud SQL for PostgreSQL with private IP and certificate-verified encrypted
  connections;
- Secret Manager for application machine keys, database credentials, Provider
  credentials, and signing material;
- a single-region `us-west1` GCS bucket for immutable Metering exports;
- Cloud Logging, Monitoring, Error Reporting, alert policies, and budgets;
- dedicated runtime, migration, deployment, and operator service accounts.

Production targets Cloud SQL PostgreSQL 18, regional high availability,
automated backups, point-in-time recovery, deletion protection, and tested
restore procedures. PostgreSQL 18 availability and repository compatibility
must be rechecked when provisioning; use the newest repository-tested supported
major if that gate is not satisfied.

The first deployment may use one Cloud SQL instance, but it must preserve the
existing ownership boundaries with separate databases or schemas and separate
runtime roles for Gateway, Control Plane, and Metering. Only the migration Job
uses the owner role. Metering keeps its inbox, facts, projection generations,
and export jobs separate and cannot read Response content or the authoritative
Usage Ledger.

The GCS export bucket is regional, versioned or retention-protected according
to the recovery policy, and denies public access. The Metering runtime receives
only the bucket metadata/read/create permissions required by ADR 0007 and never
overwrite permission.

## Identity, secrets, and origin protection

Use one service account per Cloud Run service. Internal service calls use
Cloud Run IAM and signed service identity tokens in addition to the existing
application HMAC and revision/fencing rules. Runtime identities do not receive
project-wide roles.

GitHub Actions deploys through Workload Identity Federation. Do not create or
store a long-lived Google service-account JSON key in GitHub.

Provider credentials are created as Provider Connections and stored in Secret
Manager. A staging deployment can use only the deterministic echo Provider and
therefore needs no paid Provider key. Production needs a key only for each
Provider Connection actually enabled; it does not require keys for every
supported adapter. The Gateway does not receive broad Secret Manager access.
It obtains the exact immutable execution credential through the authenticated,
bounded execution-secret delivery boundary.

Cloud Armor permits origin requests only from maintained Cloudflare egress
ranges and denies direct Internet access. Automate and review the Cloudflare IP
allowlist update. Cloud Run's ingress restriction provides a second bypass
control.

Cloudflare Access supplies its assertion in `Cf-Access-Jwt-Assertion`, while the
current control APIs expect a Human IAM bearer token. Before exposing management
traffic, add and test a narrow authentication adapter that validates the Access
JWT signature, issuer, audience, expiry, and allowed identity, then derives the
existing Human IAM principal. Never trust the assertion header unless the
request has also crossed the protected Cloudflare-to-GCP origin path. Machine
interfaces continue to use their dedicated identities and must not be made
human-accessible through this adapter.

## Capacity and availability

The current processes contain background consumers. Configure instance-based
Cloud Run billing with always-allocated CPU and minimum instances so workers do
not stop between HTTP requests.

| Environment | Gateway | Control Plane | Metering | Cloud SQL |
| --- | ---: | ---: | ---: | --- |
| Staging | minimum 1 | minimum 1 | minimum 1 | small, production-compatible networking and TLS |
| Production initial | minimum 3 | minimum 1 | minimum 1 | regional HA with PITR |

Maximum instances, concurrency, CPU, memory, request timeout, and database pool
sizes are load-test outputs, not guesses. Bound Cloud Run maximum instances so
a traffic spike cannot exhaust Cloud SQL connections. Set budget alerts for
Cloud Run, Cloud SQL, load balancing, networking, storage, and Provider spend.

One Home Region does not claim multi-region application availability. The
global edge and load balancer improve ingress availability, while stateful
writes remain intentionally fenced to `us-west`.

## Cost posture

At the time of this decision, flat hostnames fit Cloudflare Universal SSL,
Cloudflare Access has a free tier for teams under 50 users, and Workers Paid
starts at USD 5 per account per month. These are planning assumptions, not a
contract; recheck the account plan, limits, taxes, and current pricing before
enabling production billing.

The initial Cloudflare posture is:

- no certificate add-on solely for hostname depth;
- no Cloudflare Load Balancing;
- no Worker invocation on inference traffic;
- Access on management traffic, using the free tier while eligibility remains;
- a Worker/Pages BFF only when the console requires dynamic server behavior,
  with CPU and usage limits configured.

Most steady-state infrastructure cost therefore remains in GCP: Cloud SQL HA,
minimum Cloud Run instances, load balancing, logs, storage, and network egress.
Provider inference spend remains separately controlled by Limit Policy and
Quota Reservations.

## Infrastructure and delivery implementation

Before the first cloud rollout, add these repository-owned assets:

1. Reproducible image targets for `cmd/llm-gateway`, `cmd/control-plane`, and
   `cmd/metering`.
2. A dedicated migration command/image that exits after applying and verifying
   the schema. Production services must not migrate on startup.
3. Terraform under `infra/gcp` for projects, APIs, IAM, Artifact Registry,
   networking, Cloud SQL, GCS, Cloud Run, the load balancer, Cloud Armor,
   monitoring, and budgets.
4. Terraform or an equally reviewable configuration under `infra/cloudflare`
   for DNS, proxying, cache/WAF rules, Access applications/policies, and the
   optional console Worker/Pages deployment.
5. GitHub Actions that authenticate with Workload Identity Federation, build
   once, retain image digests, plan infrastructure, require production approval,
   run the migration gate, deploy by digest, and record revision evidence.
6. Explicit Cloud SQL CA/certificate handling. Use a Cloud SQL connector only if
   the runtime can prove the repository's certificate-verified transport
   invariant; otherwise use private IP with `verify-full` and rotated CA
   material.
7. The Cloudflare Access JWT adapter and its bypass, expiry, issuer, audience,
   and identity tests.

Secrets are injected by Secret Manager references, never Terraform state,
container images, repository variables, logs, or deployment output.

## Rollout sequence

Roll out staging first with no public Provider spend:

1. Create the staging project, Workload Identity Federation, budgets, and base
   networking through reviewed infrastructure code.
2. Provision Cloud SQL, its roles and backups, Secret Manager, the export
   bucket, Artifact Registry, and monitoring.
3. Build all images from one source SHA and record their immutable digests.
4. Run the migration Job and verify the resulting schema version and role
   grants.
5. Deploy Control Plane with no public traffic and verify `/healthz`, `/readyz`,
   JWKS/Human IAM behavior, secret custody, and Operations queries.
6. Create a staging Tenant and issue a one-time Gateway API Key without printing
   or logging it. Use the echo Provider first.
7. Register any required Provider Connection, validate it, then validate and
   publish a Routing Catalog revision.
8. Deploy Metering and verify its projection generation, relay status, and GCS
   export preconditions.
9. Deploy Gateway with no public traffic. Verify bounded control relay catch-up,
   Access and Routing Catalog revisions, execution-secret delivery, `/readyz`,
   `/v1/models`, one bounded `/v1/responses` request, streaming, usage, and
   Metering projection.
10. Configure the GCP load balancer and Cloud Armor. Prove the Cloud Run URL and
    load-balancer origin cannot bypass the intended edge controls.
11. Configure Cloudflare DNS, TLS, cache bypass, WAF, Access, and console. Run
    end-to-end tests through the final hostnames.

Repeat the same pipeline for production only after staging gates pass. In
production, enable one Provider Connection at a time, use a low-spend canary
Tenant, and increase traffic deliberately.

## Release evidence and rollback

A successful release report records independently:

- Git source SHA and remote CI result;
- container image digests and vulnerability scan result;
- Terraform plan/apply identity and resulting resource revisions;
- database backup timestamp, migration Job result, schema version, and runtime
  role verification;
- Cloud Run revision names, configuration hashes, and traffic percentages;
- load-balancer backend health and Cloud Armor policy revision;
- Cloudflare DNS/proxy state, edge certificate, Access policy, and cache bypass;
- `/healthz` and `/readyz` for each process;
- desired and applied Access/Routing revisions, relay cursor/source head, and
  Metering projection cutoff;
- representative `/v1/models`, non-streaming Response, streaming Response,
  management, export, revocation, and direct-origin-bypass checks;
- absence of secrets and prompt content in logs.

Do not infer deployment success from a green build, a healthy Cloud Run
revision, or a Cloudflare DNS change alone.

Application rollback shifts Cloud Run traffic to the previous compatible image
digest. Database rollback uses a pre-approved restore or forward repair; never
run destructive down migrations merely to start an older binary. A Routing
Catalog or Provider Connection is drained/disabled through its owned control
path, not deleted. Ambiguous Provider side effects retain uncertain quota and
are not retried by the deployment system.

## Consequences

- Inference receives Cloudflare's edge protection without paying Worker cost or
  adding Worker behavior to every streamed token.
- Management receives Cloudflare Access while the authoritative IAM and audit
  boundaries remain in the Go control plane.
- Public traffic has two TLS/load-balancing hops, which must be measured and
  monitored.
- A Cloudflare outage can affect both inference and management ingress even when
  GCP is healthy; internal GCP control and Metering traffic remains independent.
- The initial regional database and minimum instances create meaningful fixed
  GCP cost in exchange for worker progress and production durability.
- Multi-region failover remains future work and cannot be claimed from this
  single-Home-Region deployment.

## Rejected alternatives

### Put all inference through a Cloudflare Worker

Rejected for phase 1. It adds cost, limits, and an execution hop to the largest
streaming path without moving authoritative routing, quota, or usage ownership
out of GCP.

### Send clients directly to Cloud Run

Rejected. It bypasses the selected public DNS, edge protection, origin access
policy, and stable load-balancer boundary.

### Put only management traffic behind Cloudflare

Rejected. Inference also benefits from Cloudflare TLS, DDoS, and coarse abuse
controls. The distinction is that inference uses transparent proxying while
management additionally uses Access and may use a BFF.

### Use deep subdomains from the first release

Rejected. Flat names express the same service boundaries and avoid purchasing
certificate coverage only because of hostname depth.

### Deploy into the existing unrelated GCP project

Rejected. Shared project IAM, budgets, quotas, networks, and secrets would make
environment ownership and release evidence ambiguous.

## Acceptance

Accepted on 2026-08-30 as the deployment target and rollout plan. Acceptance
authorizes subsequent scoped infrastructure and deployment implementation; it
does not assert that GCP projects, Cloudflare configuration, Provider
Connections, migrations, or production traffic exist.

Cloudflare product and pricing assumptions were checked against the official
[Universal SSL limitations](https://developers.cloudflare.com/ssl/edge-certificates/universal-ssl/limitations/),
[Zero Trust pricing](https://www.cloudflare.com/plans/zero-trust-services/),
[Workers pricing](https://developers.cloudflare.com/workers/platform/pricing/),
and [Load Balancing documentation](https://developers.cloudflare.com/load-balancing/)
on the acceptance date and must be rechecked before purchase.
