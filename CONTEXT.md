# Universal LLM Gateway

This context describes a multi-tenant MaaS gateway that gives China-originating and global applications one model contract, while preserving provider capabilities, routing choices, conversation state, and cost accountability.

## Language

**Tenant**:
A customer organization whose credentials, policies, traffic, stored state, caches, and billing records are isolated from every other customer.
_Avoid_: Account, client

**Gateway API Key**:
A revocable credential owned by one Tenant, with its own metadata, policy revision, expiry, and usage attribution identity.
_Avoid_: Provider key, bearer token row

**Authenticated Principal**:
The current Tenant and Gateway API Key identity plus their current policy revisions, Home Region, and execution epoch established from a presented credential.
_Avoid_: Tenant header, API key string

**Limit Policy**:
A revisioned set of request, rate, token, spend, and Cache Protection bounds; the effective bound is always the most restrictive Tenant and Gateway API Key value.
_Avoid_: Billing config, soft warning

**Quota Reservation**:
A durable, expiring claim against an effective Limit Policy made before a Provider side effect and later settled using actual Usage Ledger evidence.
_Avoid_: Usage estimate, rate-limit check

**Provider Attempt**:
One bounded execution against one Model Route, with its own Quota Reservation, side-effect certainty, usage evidence, and terminal settlement.
_Avoid_: Retry, Provider request

**Usage Ledger**:
The immutable financial fact for one Provider attempt or cache refresh, attributed to its Tenant and sponsoring Gateway API Key using the price snapshot in force at execution.
_Avoid_: Usage counter, dashboard row

**Usage Projection**:
A rebuildable Tenant or Gateway API Key aggregation derived transactionally from Usage Ledger facts for reporting and quota operations.
_Avoid_: Billing source of truth

**Provider**:
An upstream model vendor or inference platform that accepts model requests and reports usage.
_Avoid_: Backend, vendor API

**Control Event Relay**:
The authenticated, encrypted, globally ordered delivery boundary that projects
control-plane facts into one Gateway region without granting the Gateway access
to control-plane tables.
_Avoid_: Shared outbox reader, Operations heartbeat bus

**Control Projection Bootstrap**:
A bounded, authenticated point-in-time bundle of one region's Access and
Provider Connection execution projections plus the current global Routing
Catalog and its Control Event source cursor.
_Avoid_: Database replica, raw-key export, best-effort repair dump

**Control Event History Floor**:
The greatest Control Event cursor through which retained relay history has been
pruned; a Gateway behind it must install a Control Projection Bootstrap before
resuming incremental delivery.
_Avoid_: Earliest row ID, Gateway acknowledgement

**Provider Connection Execution Projection**:
The Gateway-local, secret-free metadata needed to compile Model Routes for one
Provider Connection revision and credential version.
_Avoid_: Provider Connection management replica, secret-reference view

**Execution Secret Delivery**:
A bounded machine interface that returns the exact immutable Provider credential
for one enabled regional Provider Connection revision during route compilation.
_Avoid_: Secret event, inference-time control-plane lookup

**Model Route**:
A policy-governed path from a requested model capability to one concrete provider, model, credential scope, and region.
_Avoid_: Endpoint, deployment

**Model Catalog**:
The Tenant-visible list of public model identifiers currently backed by healthy, Home Region-compatible Model Routes with native text capability.
_Avoid_: Raw Provider inventory, environment model list

**Capability Profile**:
The declared set of model behaviors a Model Route supports natively, through translation, or not at all.
_Avoid_: Feature flags, compatibility boolean

**Response**:
The durable record of one model execution, including its input and output items, status, usage, attempts, and causal links.
_Avoid_: Completion, result row

**Conversation**:
An ordered, mutable container of input and output items used across multiple Responses.
_Avoid_: Session

**Response Chain**:
A branchable sequence of Responses connected through previous Response identifiers without requiring a mutable Conversation.
_Avoid_: Session

**Home Region**:
The one region authorized to perform strongly consistent writes for a Tenant, Conversation, or Response execution epoch.
_Avoid_: Primary datacenter

**Response Store**:
The authoritative durable state for Responses, Conversations, item history, execution status, and deletion tombstones.
_Avoid_: Response cache

**Prompt Cache**:
A provider-owned, ephemeral reuse of an exact prompt prefix that reduces input processing cost and latency but never stores an answer for replay.
_Avoid_: Response cache, semantic cache

**Gateway Response Cache**:
An optional gateway-owned copy of a completed answer that may satisfy a later eligible request without invoking a Provider.
_Avoid_: Prompt cache

**Cache Anchor**:
The exact stable prompt prefix and routing identity that a future request must reproduce to reuse a Prompt Cache entry.
_Avoid_: Session head

**Cache Lease**:
The gateway's time-bounded estimate that a Cache Anchor remains reusable on one exact Model Route.
_Avoid_: Guaranteed cache entry

**Cache Protection**:
An opt-in policy that may spend a bounded amount to refresh an economically valuable Cache Lease before it expires.
_Avoid_: Keepalive mode, infinite heartbeat

**Refresh Sponsor**:
The Gateway API Key whose authenticated request created a Cache Protection intent and whose current permission, refresh quota, and usage attribution govern that delayed refresh.
_Avoid_: Cache owner, background Tenant

**Continuation Forecast**:
An estimate of whether and when a cache-compatible request will arrive, used to decide whether Cache Protection has positive expected value.
_Avoid_: Task-finished classifier

**Protected Hit**:
A verified Prompt Cache hit occurring after the original Cache Lease would have expired but before a successful refresh expires.
_Avoid_: Cache hit

**Savings Ledger**:
An immutable record separating observed cache discounts, estimated Cache Protection savings, refresh costs, and experimentally validated incremental savings.
_Avoid_: Savings counter
