# Provider model discovery and import

Models & Capabilities now has **Import from provider** for operators with
`platform:providers:write` and `platform:routing:write`. Choose a connection,
fetch its model inventory, select models, configure them, and create an automatically validated routing draft. The draft
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

## Guided setup and recovery

The import dialog now continues to **Configure text models**. Operators explicitly
confirm model text/streaming support, enter input/output/cached-input prices in
USD per million tokens, provide the price source, and choose all tenants or copy
an existing route's complete visibility policy (including Limit Policy revisions).
Snapshot IDs and effective timestamps are generated. Unknown prices remain blank;
prices and model capabilities are never copied from another model.

Known non-chat name patterns are flagged before configuration and cannot be
activated by this text workflow. Names are only a warning heuristic, never proof
of capability. Operators explicitly deselect unsupported models; nothing is
silently dropped. Remaining routes are active in the draft, not in production.
Create-and-validate is followed by the separate reviewed publication step.

**Continue saved draft** on Models opens persisted drafts after a reload.
**Finish model setup** repairs incomplete routes in existing drafts through the
same form, preserves unrelated routes, saves and validates automatically.
Failed validation reports display even while the draft status remains `draft`;
issues show model names, next steps and edit buttons. Unsaved edits label the
report stale, and **Save and validate** performs the two revision-fenced actions.
The manual route editor also preserves tenant Limit Policy references and exposes
a revision field for each selected tenant.

`npm test` in `web` exercises failed-report visibility, price precision, missing
capability evidence, modality warnings, connection constraints and policy copying.

GCP enables authenticated operator discovery with a maximum of 10 upstream
list requests per operation and the existing timeouts/endpoint allowlist.
Probes remain limited to one model-list request. Neither operation generates
inference requests. The user must still publish a reviewed routing draft to
make an imported model available through the Gateway Model Catalog.
