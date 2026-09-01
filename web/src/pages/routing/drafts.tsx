/**
 * Drafts section: persisted draft list, create-from-head, open-by-id, and the draft editor with
 * visual/JSON views, save / validate / probe / publish lifecycle.
 */
import { useEffect, useState } from "react";
import { ApiError } from "../../api/client";
import { useAuth } from "../../auth/AuthProvider";
import { PERMISSIONS } from "../../auth/permissions";
import { ConfirmDialog, Modal, useToast } from "../../components/feedback";
import { Badge, Button, Card, EmptyState, Spinner, Table, Td, Th } from "../../components/ui";
import { timeAgo, truncateId } from "../../lib/format";
import {
  createDraft,
  getDraft,
  getOperation,
  listDrafts,
  probeDraft,
  publishDraft,
  updateDraft,
  validateDraft,
} from "./api";
import { blankRoute, RouteFormDrawer } from "./routeForm";
import {
  draftStatusTone,
  formatRevision,
  isTerminalOperation,
  newDraftId,
  operationTone,
  parseRegions,
  type Draft,
  type ManagedRoute,
  type Operation,
  type Revision,
  type RoutingDocument,
} from "./types";
import { CapabilityBadges, ValidationReportView } from "./widgets";

const inputStyle = {
  padding: "6px 10px",
  borderRadius: "var(--radius)",
  border: "1px solid var(--line)",
  background: "var(--bg)",
  color: "var(--ink)",
  fontSize: 12,
  fontFamily: "var(--font-mono)",
} as const;

export function DraftsSection({
  head,
  createSignal,
  onPublished,
  onHeadChanged,
}: {
  head: Revision | null;
  createSignal: number;
  onPublished: (publicationId: string) => void;
  onHeadChanged: () => void;
}) {
  const toast = useToast();
  const { can } = useAuth();
  const canWrite = can(PERMISSIONS.routingWrite);
  const [drafts, setDrafts] = useState<Draft[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [createOpen, setCreateOpen] = useState(false);
  const [openId, setOpenId] = useState("");
  const [opening, setOpening] = useState(false);
  const [loadingDrafts, setLoadingDrafts] = useState(true);

  useEffect(() => {
    if (createSignal > 0) setCreateOpen(true);
  }, [createSignal]);

  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      try {
        const result = await listDrafts();
        if (!cancelled) setDrafts(result.data ?? []);
      } catch (err) {
        if (!cancelled && err instanceof ApiError) toast(`${err.code}: ${err.message}`, "error");
      } finally {
        if (!cancelled) setLoadingDrafts(false);
      }
    };
    void load();
    return () => {
      cancelled = true;
    };
  }, [toast]);

  const upsertDraft = (draft: Draft) =>
    setDrafts((current) => {
      const idx = current.findIndex((d) => d.id === draft.id);
      if (idx < 0) return [draft, ...current];
      const next = [...current];
      next[idx] = draft;
      return next;
    });

  const openById = async () => {
    const id = openId.trim();
    if (!id) return;
    setOpening(true);
    try {
      const draft = await getDraft(id);
      upsertDraft(draft);
      setSelectedId(draft.id);
      setOpenId("");
      toast(`Draft ${draft.id} loaded`, "success");
    } catch (err) {
      if (err instanceof ApiError) toast(`${err.code}: ${err.message}`, "error");
      else throw err;
    } finally {
      setOpening(false);
    }
  };

  const selected = drafts.find((d) => d.id === selectedId) ?? null;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
      <Card
        title="Drafts"
        actions={
          <Button variant="primary" disabled={!canWrite || !head} title={!canWrite ? "Requires platform:routing:write" : head ? undefined : "No published revision to base a draft on"} onClick={() => setCreateOpen(true)}>
            Create draft
          </Button>
        }
      >
        <div style={{ fontSize: 11.5, color: "var(--ink3)", marginBottom: 10 }}>
          Drafts are loaded from the control plane; enter a draft id to open an older item directly.
        </div>
        <div style={{ display: "flex", gap: 8, marginBottom: drafts.length > 0 ? 12 : 0 }}>
          <input
            style={{ ...inputStyle, flex: 1, maxWidth: 320 }}
            placeholder="rcd_…"
            value={openId}
            onChange={(e) => setOpenId(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void openById();
            }}
          />
          <Button disabled={!openId.trim() || opening} onClick={() => void openById()}>
            {opening ? <Spinner size={12} /> : "Open draft"}
          </Button>
        </div>
        {loadingDrafts ? (
          <div style={{ padding: 24, display: "flex", justifyContent: "center" }}><Spinner size={16} /></div>
        ) : drafts.length === 0 ? (
          <EmptyState title="No drafts" hint="Create a draft from the active revision, or open one by id." />
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>Draft</Th>
                <Th>Base</Th>
                <Th>Status</Th>
                <Th>Editor</Th>
                <Th>Updated</Th>
              </tr>
            </thead>
            <tbody>
              {drafts.map((d) => (
                <tr key={d.id} onClick={() => setSelectedId(d.id)} style={{ cursor: "pointer" }}>
                  <Td mono>
                    <span style={{ color: "var(--purple)", fontWeight: 600 }}>{d.id}</span>
                  </Td>
                  <Td mono>{formatRevision(d.base_revision)}</Td>
                  <Td>
                    <Badge tone={draftStatusTone(d.status)}>
                      {d.status}
                      {d.status === "validated" && (d.validation_report.warnings ?? []).length > 0
                        ? ` · ${(d.validation_report.warnings ?? []).length} warnings`
                        : ""}
                    </Badge>
                  </Td>
                  <Td>{d.updated_by}</Td>
                  <Td mono>{timeAgo(d.updated_at)}</Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Card>

      {selected && (
        <DraftEditor
          key={selected.id}
          draft={selected}
          onDraftChange={upsertDraft}
          onPublished={onPublished}
          onHeadChanged={onHeadChanged}
          onClose={() => setSelectedId(null)}
        />
      )}

      <CreateDraftModal
        open={createOpen}
        head={head}
        onClose={() => setCreateOpen(false)}
        onCreated={(draft) => {
          upsertDraft(draft);
          setSelectedId(draft.id);
          setCreateOpen(false);
        }}
      />
    </div>
  );
}

/* ------------------------------------------------------------------ */
/* Create draft modal                                                  */
/* ------------------------------------------------------------------ */

function CreateDraftModal({
  open,
  head,
  onClose,
  onCreated,
}: {
  open: boolean;
  head: Revision | null;
  onClose: () => void;
  onCreated: (draft: Draft) => void;
}) {
  const toast = useToast();
  const [draftId, setDraftId] = useState(newDraftId);
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (open) {
      setDraftId(newDraftId());
      setReason("");
    }
  }, [open]);

  const submit = async () => {
    if (!head) return;
    setBusy(true);
    try {
      const draft = await createDraft({
        id: draftId.trim(),
        base_revision: head.revision,
        document: head.document,
        reason: reason.trim(),
      });
      toast(`Draft ${draft.id} created from ${formatRevision(head.revision)}`, "success");
      onCreated(draft);
    } catch (err) {
      if (err instanceof ApiError) toast(`${err.code}: ${err.message}`, "error");
      else throw err;
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      open={open}
      onClose={onClose}
      title="Create draft"
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" disabled={!head || !draftId.trim() || !reason.trim() || busy} onClick={() => void submit()}>
            {busy ? <Spinner size={12} /> : "Create draft"}
          </Button>
        </>
      }
    >
      <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
        <p style={{ fontSize: 12, color: "var(--ink2)", margin: 0 }}>
          {head
            ? `Copies the active document of ${formatRevision(head.revision)} into a new editable draft.`
            : "No routing catalog published yet — a draft must be based on an existing revision."}
        </p>
        <label style={{ fontSize: 11, fontWeight: 600, color: "var(--ink3)" }}>
          Draft ID
          <input style={{ ...inputStyle, width: "100%", marginTop: 3 }} value={draftId} onChange={(e) => setDraftId(e.target.value)} />
        </label>
        <label style={{ fontSize: 11, fontWeight: 600, color: "var(--ink3)" }}>
          Reason (required)
          <textarea
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            rows={3}
            placeholder="Why is this draft needed?"
            style={{
              width: "100%",
              marginTop: 3,
              resize: "vertical",
              padding: 8,
              borderRadius: "var(--radius)",
              border: "1px solid var(--line)",
              background: "var(--bg)",
              color: "var(--ink)",
              fontSize: 12,
            }}
          />
        </label>
      </div>
    </Modal>
  );
}

/* ------------------------------------------------------------------ */
/* Draft editor                                                        */
/* ------------------------------------------------------------------ */

type PendingAction = "save" | "validate" | "probe" | "publish" | null;

function DraftEditor({
  draft,
  onDraftChange,
  onPublished,
  onHeadChanged,
  onClose,
}: {
  draft: Draft;
  onDraftChange: (draft: Draft) => void;
  onPublished: (publicationId: string) => void;
  onHeadChanged: () => void;
  onClose: () => void;
}) {
  const toast = useToast();
  const { can } = useAuth();
  const canWrite = can(PERMISSIONS.routingWrite);
  const [doc, setDoc] = useState<RoutingDocument>(() => JSON.parse(JSON.stringify(draft.document)) as RoutingDocument);
  const [dirty, setDirty] = useState(false);
  const [view, setView] = useState<"visual" | "json">("visual");
  const [jsonText, setJsonText] = useState("");
  const [jsonError, setJsonError] = useState<string | null>(null);
  const [editIndex, setEditIndex] = useState<number | null>(null);
  const [formOpen, setFormOpen] = useState(false);
  const [pending, setPending] = useState<PendingAction>(null);
  const [busy, setBusy] = useState(false);
  const [publishRegions, setPublishRegions] = useState("");
  const [probeOps, setProbeOps] = useState<Operation[] | null>(null);

  const report = draft.validation_report;
  const warnings = report?.warnings ?? [];
  const publishable = draft.status === "validated" && report?.valid === true;

  const mutateDoc = (next: RoutingDocument) => {
    setDoc(next);
    setDirty(true);
  };

  const patchRoute = (index: number, route: ManagedRoute) => {
    const routes = [...doc.routes];
    routes[index] = route;
    mutateDoc({ routes });
  };

  const openJsonView = () => {
    setJsonText(JSON.stringify(doc, null, 2));
    setJsonError(null);
    setView("json");
  };

  const applyJson = () => {
    try {
      const parsed = JSON.parse(jsonText) as RoutingDocument;
      if (!parsed || typeof parsed !== "object" || !Array.isArray(parsed.routes)) {
        setJsonError('Document must be a JSON object with a "routes" array.');
        return;
      }
      setDoc(parsed);
      setDirty(true);
      setJsonError(null);
      toast("JSON applied — it replaced the whole document", "info");
    } catch (err) {
      setJsonError(err instanceof Error ? err.message : String(err));
    }
  };

  const run = async (action: Exclude<PendingAction, null>, reason: string) => {
    setPending(null);
    setBusy(true);
    try {
      if (action === "save") {
        const updated = await updateDraft(draft.id, { document: doc, expected_revision: draft.revision, reason });
        onDraftChange(updated);
        setDirty(false);
        toast("Draft saved", "success");
      } else if (action === "validate") {
        const updated = await validateDraft(draft.id, { expected_revision: draft.revision, reason });
        onDraftChange(updated);
        const errors = (updated.validation_report.errors ?? []).length;
        const warns = (updated.validation_report.warnings ?? []).length;
        toast(`Validation complete — ${errors} errors · ${warns} warnings`, updated.validation_report.valid ? "success" : "error");
      } else if (action === "probe") {
        const result = await probeDraft(draft.id, { expected_revision: draft.revision, reason });
        setProbeOps(result.data ?? []);
        toast(`Probing ${(result.data ?? []).length} connection(s)`, "info");
      } else {
        const regions = parseRegions(publishRegions);
        const result = await publishDraft(draft.id, { expected_revision: draft.revision, required_regions: regions, reason });
        toast(`Published ${formatRevision(result.publication.catalog_revision)} — tracking publication`, "success");
        onPublished(result.publication.id);
        onHeadChanged();
      }
    } catch (err) {
      if (err instanceof ApiError) toast(`${err.code}: ${err.message}`, "error");
      else throw err;
    } finally {
      setBusy(false);
    }
  };

  const editTarget = editIndex !== null ? (doc.routes[editIndex] ?? null) : null;

  return (
    <Card
      title={
        <span style={{ display: "inline-flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
          Draft <span style={{ fontFamily: "var(--font-mono)", color: "var(--purple)" }}>{draft.id}</span>
          <Badge tone={draftStatusTone(draft.status)}>
            {draft.status}
            {draft.status === "validated" && warnings.length > 0 ? ` · ${warnings.length} warnings` : ""}
          </Badge>
          {dirty && <Badge tone="amber">unsaved changes</Badge>}
        </span>
      }
      actions={
        <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
          <Button disabled={!canWrite || busy || !dirty} title={canWrite ? undefined : "Requires platform:routing:write"} onClick={() => setPending("save")}>Save</Button>
          <Button
            disabled={!canWrite || busy || dirty}
            title={!canWrite ? "Requires platform:routing:write" : dirty ? "Save before validating" : undefined}
            onClick={() => setPending("validate")}
          >
            Validate
          </Button>
          <Button
            disabled={!canWrite || busy || dirty}
            title={!canWrite ? "Requires platform:routing:write" : dirty ? "Save before probing" : undefined}
            onClick={() => setPending("probe")}
          >
            Probe connections
          </Button>
          <Button
            variant="primary"
            disabled={!canWrite || busy || dirty || !publishable}
            title={
              !canWrite
                ? "Requires platform:routing:write"
                : dirty
                  ? "Save before publishing"
                  : !publishable
                    ? "Draft must pass validation first"
                    : undefined
            }
            onClick={() => setPending("publish")}
          >
            Publish…
          </Button>
          <Button onClick={onClose}>Close</Button>
        </div>
      }
    >
      <div style={{ fontSize: 11.5, color: "var(--ink2)", marginBottom: 12 }}>
        Base revision <b style={{ fontFamily: "var(--font-mono)" }}>{formatRevision(draft.base_revision)}</b>
        {" · "}draft revision <b style={{ fontFamily: "var(--font-mono)" }}>{draft.revision}</b> (sent as expected_revision)
        {" · "}{draft.updated_by} · updated {timeAgo(draft.updated_at)}
      </div>

      <div style={{ display: "grid", gridTemplateColumns: "minmax(0,1.7fr) minmax(0,1fr)", gap: 14, alignItems: "start" }}>
        {/* ---- editor (visual table / JSON) ---- */}
        <div style={{ border: "1px solid var(--line)", borderRadius: "var(--radius-lg)", overflow: "hidden" }}>
          <div style={{ display: "flex", gap: 2, padding: 8, borderBottom: "1px solid var(--line)", alignItems: "center" }}>
            {(["visual", "json"] as const).map((v) => (
              <button
                key={v}
                onClick={() => (v === "json" ? openJsonView() : setView("visual"))}
                style={{
                  padding: "5px 12px",
                  border: "none",
                  borderRadius: "var(--radius)",
                  background: view === v ? "var(--chip)" : "transparent",
                  color: view === v ? "var(--ink)" : "var(--ink2)",
                  fontSize: 12,
                  fontWeight: view === v ? 600 : 400,
                  cursor: "pointer",
                }}
              >
                {v === "visual" ? "Visual routes" : "JSON"}
              </button>
            ))}
            <span style={{ flex: 1 }} />
            {view === "visual" && (
              <Button
                disabled={!canWrite}
                title={canWrite ? undefined : "Requires platform:routing:write"}
                onClick={() => {
                  const routes = [...doc.routes, blankRoute()];
                  mutateDoc({ routes });
                  setEditIndex(routes.length - 1);
                  setFormOpen(true);
                }}
              >
                Add route
              </Button>
            )}
          </div>

          {view === "visual" ? (
            doc.routes.length === 0 ? (
              <EmptyState title="No routes in draft" hint="Add a route, or switch to the JSON view to paste a document." />
            ) : (
              <Table>
                <thead>
                  <tr>
                    <Th>Route</Th>
                    <Th>Public model</Th>
                    <Th>Connection · provider model</Th>
                    <Th>Region</Th>
                    <Th>Status</Th>
                    <Th>Capabilities</Th>
                    <Th></Th>
                  </tr>
                </thead>
                <tbody>
                  {doc.routes.map((route, i) => (
                    <tr key={`${route.route_id}-${i}`}>
                      <Td mono>{route.route_id || <span style={{ color: "var(--red)" }}>missing</span>}</Td>
                      <Td mono>{route.public_model}</Td>
                      <Td mono>
                        {route.provider_connection_id}
                        <span style={{ color: "var(--ink3)" }}> · {route.provider_model}</span>
                      </Td>
                      <Td mono>{route.execution_region}</Td>
                      <Td>{route.administrative_status}</Td>
                      <Td>
                        <CapabilityBadges capabilities={route.capabilities ?? {}} />
                      </Td>
                      <Td>
                        <div style={{ display: "flex", gap: 5 }}>
                          <Button
                            disabled={!canWrite}
                            title={canWrite ? undefined : "Requires platform:routing:write"}
                            onClick={() => {
                              setEditIndex(i);
                              setFormOpen(true);
                            }}
                          >
                            Edit
                          </Button>
                          <Button
                            disabled={!canWrite}
                            title={canWrite ? undefined : "Requires platform:routing:write"}
                            onClick={() => {
                              const copy = JSON.parse(JSON.stringify(route)) as ManagedRoute;
                              copy.route_id = `${route.route_id}-copy`;
                              mutateDoc({ routes: [...doc.routes.slice(0, i + 1), copy, ...doc.routes.slice(i + 1)] });
                            }}
                          >
                            Duplicate
                          </Button>
                          <Button
                            disabled={!canWrite}
                            title={canWrite ? undefined : "Requires platform:routing:write"}
                            style={{ color: "var(--red)" }}
                            onClick={() => mutateDoc({ routes: doc.routes.filter((_, j) => j !== i) })}
                          >
                            Remove
                          </Button>
                        </div>
                      </Td>
                    </tr>
                  ))}
                </tbody>
              </Table>
            )
          ) : (
            <div style={{ padding: 12 }}>
              <div style={{ fontSize: 11, color: "var(--ink3)", marginBottom: 6 }}>
                Applying JSON replaces the whole document — once applied, this JSON is the source of truth and the
                visual table re-syncs from it.
              </div>
              <textarea
                value={jsonText}
                onChange={(e) => setJsonText(e.target.value)}
                rows={20}
                spellCheck={false}
                style={{
                  width: "100%",
                  resize: "vertical",
                  padding: 10,
                  borderRadius: "var(--radius)",
                  border: "1px solid var(--line)",
                  background: "var(--bg)",
                  color: "var(--ink)",
                  fontFamily: "var(--font-mono)",
                  fontSize: 11.5,
                  lineHeight: 1.6,
                }}
              />
              {jsonError && (
                <div role="alert" style={{ marginTop: 6, padding: "8px 10px", borderRadius: "var(--radius)", background: "var(--red-bg)", color: "var(--red)", fontSize: 12 }}>
                  {jsonError}
                </div>
              )}
              <div style={{ display: "flex", justifyContent: "flex-end", gap: 8, marginTop: 8 }}>
                <Button onClick={() => setView("visual")}>Back to visual</Button>
                <Button
                  variant="primary"
                  disabled={!canWrite}
                  title={canWrite ? undefined : "Requires platform:routing:write"}
                  onClick={applyJson}
                >
                  Apply JSON
                </Button>
              </div>
            </div>
          )}
        </div>

        {/* ---- side column: validation report + probe + lifecycle ---- */}
        <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          <Card title="Validation report">
            {draft.status === "validated" ? (
              <ValidationReportView report={report} hash={draft.validation_hash} />
            ) : (
              <div style={{ fontSize: 11.5, color: "var(--ink3)" }}>Not validated yet — run Validate.</div>
            )}
          </Card>

          {probeOps && <ProbePanel initialOps={probeOps} />}

          <Card title="Publication lifecycle">
            <div style={{ display: "flex", flexDirection: "column", gap: 7, fontSize: 11.5 }}>
              {(
                [
                  ["published", "Publish request durably enqueued — proves nothing about application"],
                  ["rolling_out", "Regions fetching and validating the revision"],
                  ["active", "Receipts confirmed for all required regions"],
                  ["partially_applied", "Some regions applied; others missing or failed"],
                  ["failed", "The rollout failed — state per region is in the receipts"],
                ] as const
              ).map(([status, text]) => (
                <div key={status} style={{ display: "flex", alignItems: "baseline", gap: 8 }}>
                  <span style={{ flex: "none", minWidth: 110 }}>
                    <Badge tone={status === "active" ? "green" : status === "failed" ? "red" : status === "partially_applied" ? "amber" : status === "rolling_out" ? "amber" : "blue"}>
                      {status.replace(/_/g, " ")}
                    </Badge>
                  </span>
                  <span style={{ color: "var(--ink2)" }}>{text}</span>
                </div>
              ))}
            </div>
          </Card>
        </div>
      </div>

      {/* route form drawer */}
      {editTarget && (
        <RouteFormDrawer
          key={`${editIndex}-${editTarget.route_id}`}
          open={formOpen}
          route={editTarget}
          title={editTarget.route_id ? `Edit route ${editTarget.route_id}` : "New route"}
          onClose={() => setFormOpen(false)}
          onSave={(route) => {
            if (editIndex !== null) patchRoute(editIndex, route);
            setFormOpen(false);
          }}
        />
      )}

      {/* action confirm dialogs */}
      <ConfirmDialog
        open={pending === "save"}
        onClose={() => setPending(null)}
        onConfirm={(reason) => void run("save", reason)}
        title={`Save draft ${draft.id}`}
        description="Replaces the draft document server-side (PUT with the current draft revision as expected_revision)."
        confirmLabel="Save draft"
      />
      <ConfirmDialog
        open={pending === "validate"}
        onClose={() => setPending(null)}
        onConfirm={(reason) => void run("validate", reason)}
        title={`Validate draft ${draft.id}`}
        description="Runs full document validation against the live Provider Connection registry and refreshes the validation report."
        confirmLabel="Validate"
      />
      <ConfirmDialog
        open={pending === "probe"}
        onClose={() => setPending(null)}
        onConfirm={(reason) => void run("probe", reason)}
        title={`Probe connections for ${draft.id}`}
        description="Requests a probe operation on every Provider Connection referenced by the draft. Operations are polled until terminal."
        confirmLabel="Probe connections"
      />
      <ConfirmDialog
        open={pending === "publish"}
        onClose={() => setPending(null)}
        onConfirm={(reason) => void run("publish", reason)}
        title={`Publish draft ${draft.id}`}
        description={
          <span>
            <span style={{ display: "block", marginBottom: 8 }}>
              Publishing durably enqueues the revision for all required regions. Acceptance of the enqueue does NOT mean
              every gateway has applied it — track per-region receipts.
            </span>
            <span style={{ display: "block", fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 3 }}>
              Required regions (comma-separated, optional)
            </span>
            <input
              style={{ ...inputStyle, width: "100%" }}
              placeholder="us-west, us-east, eu-west, ap-southeast"
              value={publishRegions}
              onChange={(e) => setPublishRegions(e.target.value)}
            />
          </span>
        }
        confirmLabel="Publish revision"
      />
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/* Probe panel — polls each operation every 1.5s until terminal        */
/* ------------------------------------------------------------------ */

const PROBE_POLL_MS = 1500;

function ProbePanel({ initialOps }: { initialOps: Operation[] }) {
  const [ops, setOps] = useState<Record<string, Operation>>(() =>
    Object.fromEntries(initialOps.map((op) => [op.id, op])),
  );

  useEffect(() => {
    let cancelled = false;
    let timer: number | undefined;

    const tick = async () => {
      const current = Object.values(ops);
      const pendingOps = current.filter((op) => !isTerminalOperation(op.status));
      if (pendingOps.length === 0) return;
      const updates = await Promise.all(
        pendingOps.map(async (op) => {
          try {
            return await getOperation(op.id);
          } catch {
            return null; // transient poll failure — retry next tick
          }
        }),
      );
      if (cancelled) return;
      setOps((prev) => {
        const next = { ...prev };
        for (const update of updates) {
          if (update) next[update.id] = update;
        }
        return next;
      });
      timer = window.setTimeout(tick, PROBE_POLL_MS);
    };

    timer = window.setTimeout(tick, PROBE_POLL_MS);
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
    };
  }, [ops]);

  const list = Object.values(ops).sort((a, b) => a.connection_id.localeCompare(b.connection_id));
  const allTerminal = list.every((op) => isTerminalOperation(op.status));

  return (
    <Card
      title={
        <span style={{ display: "inline-flex", alignItems: "center", gap: 8 }}>
          Connection probes
          {!allTerminal && <Spinner size={12} />}
        </span>
      }
    >
      {list.length === 0 ? (
        <div style={{ fontSize: 11.5, color: "var(--ink3)" }}>No connections referenced by this draft.</div>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
          {list.map((op) => (
            <div key={op.id} style={{ border: "1px solid var(--line)", borderRadius: "var(--radius)", padding: "8px 10px" }}>
              <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                <span style={{ fontFamily: "var(--font-mono)", fontSize: 11.5 }}>{op.connection_id}</span>
                <span style={{ fontFamily: "var(--font-mono)", fontSize: 10.5, color: "var(--ink3)" }}>{truncateId(op.id)}</span>
                <span style={{ flex: 1 }} />
                <Badge tone={operationTone(op.status)}>{op.status}</Badge>
              </div>
              {op.status === "failed" && (
                <div style={{ marginTop: 4, fontSize: 11.5, color: "var(--red)" }}>
                  {op.error_code ? `${op.error_code}: ` : ""}
                  {op.error_message ?? "probe failed"}
                </div>
              )}
              {op.status === "succeeded" && op.result && (
                <div style={{ marginTop: 4, fontFamily: "var(--font-mono)", fontSize: 10.5, color: "var(--ink3)", wordBreak: "break-all" }}>
                  {JSON.stringify(op.result)}
                </div>
              )}
            </div>
          ))}
        </div>
      )}
    </Card>
  );
}
