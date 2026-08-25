# Universal LLM Gateway

This context describes a multi-tenant MaaS gateway that gives China-originating and global applications one model contract, while preserving provider capabilities, routing choices, conversation state, and cost accountability.

## Language

**Tenant**:
A customer organization whose credentials, policies, traffic, stored state, caches, and billing records are isolated from every other customer.
_Avoid_: Account, client

**Provider**:
An upstream model vendor or inference platform that accepts model requests and reports usage.
_Avoid_: Backend, vendor API

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

**Continuation Forecast**:
An estimate of whether and when a cache-compatible request will arrive, used to decide whether Cache Protection has positive expected value.
_Avoid_: Task-finished classifier

**Protected Hit**:
A verified Prompt Cache hit occurring after the original Cache Lease would have expired but before a successful refresh expires.
_Avoid_: Cache hit

**Savings Ledger**:
An immutable record separating observed cache discounts, estimated Cache Protection savings, refresh costs, and experimentally validated incremental savings.
_Avoid_: Savings counter
