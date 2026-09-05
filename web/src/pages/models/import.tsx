import { useEffect, useState } from "react";
import { apiGet, apiSend, ApiError } from "../../api/client";
import { Modal, ErrorBanner } from "../../components/feedback";
import { Button, Spinner } from "../../components/ui";
import { createDraft, getCurrentRevision } from "../routing/api";
import { blankRoute } from "../routing/routeForm";
import type { Draft, ManagedRoute } from "../routing/types";
import type { ProviderConnection, ProviderOperation } from "../providers/types";

interface Model { id: string; owned_by: string }
interface ModelPage { data: Model[]; next_cursor?: string }
const style = { width: "100%", padding: 8, background: "var(--panel)", color: "var(--ink)", border: "1px solid var(--line)", borderRadius: 6 };

// Discovery supplies identity only. Incomplete capabilities and cost evidence
// deliberately fail publication validation until reviewed in the route editor.
export function importedRoute(connection: ProviderConnection, model: Model): ManagedRoute {
  const route = blankRoute();
  return { ...route, route_id: `route-${crypto.randomUUID()}`, public_model: model.id,
    provider_model: model.id, provider_connection_id: connection.id,
    execution_region: connection.region, home_region: connection.region,
    capability_profile_revision: connection.capability_declaration.revision,
    administrative_status: "disabled", tenant_visibility_policy: { all_tenants: false, tenant_ids: [] },
    provider_cost_snapshot: { ...route.provider_cost_snapshot, provider: connection.provider, model: model.id, region: connection.region },
  };
}

export function ImportModels({ onClose, onCreated }: { onClose: () => void; onCreated: (draft: Draft) => void }) {
  const [draftId] = useState(() => `rcd_${crypto.randomUUID()}`);
  const [connections, setConnections] = useState<ProviderConnection[]>([]);
  const [connectionId, setConnectionId] = useState("");
  const [operation, setOperation] = useState<ProviderOperation | null>(null);
  const [models, setModels] = useState<Model[]>([]);
  const [cursor, setCursor] = useState<string | undefined>();
  const [selected, setSelected] = useState<Record<string, Model>>({});
  const [filter, setFilter] = useState("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<ApiError | null>(null);
  const [message, setMessage] = useState("");
  const [pollRetry, setPollRetry] = useState(0);
  const fail = (err: unknown) => setError(err instanceof ApiError ? err : new ApiError(0, "request_failed", "Request failed. Please try again."));
  const connection = connections.find(c => c.id === connectionId);
  const running = operation?.status === "queued" || operation?.status === "running";

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        let next = "";
        const all: ProviderConnection[] = [];
        do {
          const page = await apiGet<{ data: ProviderConnection[]; next_cursor?: string }>(`/control/v1/provider-connections?limit=100&cursor=${encodeURIComponent(next)}`);
          all.push(...page.data); next = page.next_cursor ?? "";
        } while (next && !cancelled);
        if (!cancelled) setConnections(all);
      } catch (err) { if (!cancelled) fail(err); }
      finally { if (!cancelled) setLoading(false); }
    })();
    return () => { cancelled = true; };
  }, []);

  useEffect(() => {
    if (!operation || !running) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    const deadline = Date.now() + 120_000;
    const poll = async () => {
      if (Date.now() > deadline) { setError(new ApiError(0, "discovery_pending", "Discovery is still pending. Resume checking without starting another operation.")); return; }
      try {
        const current = await apiGet<ProviderOperation>(`/control/v1/provider-operations/${encodeURIComponent(operation.id)}`);
        if (cancelled) return;
        if (current.status === "succeeded") {
          const page = await apiGet<ModelPage>(`/control/v1/provider-operations/${encodeURIComponent(current.id)}/models?limit=100`);
          if (cancelled) return;
          setModels(page.data); setCursor(page.next_cursor); setOperation(current);
        } else if (current.status === "failed" || current.status === "uncertain") {
          setOperation(current); setMessage(`Discovery ${current.status}: ${current.error_code ?? "provider_operation_failed"}. Refresh the connection and try again.`);
        } else { timer = setTimeout(poll, 1500); }
      } catch (err) { if (!cancelled) fail(err); }
    };
    void poll();
    return () => { cancelled = true; clearTimeout(timer); };
  }, [operation?.id, running, pollRetry]);

  const discover = async () => {
    if (!connection) return;
    setBusy(true); setError(null); setMessage(""); setSelected({}); setModels([]); setCursor(undefined); setOperation(null);
    try {
      const fresh = await apiGet<ProviderConnection>(`/control/v1/provider-connections/${encodeURIComponent(connection.id)}`);
      setConnections(cs => cs.map(c => c.id === fresh.id ? fresh : c));
      setOperation(await apiSend<ProviderOperation>("POST", `/control/v1/provider-connections/${encodeURIComponent(fresh.id)}/model-discoveries`, {
        expected_revision: fresh.revision, reason: "Discover provider models for catalog import",
      }));
    } catch (err) { fail(err); }
    finally { setBusy(false); }
  };
  const more = async () => {
    if (!cursor || !operation) return;
    setBusy(true); setError(null);
    try {
      const page = await apiGet<ModelPage>(`/control/v1/provider-operations/${encodeURIComponent(operation.id)}/models?limit=100&cursor=${encodeURIComponent(cursor)}`);
      setModels(ms => [...ms, ...page.data]); setCursor(page.next_cursor);
    } catch (err) { fail(err); } finally { setBusy(false); }
  };
  const submit = async () => {
    if (!connection || !operation || operation.status !== "succeeded") return;
    setBusy(true); setError(null);
    try {
      const [head, fresh] = await Promise.all([getCurrentRevision().catch(err => { if (err instanceof ApiError && err.status === 404) return { revision: 0, document: { routes: [] } }; throw err; }), apiGet<ProviderConnection>(`/control/v1/provider-connections/${encodeURIComponent(connection.id)}`)]);
      if (fresh.revision !== operation.expected_revision) throw new ApiError(409, "revision_conflict", "Connection changed since discovery. Fetch models again.");
      const additions = Object.values(selected).filter(model => !head.document.routes.some(route => route.provider_connection_id === connection.id && route.provider_model === model.id && route.execution_region === fresh.region));
      if (!additions.length) { setMessage("The selected models already have routes in the active catalog."); return; }
      onCreated(await createDraft({ id: draftId, base_revision: head.revision,
        document: { ...head.document, routes: [...head.document.routes, ...additions.map(model => importedRoute(fresh, model))] },
        reason: `${reason.trim()} (discovery ${operation.id})`,
      }));
    } catch (err) { fail(err); } finally { setBusy(false); }
  };
  return <Modal open title="Import models from provider" onClose={() => { if (!busy) onClose(); }} footer={<>
    <Button disabled={busy} onClick={onClose}>Cancel</Button>
    <Button variant="primary" disabled={busy || running || operation?.status !== "succeeded" || !Object.keys(selected).length || !reason.trim()} onClick={() => void submit()}>Create draft ({Object.keys(selected).length})</Button>
  </>}>
    <div style={{ display: "grid", gap: 12 }}>
      {error && <ErrorBanner error={error} retry={running ? () => { setError(null); setPollRetry(n => n + 1); } : undefined} />}
      <label>Provider connection<select aria-label="Provider connection" style={style} value={connectionId} disabled={busy || running} onChange={e => { setConnectionId(e.target.value); setModels([]); setSelected({}); setOperation(null); setCursor(undefined); setMessage(""); setError(null); }}>
        <option value="">{loading ? "Loading connections…" : "Select a connection"}</option>
        {connections.map(c => <option key={c.id} value={c.id}>{c.display_name || c.id} · {c.provider} · {c.region}</option>)}
      </select></label>
      {!loading && !connections.length && <p>No connections found. Add one in Provider Connections first.</p>}
      <Button disabled={!connection || busy || running} onClick={() => void discover()}>{busy || running ? <Spinner size={12} /> : null} {running ? "Fetching models…" : "Fetch provider models"}</Button>
      <div style={{ fontSize: 12, color: "var(--ink2)" }}>Uses the connection’s credentials on the server to fetch model names. Review capabilities, pricing, visibility and status in the draft before publishing.</div>
      {message && <p role="status">{message}</p>}
      {operation?.status === "succeeded" && <>
        <div>{models.length} models loaded{cursor ? " · more available" : ""}</div>
        <input aria-label="Filter discovered models" style={style} placeholder="Filter loaded models…" value={filter} onChange={e => setFilter(e.target.value)} />
        <div style={{ maxHeight: 260, overflow: "auto" }}>
          {models.filter(m => m.id.toLowerCase().includes(filter.toLowerCase())).map(m => <label key={m.id} style={{ display: "flex", gap: 8, padding: "6px 0", overflowWrap: "anywhere" }}>
            <input type="checkbox" checked={!!selected[m.id]} onChange={e => setSelected(prev => { const next = { ...prev }; if (e.target.checked) next[m.id] = m; else delete next[m.id]; return next; })} />{m.id}
          </label>)}
          {!models.length && <p>The provider returned no models.</p>}
        </div>
        {cursor && <Button disabled={busy} onClick={() => void more()}>Load more models</Button>}
        <label>Reason<input aria-label="Import reason" style={style} value={reason} onChange={e => setReason(e.target.value)} placeholder="Why are these models being added?" /></label>
        <div style={{ fontSize: 12 }}>Imported routes start disabled. Existing routes are preserved; duplicates on this connection and region are skipped.</div>
      </>}
    </div>
  </Modal>;
}
