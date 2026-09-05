# Provider model discovery and import

Models & Capabilities now has **Import from provider** for operators with
`platform:providers:write` and `platform:routing:write`. Choose a connection,
fetch its model inventory, select models, and create a routing draft. The draft
opens on the Models page with the existing edit, validate and publish controls.
Rollout receipts remain the authority for application to gateways.

Discovery reuses the existing asynchronous Provider Connection operation and
its server-side Secret Custody adapter (OpenAI, DeepSeek, Anthropic and Gemini).
No provider secret is sent to the browser. Provider inventory does not assert
Gateway capability, pricing, or inference authorization.

## Read contract

`GET /control/v1/provider-operations/{operation_id}/models?limit=100&cursor=...`
(BFF prefix `/api`). Only a succeeded `model_discovery` operation can be read.
The response contains `data: [{id, owned_by, capabilities?}]` and an optional
`next_cursor`. Pages use model-ID ordering and are bound to one immutable
operation. Default/max page size is 100. Invalid cursors, limits and operation
types return 400; normal registry authorization and not-found errors apply.
The BFF requires provider-read or routing-read permission for operation reads.

Fetching uses the existing POST model-discoveries endpoint and revision fence.
Polling can be resumed after timeout without queueing a second operation.
Changing connections clears selection. Import rechecks connection revision,
uses the current catalog as its base, preserves existing routes and skips
existing connection/model/region combinations. The discovery ID is recorded
in the draft reason. A stale base is rejected by the existing CAS mechanism.

New routes are disabled with no tenant visibility, no declared capabilities,
and incomplete cost evidence. Review these fields before validation/publication.
The route editor now includes cost source and effective-at fields required by
the backend. Unknown prices are not silently copied from another model.

GCP enables authenticated operator discovery with a maximum of 10 upstream
list requests per operation and the existing timeouts/endpoint allowlist.
Probes remain limited to one model-list request. Neither operation generates
inference requests. The user must still publish a reviewed routing draft to
make an imported model available through the Gateway Model Catalog.
