/** Route edit drawer: form for every ManagedRoute field, used by the draft editor. */
import { useState, type CSSProperties, type ReactNode } from "react";
import { Drawer } from "../../components/feedback";
import { Button } from "../../components/ui";
import type { CapabilitySupport, ManagedRoute, RouteAdministrativeStatus } from "./types";

const inputStyle: CSSProperties = {
  width: "100%",
  padding: "6px 8px",
  borderRadius: "var(--radius)",
  border: "1px solid var(--line)",
  background: "var(--bg)",
  color: "var(--ink)",
  fontSize: 12,
};

const monoInputStyle: CSSProperties = { ...inputStyle, fontFamily: "var(--font-mono)", fontSize: 12 };

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label style={{ display: "block" }}>
      <span style={{ display: "block", fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 3 }}>{label}</span>
      {children}
    </label>
  );
}

function blankRoute(): ManagedRoute {
  return {
    route_id: "",
    public_model: "",
    provider_connection_id: "",
    provider_model: "",
    execution_region: "",
    home_region: "",
    capability_profile_revision: 1,
    capabilities: {},
    provider_cost_snapshot: {
      id: "",
      provider: "",
      model: "",
      region: "",
      currency: "USD",
      input_per_million_micros: 0,
      cached_input_per_million_micros: 0,
      cache_write_per_million_micros: 0,
      output_per_million_micros: 0,
    },
    administrative_status: "active",
    selection_policy: { priority: 100, weight: 1 },
    tenant_visibility_policy: { all_tenants: true },
    cache_usage_reliable: false,
    cache_protection_policy: { enabled: false },
  };
}

export { blankRoute };

export function RouteFormDrawer({
  open,
  route,
  title,
  onSave,
  onClose,
}: {
  open: boolean;
  /** Initial route; the drawer is remounted per edit target via `key` from the parent. */
  route: ManagedRoute;
  title: string;
  onSave: (route: ManagedRoute) => void;
  onClose: () => void;
}) {
  const [form, setForm] = useState<ManagedRoute>(() => JSON.parse(JSON.stringify(route)) as ManagedRoute);
  const [caps, setCaps] = useState<Array<{ name: string; support: CapabilitySupport }>>(() =>
    Object.entries(route.capabilities ?? {}).map(([name, support]) => ({ name, support })),
  );
  const [tenantIds, setTenantIds] = useState(() => (route.tenant_visibility_policy.tenant_ids ?? []).join(", "));
  const [error, setError] = useState<string | null>(null);

  const patch = (partial: Partial<ManagedRoute>) => setForm((f) => ({ ...f, ...partial }));
  const patchCost = (partial: Partial<ManagedRoute["provider_cost_snapshot"]>) =>
    setForm((f) => ({ ...f, provider_cost_snapshot: { ...f.provider_cost_snapshot, ...partial } }));
  const patchSelection = (partial: Partial<ManagedRoute["selection_policy"]>) =>
    setForm((f) => ({ ...f, selection_policy: { ...f.selection_policy, ...partial } }));

  const num = (value: string): number => {
    const n = Number(value);
    return Number.isFinite(n) ? n : 0;
  };

  const save = () => {
    const required: Array<[string, string]> = [
      ["route_id", form.route_id],
      ["public_model", form.public_model],
      ["provider_connection_id", form.provider_connection_id],
      ["provider_model", form.provider_model],
      ["execution_region", form.execution_region],
      ["home_region", form.home_region],
    ];
    const missing = required.filter(([, v]) => v.trim().length === 0).map(([k]) => k);
    if (missing.length > 0) {
      setError(`Required fields missing: ${missing.join(", ")}`);
      return;
    }
    const capabilities: Record<string, CapabilitySupport> = {};
    for (const cap of caps) {
      const name = cap.name.trim();
      if (name) capabilities[name] = cap.support;
    }
    const saved: ManagedRoute = {
      ...form,
      route_id: form.route_id.trim(),
      public_model: form.public_model.trim(),
      provider_connection_id: form.provider_connection_id.trim(),
      provider_model: form.provider_model.trim(),
      execution_region: form.execution_region.trim(),
      home_region: form.home_region.trim(),
      capabilities,
      tenant_visibility_policy: {
        all_tenants: form.tenant_visibility_policy.all_tenants,
        limit_policy_revisions: form.tenant_visibility_policy.all_tenants ? undefined : Object.fromEntries(tenantIds.split(",").map(s => s.trim()).filter(Boolean).map(id => [id, form.tenant_visibility_policy.limit_policy_revisions?.[id] ?? 0])),
        tenant_ids: form.tenant_visibility_policy.all_tenants
          ? undefined
          : tenantIds.split(",").map((s) => s.trim()).filter(Boolean),
      },
      embedding_path: form.embedding_path?.trim() || undefined,
      moderation_path: form.moderation_path?.trim() || undefined,
      rerank_path: form.rerank_path?.trim() || undefined,
      embedding_dimensions: form.embedding_dimensions || undefined,
    };
    onSave(saved);
  };

  return (
    <Drawer open={open} onClose={onClose} title={title} width={520}>
      <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
        {error && (
          <div role="alert" style={{ padding: "8px 10px", borderRadius: "var(--radius)", background: "var(--red-bg)", color: "var(--red)", fontSize: 12 }}>
            {error}
          </div>
        )}

        <Field label="Route ID (required)">
          <input style={monoInputStyle} value={form.route_id} onChange={(e) => patch({ route_id: e.target.value })} placeholder="route_fast_chat_usw" />
        </Field>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 10 }}>
          <Field label="Public model (required)">
            <input style={monoInputStyle} value={form.public_model} onChange={(e) => patch({ public_model: e.target.value })} placeholder="fast-chat" />
          </Field>
          <Field label="Provider model (required)">
            <input style={monoInputStyle} value={form.provider_model} onChange={(e) => patch({ provider_model: e.target.value })} placeholder="gpt-4o-mini" />
          </Field>
        </div>
        <Field label="Provider connection ID (required)">
          <input style={monoInputStyle} value={form.provider_connection_id} onChange={(e) => patch({ provider_connection_id: e.target.value })} placeholder="pcn_…" />
        </Field>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 10 }}>
          <Field label="Execution region (required)">
            <input style={monoInputStyle} value={form.execution_region} onChange={(e) => patch({ execution_region: e.target.value })} placeholder="us-west" />
          </Field>
          <Field label="Home region (required)">
            <input style={monoInputStyle} value={form.home_region} onChange={(e) => patch({ home_region: e.target.value })} placeholder="us-west" />
          </Field>
        </div>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 10 }}>
          <Field label="Administrative status">
            <select
              style={inputStyle}
              value={form.administrative_status}
              onChange={(e) => patch({ administrative_status: e.target.value as RouteAdministrativeStatus })}
            >
              <option value="active">active</option>
              <option value="draining">draining</option>
              <option value="disabled">disabled</option>
            </select>
          </Field>
          <Field label="Capability profile revision">
            <input style={monoInputStyle} type="number" min={0} value={form.capability_profile_revision} onChange={(e) => patch({ capability_profile_revision: num(e.target.value) })} />
          </Field>
        </div>

        <div>
          <div style={{ fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 6 }}>CAPABILITIES</div>
          <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
            {caps.map((cap, i) => (
              <div key={i} style={{ display: "flex", gap: 6, alignItems: "center" }}>
                <input
                  style={{ ...monoInputStyle, flex: 1 }}
                  value={cap.name}
                  placeholder="streaming"
                  onChange={(e) => setCaps((c) => c.map((x, j) => (j === i ? { ...x, name: e.target.value } : x)))}
                />
                <select
                  style={{ ...inputStyle, width: 130 }}
                  value={cap.support}
                  onChange={(e) => setCaps((c) => c.map((x, j) => (j === i ? { ...x, support: e.target.value as CapabilitySupport } : x)))}
                >
                  <option value="native">native</option>
                  <option value="translated">translated</option>
                  <option value="unsupported">unsupported</option>
                </select>
                <Button onClick={() => setCaps((c) => c.filter((_, j) => j !== i))}>Remove</Button>
              </div>
            ))}
            <div>
              <Button onClick={() => setCaps((c) => [...c, { name: "", support: "native" }])}>Add capability</Button>
            </div>
          </div>
        </div>

        <div>
          <div style={{ fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 6 }}>SELECTION POLICY</div>
          <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr 1fr", gap: 10 }}>
            <Field label="Priority">
              <input style={monoInputStyle} type="number" value={form.selection_policy.priority} onChange={(e) => patchSelection({ priority: num(e.target.value) })} />
            </Field>
            <Field label="Weight">
              <input style={monoInputStyle} type="number" value={form.selection_policy.weight} onChange={(e) => patchSelection({ weight: num(e.target.value) })} />
            </Field>
            <Field label="Max concurrency">
              <input style={monoInputStyle} type="number" min={0} value={form.selection_policy.max_concurrency ?? 0} onChange={(e) => patchSelection({ max_concurrency: num(e.target.value) || undefined })} />
            </Field>
          </div>
          <label style={{ display: "flex", alignItems: "center", gap: 6, marginTop: 8, fontSize: 12, color: "var(--ink2)" }}>
            <input type="checkbox" checked={form.selection_policy.sticky_routing_eligible ?? false} onChange={(e) => patchSelection({ sticky_routing_eligible: e.target.checked })} />
            Sticky routing eligible
          </label>
        </div>

        <div>
          <div style={{ fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 6 }}>TENANT VISIBILITY</div>
          <label style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 12, color: "var(--ink2)" }}>
            <input
              type="checkbox"
              checked={form.tenant_visibility_policy.all_tenants ?? false}
              onChange={(e) => patch({ tenant_visibility_policy: { ...form.tenant_visibility_policy, all_tenants: e.target.checked } })}
            />
            Visible to all tenants
          </label>
          {!form.tenant_visibility_policy.all_tenants && (
            <div style={{ marginTop: 8 }}>
              <Field label="Tenant IDs (comma-separated)">
                <input style={monoInputStyle} value={tenantIds} onChange={(e) => setTenantIds(e.target.value)} placeholder="tn_…, tn_…" />
              </Field>
              {[...new Set(tenantIds.split(",").map(s => s.trim()).filter(Boolean))].map(id => <Field key={id} label={`${id} Limit Policy revision (required)`}>
                <input style={monoInputStyle} type="number" min={1} step={1} value={form.tenant_visibility_policy.limit_policy_revisions?.[id] ?? ""}
                  onChange={e => patch({ tenant_visibility_policy: { ...form.tenant_visibility_policy, limit_policy_revisions: { ...form.tenant_visibility_policy.limit_policy_revisions, [id]: num(e.target.value) } } })} />
              </Field>)}
            </div>
          )}
        </div>

        <div>
          <div style={{ fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 6 }}>CACHE</div>
          <label style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 12, color: "var(--ink2)" }}>
            <input type="checkbox" checked={form.cache_usage_reliable} onChange={(e) => patch({ cache_usage_reliable: e.target.checked })} />
            Cache usage metering reliable
          </label>
          <label style={{ display: "flex", alignItems: "center", gap: 6, marginTop: 6, fontSize: 12, color: "var(--ink2)" }}>
            <input
              type="checkbox"
              checked={form.cache_protection_policy.enabled}
              onChange={(e) => patch({ cache_protection_policy: { ...form.cache_protection_policy, enabled: e.target.checked } })}
            />
            Cache protection enabled
          </label>
          {form.cache_protection_policy.enabled && (
            <div style={{ marginTop: 8, maxWidth: 200 }}>
              <Field label="Cache TTL (seconds)">
                <input
                  style={monoInputStyle}
                  type="number"
                  min={0}
                  value={form.cache_protection_policy.ttl_seconds ?? 0}
                  onChange={(e) => patch({ cache_protection_policy: { ...form.cache_protection_policy, ttl_seconds: num(e.target.value) } })}
                />
              </Field>
            </div>
          )}
        </div>

        <div>
          <div style={{ fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 6 }}>COST SNAPSHOT (USD micros per 1M units)</div>
          <Field label="Price source (URL or reference)">
            <input style={monoInputStyle} value={form.provider_cost_snapshot.source ?? ""} onChange={e => patchCost({ source: e.target.value })} />
          </Field>
          <Field label="Effective at (Unix seconds)">
            <input style={monoInputStyle} type="number" min={1} value={form.provider_cost_snapshot.effective_at ?? ""} onChange={e => patchCost({ effective_at: num(e.target.value) })} />
          </Field>
          <Field label="Snapshot ID">
            <input style={monoInputStyle} value={form.provider_cost_snapshot.id} onChange={(e) => patchCost({ id: e.target.value })} placeholder="price_…" />
          </Field>
          <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 10, marginTop: 8 }}>
            <Field label="Input / 1M">
              <input style={monoInputStyle} type="number" min={0} value={form.provider_cost_snapshot.input_per_million_micros} onChange={(e) => patchCost({ input_per_million_micros: num(e.target.value) })} />
            </Field>
            <Field label="Output / 1M">
              <input style={monoInputStyle} type="number" min={0} value={form.provider_cost_snapshot.output_per_million_micros} onChange={(e) => patchCost({ output_per_million_micros: num(e.target.value) })} />
            </Field>
            <Field label="Cached input / 1M">
              <input style={monoInputStyle} type="number" min={0} value={form.provider_cost_snapshot.cached_input_per_million_micros} onChange={(e) => patchCost({ cached_input_per_million_micros: num(e.target.value) })} />
            </Field>
            <Field label="Cache write / 1M">
              <input style={monoInputStyle} type="number" min={0} value={form.provider_cost_snapshot.cache_write_per_million_micros} onChange={(e) => patchCost({ cache_write_per_million_micros: num(e.target.value) })} />
            </Field>
          </div>
        </div>

        <div>
          <div style={{ fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 6 }}>MODALITY PATHS (OPTIONAL)</div>
          <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 10 }}>
            <Field label="Embedding path">
              <input style={monoInputStyle} value={form.embedding_path ?? ""} onChange={(e) => patch({ embedding_path: e.target.value })} />
            </Field>
            <Field label="Embedding dimensions">
              <input style={monoInputStyle} type="number" min={0} value={form.embedding_dimensions ?? 0} onChange={(e) => patch({ embedding_dimensions: num(e.target.value) || undefined })} />
            </Field>
            <Field label="Moderation path">
              <input style={monoInputStyle} value={form.moderation_path ?? ""} onChange={(e) => patch({ moderation_path: e.target.value })} />
            </Field>
            <Field label="Rerank path">
              <input style={monoInputStyle} value={form.rerank_path ?? ""} onChange={(e) => patch({ rerank_path: e.target.value })} />
            </Field>
          </div>
        </div>

        <div style={{ display: "flex", justifyContent: "flex-end", gap: 8, borderTop: "1px solid var(--line)", paddingTop: 12 }}>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" onClick={save}>Apply to draft</Button>
        </div>
      </div>
    </Drawer>
  );
}
