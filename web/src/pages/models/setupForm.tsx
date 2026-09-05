import { useEffect, useState } from "react";
import { apiGet, ApiError } from "../../api/client";
import { ErrorBanner, Modal } from "../../components/feedback";
import { Button } from "../../components/ui";
import type { ManagedRoute, TenantVisibilityPolicy } from "../routing/types";
import type { ProviderConnection } from "../providers/types";
import { configureModel, modalityWarning, setupProblems, type ModelSettings } from "./setup";

const inputStyle = { width: "100%", padding: 8, background: "var(--panel)", color: "var(--ink)", border: "1px solid var(--line)", borderRadius: 6 };

export function ModelSetup({ routes, templates, onClose, onApply, busy = false, submitLabel = "Apply setup" }: {
  routes: ManagedRoute[]; templates: ManagedRoute[]; onClose: () => void;
  onApply: (routes: ManagedRoute[]) => Promise<void>; busy?: boolean; submitLabel?: string;
}) {
  const [connections, setConnections] = useState<ProviderConnection[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<ApiError | null>(null);
  const [attempted, setAttempted] = useState(false);
  const [visibilityId, setVisibilityId] = useState("");
  const [settings, setSettings] = useState<ModelSettings[]>(() => routes.map(route => ({
    include: true, textConfirmed: false, streaming: false,
    input: route.provider_cost_snapshot.source ? String(route.provider_cost_snapshot.input_per_million_micros / 1_000_000) : "",
    output: route.provider_cost_snapshot.source ? String(route.provider_cost_snapshot.output_per_million_micros / 1_000_000) : "",
    cached: route.provider_cost_snapshot.source ? String(route.provider_cost_snapshot.cached_input_per_million_micros / 1_000_000) : "",
    source: route.provider_cost_snapshot.source ?? "",
  })));
  useEffect(() => {
    let cancelled = false;
    void Promise.all([...new Set(routes.map(r => r.provider_connection_id))].map(id => apiGet<ProviderConnection>(`/control/v1/provider-connections/${encodeURIComponent(id)}`)))
      .then(cs => { if (!cancelled) setConnections(cs); })
      .catch(err => { if (!cancelled) setError(err instanceof ApiError ? err : new ApiError(0, "request_failed", "Could not load provider connections.")); })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, [routes]);
  const patch = (index: number, update: Partial<ModelSettings>) => setSettings(ss => ss.map((s, i) => i === index ? { ...s, ...update } : s));
  const visibility: TenantVisibilityPolicy | undefined = visibilityId === "all" ? { all_tenants: true } : templates.find(r => r.route_id === visibilityId)?.tenant_visibility_policy;
  const count = settings.filter(s => s.include).length;
  const apply = async () => {
    setAttempted(true);
    if (!visibility || !count || routes.some((r, i) => setupProblems(r, settings[i], connections.find(c => c.id === r.provider_connection_id)).length)) return;
    setError(null);
    try { await onApply(routes.flatMap((r, i) => settings[i].include ? [configureModel(r, settings[i], connections.find(c => c.id === r.provider_connection_id)!, visibility)] : [])); }
    catch (err) { setError(err instanceof ApiError ? err : new ApiError(0, "setup_failed", "Could not save model setup. Please try again.")); }
  };
  return <Modal open title="Configure text models" onClose={() => { if (!busy) onClose(); }} footer={<>
    <Button disabled={busy} onClick={onClose}>Back</Button>
    <Button variant="primary" disabled={busy || loading || !connections.length} onClick={() => void apply()}>{busy ? "Saving…" : submitLabel} ({count})</Button>
  </>}>
    <div style={{ display: "grid", gap: 16 }}>
      <p>Select → Configure → Validate → Publish. Provider discovery supplies model names; confirm capabilities and enter your actual prices below.</p>
      {error && <ErrorBanner error={error} />}
      <label>Who can use these models after publication?
        <select aria-label="Model visibility" style={inputStyle} value={visibilityId} disabled={busy} onChange={e => setVisibilityId(e.target.value)}>
          <option value="">Choose tenant access…</option>
          <option value="all">All tenants</option>
          {templates.filter(r => r.tenant_visibility_policy.all_tenants || r.tenant_visibility_policy.tenant_ids?.length).map(r => <option key={r.route_id} value={r.route_id}>Same access as {r.public_model} ({r.route_id})</option>)}
        </select>
      </label>
      {visibility && <div style={{ fontSize: 12 }}>Access: {visibility.all_tenants ? "every tenant" : visibility.tenant_ids?.join(", ")}. Routes become active only after you publish the validated draft.</div>}
      {attempted && !visibility && <p role="alert">Choose tenant access before continuing.</p>}
      {routes.map((route, i) => {
        const s = settings[i];
        const warning = modalityWarning(route.provider_model);
        const connection = connections.find(c => c.id === route.provider_connection_id);
        const problems = attempted ? setupProblems(route, s, connection) : [];
        return <fieldset key={route.route_id} disabled={busy} style={{ border: "1px solid var(--line)", borderRadius: 8, padding: 12, minWidth: 0 }}>
          <legend><label><input type="checkbox" checked={s.include} onChange={e => patch(i, { include: e.target.checked })} /> {route.public_model}</label></legend>
          {!s.include ? <p>This model will be excluded from this draft. The provider inventory is unchanged.</p> : <div style={{ display: "grid", gap: 10 }}>
            {warning ? <p role="note" style={{ color: "var(--amber)" }}>{warning}</p> : <>
              <label><input type="checkbox" checked={s.textConfirmed} onChange={e => patch(i, { textConfirmed: e.target.checked })} /> I confirm this model supports native text requests on this connection.</label>
              {connection?.capability_declaration.features.streaming === "native" && <label><input type="checkbox" checked={s.streaming} onChange={e => patch(i, { streaming: e.target.checked })} /> Enable native streaming (model support confirmed)</label>}
              <div style={{ fontSize: 12 }}>USD per 1 million tokens. Enter your provider or contract rates; unknown prices are never filled with zero.</div>
              {([['input', 'Input'], ['output', 'Output'], ['cached', 'Cached input']] as const).map(([key, label]) => <label key={key}>{label} price<input aria-label={`${route.public_model} ${label} price`} style={inputStyle} inputMode="decimal" placeholder="e.g. 1.25" value={s[key]} onChange={e => patch(i, { [key]: e.target.value })} /></label>)}
              <label>Price source<input aria-label={`${route.public_model} Price source`} style={inputStyle} placeholder="Provider pricing URL or contract reference" value={s.source} onChange={e => patch(i, { source: e.target.value })} /></label>
            </>}
            {!!problems.length && <ul role="alert">{problems.map(p => <li key={p}>{p}</li>)}</ul>}
          </div>}
        </fieldset>;
      })}
      {attempted && !count && <p role="alert">Keep at least one text model.</p>}
      <div style={{ fontSize: 12 }}>Snapshot IDs and effective times are generated when you continue. Existing routes outside this setup are preserved. Publication remains a separate step.</div>
    </div>
  </Modal>;
}
