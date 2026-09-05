/**
 * Models & Capabilities — public models routable on the active catalog
 * revision. Joins /llm/models (tenant-visible) with the control-plane routing
 * catalog and provider connections; all filtering is client-side over the join.
 */
import { useMemo, useState, type CSSProperties, type ReactNode } from "react";
import { ImportModels } from "./import";
import { useAuth } from "../../auth/AuthProvider";
import { PERMISSIONS } from "../../auth/permissions";
import { DraftEditor } from "../routing/drafts";
import { PublicationTracker } from "../routing/publication";
import type { Draft } from "../routing/types";
import { apiGet } from "../../api/client";
import { Drawer, ErrorBanner, Loading } from "../../components/feedback";
import { Badge, Button, Card, CodeBlock, CopyButton, EmptyState, KeyValueList, Table, Td, Th } from "../../components/ui";
import { formatMicrosUSD, formatDateTime, formatNumber } from "../../lib/format";
import {
  statusTone,
  formatRevision,
  useResource,
  type LLMModelList,
  type ProviderConnection,
  type ProviderConnectionPage,
  type Resource,
} from "../overview/lib";
import {
  deriveFilterOptions,
  EMPTY_FILTERS,
  filterRows,
  joinModels,
  type CatalogRevision,
  type CapabilitySupport,
  type ManagedRoute,
  type ModelFilters,
  type ModelRow,
} from "./lib";

/* ------------------------------------------------------------------ */
/* Capability badge                                                    */
/* ------------------------------------------------------------------ */

const capStyles: Record<CapabilitySupport, CSSProperties> = {
  native: { background: "var(--blue-bg)", color: "var(--blue)" },
  translated: { background: "var(--purple-bg)", color: "var(--purple)" },
  unsupported: { background: "var(--chip)", color: "var(--ink2)" },
};

const capLabels: Record<CapabilitySupport, string> = {
  native: "Native",
  translated: "Translated",
  unsupported: "Unsupported",
};

function CapBadge({ support, prefix }: { support: CapabilitySupport | undefined; prefix?: string }) {
  const base: CSSProperties = { fontSize: 11, fontWeight: 500, padding: "1px 7px", borderRadius: "var(--radius-pill)", whiteSpace: "nowrap" };
  if (!support) {
    return <span style={{ ...base, border: "1px dashed var(--line)", color: "var(--ink3)" }}>{prefix ? `${prefix} ` : ""}Not declared</span>;
  }
  return <span style={{ ...base, ...capStyles[support] }}>{prefix ? `${prefix} ${capLabels[support]}` : capLabels[support]}</span>;
}

/* ------------------------------------------------------------------ */
/* Filter bar                                                          */
/* ------------------------------------------------------------------ */

const selectStyle: CSSProperties = {
  padding: "6px 8px",
  border: "1px solid var(--line)",
  borderRadius: "var(--radius)",
  background: "var(--panel)",
  fontSize: 12,
};

function FilterBar({
  filters,
  onChange,
  options,
  view,
  onView,
}: {
  filters: ModelFilters;
  onChange: (filters: ModelFilters) => void;
  options: ReturnType<typeof deriveFilterOptions>;
  view: "table" | "cards";
  onView: (view: "table" | "cards") => void;
}) {
  const set = (patch: Partial<ModelFilters>) => onChange({ ...filters, ...patch });
  const select = (value: string, allLabel: string, values: string[], patch: (v: string) => void) => (
    <select value={value} onChange={(e) => patch(e.target.value)} style={selectStyle}>
      <option value="">{allLabel}</option>
      {values.map((v) => (
        <option key={v} value={v}>
          {v}
        </option>
      ))}
    </select>
  );
  return (
    <div style={{ display: "flex", flexWrap: "wrap", gap: 8, marginBottom: 12, alignItems: "center" }}>
      <input
        value={filters.search}
        onChange={(e) => set({ search: e.target.value })}
        placeholder="Search model, provider model, or route…"
        style={{ width: 230, padding: "6px 10px", border: "1px solid var(--line)", borderRadius: "var(--radius)", background: "var(--panel)", fontSize: 12 }}
      />
      {select(filters.provider, "All providers", options.providers, (v) => set({ provider: v }))}
      {select(filters.region, "All regions", options.regions, (v) => set({ region: v }))}
      {select(filters.capability, "Any capability", options.capabilities, (v) => set({ capability: v }))}
      {select(filters.status, "Any status", options.statuses, (v) => set({ status: v }))}
      <span style={{ flex: 1 }} />
      <div style={{ display: "flex", border: "1px solid var(--line)", borderRadius: "var(--radius)", overflow: "hidden" }}>
        {(["table", "cards"] as const).map((v) => (
          <button
            key={v}
            onClick={() => onView(v)}
            style={{
              border: "none",
              padding: "5px 12px",
              fontSize: 12,
              cursor: "pointer",
              background: view === v ? "var(--blue)" : "transparent",
              color: view === v ? "#fff" : "var(--ink2)",
              fontWeight: view === v ? 600 : 500,
            }}
          >
            {v === "table" ? "Table" : "Cards"}
          </button>
        ))}
      </div>
    </div>
  );
}

/* ------------------------------------------------------------------ */
/* Table + cards views                                                 */
/* ------------------------------------------------------------------ */

function providerOf(route: ManagedRoute | null, connById: Map<string, ProviderConnection>): string {
  if (!route) return "—";
  return connById.get(route.provider_connection_id)?.provider ?? route.provider_connection_id;
}

function priceOf(route: ManagedRoute | null): string {
  const snapshot = route?.provider_cost_snapshot;
  if (!snapshot || !snapshot.id) return "—";
  return `${formatMicrosUSD(snapshot.input_per_million_micros)} in · ${formatMicrosUSD(snapshot.output_per_million_micros)} out`;
}

function ModelsTable({
  rows,
  capabilities,
  connById,
  onOpen,
}: {
  rows: ModelRow[];
  capabilities: string[];
  connById: Map<string, ProviderConnection>;
  onOpen: (row: ModelRow) => void;
}) {
  return (
    <Card style={{ padding: 0, overflow: "hidden" }}>
      <Table style={{ minWidth: 1080 }}>
        <thead>
          <tr>
            <Th>Public model</Th>
            <Th>Provider</Th>
            <Th>Provider model · region</Th>
            {capabilities.map((c) => (
              <Th key={c}>{c}</Th>
            ))}
            <Th>Status</Th>
            <Th>Price / 1M</Th>
            <Th>{""}</Th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => {
            const route = row.primary;
            return (
              <tr key={row.publicModel} onClick={() => onOpen(row)} style={{ cursor: "pointer" }}>
                <Td mono>
                  <span style={{ fontWeight: 600, color: "var(--purple)" }}>{row.publicModel}</span>
                  {row.routes.length > 1 && (
                    <span style={{ color: "var(--ink3)", fontSize: 11 }}> · {row.routes.length} routes</span>
                  )}
                </Td>
                <Td>{providerOf(route, connById)}</Td>
                <Td mono>
                  {route ? (
                    <span style={{ color: "var(--ink2)" }}>
                      {route.provider_model} · {route.execution_region}
                    </span>
                  ) : (
                    "—"
                  )}
                </Td>
                {capabilities.map((c) => (
                  <Td key={c}>{route ? <CapBadge support={route.capabilities?.[c]} /> : <span style={{ color: "var(--ink3)" }}>—</span>}</Td>
                ))}
                <Td>
                  {route ? (
                    <Badge tone={statusTone(route.administrative_status)}>{route.administrative_status}</Badge>
                  ) : (
                    <Badge tone="neutral">no route visible</Badge>
                  )}
                </Td>
                <Td mono>
                  <span style={{ color: "var(--ink2)", whiteSpace: "nowrap" }}>{priceOf(route)}</span>
                </Td>
                <Td>
                  <span style={{ color: "var(--blue)", fontSize: 12 }}>Details →</span>
                </Td>
              </tr>
            );
          })}
        </tbody>
      </Table>
    </Card>
  );
}

function ModelsCards({
  rows,
  capabilities,
  connById,
  onOpen,
}: {
  rows: ModelRow[];
  capabilities: string[];
  connById: Map<string, ProviderConnection>;
  onOpen: (row: ModelRow) => void;
}) {
  return (
    <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill,minmax(280px,1fr))", gap: 12 }}>
      {rows.map((row) => {
        const route = row.primary;
        return (
          <button
            key={row.publicModel}
            onClick={() => onOpen(row)}
            style={{
              textAlign: "left",
              cursor: "pointer",
              background: "var(--panel)",
              border: "1px solid var(--line)",
              borderRadius: "var(--radius-lg)",
              padding: 14,
              boxShadow: "var(--shadow)",
              display: "flex",
              flexDirection: "column",
              gap: 8,
            }}
          >
            <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
              <span style={{ fontFamily: "var(--font-mono)", fontWeight: 600, color: "var(--purple)" }}>{row.publicModel}</span>
              <span style={{ flex: 1 }} />
              {route ? (
                <Badge tone={statusTone(route.administrative_status)}>{route.administrative_status}</Badge>
              ) : (
                <Badge tone="neutral">no route visible</Badge>
              )}
            </div>
            <div style={{ fontSize: 12, color: "var(--ink2)", fontFamily: "var(--font-mono)" }}>
              {route ? `${providerOf(route, connById)} · ${route.provider_model} · ${route.execution_region}` : "No route visible in the current catalog"}
            </div>
            <div style={{ display: "flex", flexWrap: "wrap", gap: 4 }}>
              {route ? (
                capabilities.map((c) => <CapBadge key={c} support={route.capabilities?.[c]} prefix={c} />)
              ) : (
                <span style={{ fontSize: 11, color: "var(--ink3)" }}>—</span>
              )}
            </div>
            <div style={{ fontSize: 11, color: "var(--ink3)", fontFamily: "var(--font-mono)" }}>{priceOf(route)}</div>
          </button>
        );
      })}
    </div>
  );
}

/* ------------------------------------------------------------------ */
/* Detail drawer                                                       */
/* ------------------------------------------------------------------ */

function visibilityOf(route: ManagedRoute): string {
  const policy = route.tenant_visibility_policy ?? {};
  if (policy.all_tenants) return "All tenants";
  const tenants = policy.tenant_ids?.length ?? 0;
  return tenants > 0 ? `${tenants} tenant(s)` : "No tenants";
}

function CostSnapshot({ route }: { route: ManagedRoute }) {
  const s = route.provider_cost_snapshot;
  if (!s || !s.id) return <div style={{ fontSize: 12, color: "var(--ink3)" }}>No provider cost snapshot on this route.</div>;
  const items: Array<{ key: string; value: ReactNode; mono?: boolean }> = [
    { key: "Input / 1M tokens", value: formatMicrosUSD(s.input_per_million_micros), mono: true },
    { key: "Cached input / 1M tokens", value: formatMicrosUSD(s.cached_input_per_million_micros), mono: true },
    { key: "Cache write / 1M tokens", value: formatMicrosUSD(s.cache_write_per_million_micros), mono: true },
    { key: "Output / 1M tokens", value: formatMicrosUSD(s.output_per_million_micros), mono: true },
  ];
  if (s.embedding_input_per_million_micros) items.push({ key: "Embedding input / 1M", value: formatMicrosUSD(s.embedding_input_per_million_micros), mono: true });
  if (s.moderation_input_per_million_micros) items.push({ key: "Moderation input / 1M", value: formatMicrosUSD(s.moderation_input_per_million_micros), mono: true });
  if (s.rerank_document_per_thousand_micros) items.push({ key: "Rerank / 1K documents", value: formatMicrosUSD(s.rerank_document_per_thousand_micros), mono: true });
  items.push(
    { key: "Currency", value: s.currency, mono: true },
    { key: "Effective at", value: formatDateTime(new Date(s.effective_at * 1000).toISOString()), mono: true },
    { key: "Source", value: s.source, mono: true },
  );
  return <KeyValueList items={items} />;
}

function RouteDetail({ route, connById, revision }: { route: ManagedRoute; connById: Map<string, ProviderConnection>; revision: number | null }) {
  const conn = connById.get(route.provider_connection_id);
  const capabilityKeys = Object.keys(route.capabilities ?? {}).sort();
  return (
    <>
      <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
        <Badge tone="purple">{providerOf(route, connById)}</Badge>
        <Badge tone={statusTone(route.administrative_status)}>{route.administrative_status}</Badge>
      </div>
      <KeyValueList
        items={[
          { key: "Provider model", value: route.provider_model, mono: true },
          { key: "Route ID", value: route.route_id, mono: true },
          { key: "Execution region", value: route.execution_region, mono: true },
          { key: "Home region", value: route.home_region, mono: true },
          {
            key: "Connection",
            value: conn ? `${conn.display_name} (${conn.id})` : route.provider_connection_id,
            mono: true,
          },
          { key: "Catalog revision", value: revision !== null ? formatRevision(revision) : "—", mono: true },
          { key: "Capability profile rev", value: String(route.capability_profile_revision), mono: true },
          { key: "Tenant visibility", value: visibilityOf(route) },
          {
            key: "Cache behavior",
            value: `${route.cache_usage_reliable ? "Cache usage reliable" : "Cache usage unreliable"} · cache protection ${
              route.cache_protection_policy?.enabled ? `on (TTL ${route.cache_protection_policy.ttl_seconds ?? 0}s)` : "off"
            }`,
          },
        ]}
      />
      <div>
        <div style={{ color: "var(--ink3)", fontSize: 11, marginBottom: 6 }}>Capabilities</div>
        {capabilityKeys.length === 0 ? (
          <div style={{ fontSize: 12, color: "var(--ink3)" }}>No capabilities declared on this route.</div>
        ) : (
          <div style={{ display: "flex", flexDirection: "column", gap: 5 }}>
            {capabilityKeys.map((key) => (
              <div key={key} style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 12 }}>
                <span style={{ minWidth: 140 }}>{key}</span>
                <CapBadge support={route.capabilities[key]} />
              </div>
            ))}
          </div>
        )}
      </div>
      <div>
        <div style={{ color: "var(--ink3)", fontSize: 11, marginBottom: 6 }}>Provider cost snapshot</div>
        <CostSnapshot route={route} />
      </div>
      <div>
        <div style={{ color: "var(--ink3)", fontSize: 11, marginBottom: 6 }}>Raw route JSON</div>
        <CodeBlock code={JSON.stringify(route, null, 2)} lang="json" />
      </div>
    </>
  );
}

function ModelDrawer({
  row,
  connById,
  revision,
  onClose,
}: {
  row: ModelRow | null;
  connById: Map<string, ProviderConnection>;
  revision: number | null;
  onClose: () => void;
}) {
  const [routeId, setRouteId] = useState<string | null>(null);
  const route = row ? (row.routes.find((r) => r.route_id === routeId) ?? row.primary) : null;
  return (
    <Drawer
      open={row !== null}
      onClose={() => {
        setRouteId(null);
        onClose();
      }}
      title={
        row ? (
          <span style={{ display: "flex", alignItems: "center", gap: 8 }}>
            <span style={{ fontFamily: "var(--font-mono)", color: "var(--purple)" }}>{row.publicModel}</span>
            <CopyButton text={row.publicModel} label="Copy ID" />
          </span>
        ) : (
          ""
        )
      }
      width={440}
    >
      {row && (
        <div style={{ display: "flex", flexDirection: "column", gap: 14, fontSize: 12 }}>
          {row.model === null && (
            <div style={{ padding: "8px 11px", borderRadius: 8, background: "var(--amber-bg)", color: "var(--amber)", fontSize: 12 }}>
              This route is not currently visible to this tenant (its public model is absent from /llm/models).
            </div>
          )}
          {row.model !== null && route === null && (
            <div style={{ padding: "8px 11px", borderRadius: 8, background: "var(--chip)", color: "var(--ink2)", fontSize: 12 }}>
              No route visible for this model on the current catalog revision.
            </div>
          )}
          {row.model && (
            <KeyValueList
              items={[
                { key: "Owned by", value: row.model.owned_by, mono: true },
                { key: "Created", value: formatDateTime(new Date(row.model.created * 1000).toISOString()), mono: true },
              ]}
            />
          )}
          {row.routes.length > 1 && (
            <div style={{ display: "flex", flexWrap: "wrap", gap: 4 }}>
              {row.routes.map((r) => (
                <Button key={r.route_id} variant={route?.route_id === r.route_id ? "primary" : "ghost"} onClick={() => setRouteId(r.route_id)}>
                  {r.execution_region}
                </Button>
              ))}
            </div>
          )}
          {route && <RouteDetail route={route} connById={connById} revision={revision} />}
          <div style={{ padding: "9px 11px", borderRadius: 8, background: "var(--chip)", fontSize: 12, color: "var(--ink2)" }}>
            Provider credentials and secret references are never shown here. Route internals live in the Routing Catalog.
          </div>
        </div>
      )}
    </Drawer>
  );
}

/* ------------------------------------------------------------------ */
/* Routes not visible to this tenant                                   */
/* ------------------------------------------------------------------ */

function HiddenRoutesSection({
  routes,
  connById,
  onOpen,
}: {
  routes: ManagedRoute[];
  connById: Map<string, ProviderConnection>;
  onOpen: (route: ManagedRoute) => void;
}) {
  const [open, setOpen] = useState(false);
  if (routes.length === 0) return null;
  return (
    <Card style={{ marginTop: 14 }}>
      <button
        onClick={() => setOpen(!open)}
        style={{ border: "none", background: "transparent", padding: 0, cursor: "pointer", fontSize: 13, fontWeight: 600, color: "var(--ink)" }}
      >
        {open ? "▾" : "▸"} Routes not currently visible to this tenant ({formatNumber(routes.length)})
      </button>
      {open && (
        <div style={{ marginTop: 10 }}>
          <Table>
            <thead>
              <tr>
                <Th>Route</Th>
                <Th>Public model</Th>
                <Th>Provider</Th>
                <Th>Provider model · region</Th>
                <Th>Status</Th>
                <Th>{""}</Th>
              </tr>
            </thead>
            <tbody>
              {routes.map((route) => (
                <tr key={route.route_id} onClick={() => onOpen(route)} style={{ cursor: "pointer" }}>
                  <Td mono>{route.route_id}</Td>
                  <Td mono>
                    <span style={{ color: "var(--purple)" }}>{route.public_model}</span>
                  </Td>
                  <Td>{providerOf(route, connById)}</Td>
                  <Td mono>
                    <span style={{ color: "var(--ink2)" }}>
                      {route.provider_model} · {route.execution_region}
                    </span>
                  </Td>
                  <Td>
                    <Badge tone={statusTone(route.administrative_status)}>{route.administrative_status}</Badge>
                  </Td>
                  <Td>
                    <span style={{ color: "var(--blue)", fontSize: 12 }}>Details →</span>
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        </div>
      )}
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/* Page                                                                */
/* ------------------------------------------------------------------ */

export default function ModelsPage() {
  const { can } = useAuth();
  const [importOpen, setImportOpen] = useState(false);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [publicationId, setPublicationId] = useState<string | null>(null);
  const modelsRes: Resource<LLMModelList> = useResource(() => apiGet<LLMModelList>("/llm/models"), []);
  const catalogRes: Resource<CatalogRevision> = useResource(() => apiGet<CatalogRevision>("/control/v1/routing-catalog"), []);
  const connsRes: Resource<ProviderConnectionPage> = useResource(
    () => apiGet<ProviderConnectionPage>("/control/v1/provider-connections?limit=100"),
    [],
  );

  const [filters, setFilters] = useState<ModelFilters>(EMPTY_FILTERS);
  const [view, setView] = useState<"table" | "cards">("table");
  const [selected, setSelected] = useState<ModelRow | null>(null);

  const connById = useMemo(
    () => new Map((connsRes.data?.data ?? []).map((c) => [c.id, c])),
    [connsRes.data],
  );
  const routes = useMemo(() => catalogRes.data?.document?.routes ?? [], [catalogRes.data]);
  const { rows, hiddenRoutes } = useMemo(() => joinModels(modelsRes.data?.data ?? [], routes), [modelsRes.data, routes]);
  const options = useMemo(() => deriveFilterOptions(rows, connById), [rows, connById]);
  const visible = useMemo(() => filterRows(rows, filters, connById), [rows, filters, connById]);
  const revision = catalogRes.data?.revision ?? null;

  const openHiddenRoute = (route: ManagedRoute) =>
    setSelected({ publicModel: route.public_model, model: null, routes: [route], primary: route });

  return (
    <div style={{ padding: "20px 24px", maxWidth: 1360, margin: "0 auto" }}>
      <div style={{ display: "flex", alignItems: "flex-start", flexWrap: "wrap", gap: 16, marginBottom: 14 }}>
        <div style={{ flex: 1 }}>
          <h1 style={{ margin: 0, fontSize: 18, fontWeight: 600 }}>Models &amp; Capabilities</h1>
          <div style={{ color: "var(--ink2)", marginTop: 2, fontSize: 13 }}>
            Public models routable on the active catalog revision. Capability states: Native, Translated by the gateway, Unsupported, or Not
            declared.
          </div>
        </div>
        {can(PERMISSIONS.providersWrite) && can(PERMISSIONS.routingWrite) && <Button variant="primary" onClick={() => setImportOpen(true)}>Import from provider</Button>}
        {revision !== null && (
          <span style={{ fontFamily: "var(--font-mono)", fontSize: 12, color: "var(--purple)", paddingTop: 4 }}>
            {formatRevision(revision)}
          </span>
        )}
      </div>

      {importOpen && <ImportModels onClose={() => setImportOpen(false)} onCreated={d => { setDraft(d); setImportOpen(false); }} />}
      {draft && <DraftEditor key={draft.id} draft={draft} onDraftChange={setDraft} onClose={() => setDraft(null)} onHeadChanged={catalogRes.reload} onPublished={id => { setDraft(null); setPublicationId(id); catalogRes.reload(); }} />}
      {publicationId && <PublicationTracker publicationId={publicationId} onClose={() => setPublicationId(null)} onTerminal={() => { modelsRes.reload(); catalogRes.reload(); }} />}
      {[modelsRes, catalogRes, connsRes]
        .filter((r) => r.error?.isUpstreamNotConfigured)
        .map((r, i) => (
          <div key={i} style={{ marginBottom: 10 }}>
            <ErrorBanner error={r.error!} retry={r.reload} />
          </div>
        ))}

      {catalogRes.error && !catalogRes.error.isUpstreamNotConfigured && (
        <div style={{ marginBottom: 10 }}>
          <ErrorBanner error={catalogRes.error} retry={catalogRes.reload} />
        </div>
      )}
      {connsRes.error && !connsRes.error.isUpstreamNotConfigured && (
        <div style={{ marginBottom: 10, padding: "8px 11px", borderRadius: 8, background: "var(--amber-bg)", color: "var(--amber)", fontSize: 12 }}>
          Provider connections unavailable ({connsRes.error.code}) — showing raw connection IDs.
        </div>
      )}

      {modelsRes.loading && !modelsRes.data ? (
        <Loading />
      ) : modelsRes.error && !modelsRes.error.isUpstreamNotConfigured ? (
        <ErrorBanner error={modelsRes.error} retry={modelsRes.reload} />
      ) : modelsRes.error ? (
        <EmptyState title="Model list unavailable" hint="The gateway upstream must be configured on the BFF before tenant-visible models can load" />
      ) : (
        <>
          <FilterBar filters={filters} onChange={setFilters} options={options} view={view} onView={setView} />
          {rows.length === 0 ? (
            <EmptyState title="No models available" hint="No public models are visible to this tenant on the active catalog revision" />
          ) : visible.length === 0 ? (
            <EmptyState title="No models match the current filters" hint="Adjust or clear the filters to see more models" />
          ) : view === "table" ? (
            <ModelsTable rows={visible} capabilities={options.capabilities} connById={connById} onOpen={setSelected} />
          ) : (
            <ModelsCards rows={visible} capabilities={options.capabilities} connById={connById} onOpen={setSelected} />
          )}
          {rows.length > 0 && (
            <div style={{ marginTop: 8, fontSize: 12, color: "var(--ink3)" }}>
              Showing {formatNumber(visible.length)} of {formatNumber(rows.length)} models
            </div>
          )}
          {catalogRes.data && <HiddenRoutesSection routes={hiddenRoutes} connById={connById} onOpen={openHiddenRoute} />}
        </>
      )}

      <ModelDrawer row={selected} connById={connById} revision={revision} onClose={() => setSelected(null)} />
    </div>
  );
}
