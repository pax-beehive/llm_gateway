/**
 * Models & Capabilities data layer: types for the routing catalog revision and
 * provider connections, plus the client-side join between the tenant-visible
 * model list (/llm/models) and the catalog's managed routes.
 */
import type { LLMModel, ProviderConnection } from "../overview/lib";

export type CapabilitySupport = "native" | "translated" | "unsupported";

export interface PriceSnapshot {
  id: string;
  provider: string;
  model: string;
  region: string;
  currency: string;
  input_per_million_micros: number;
  cached_input_per_million_micros: number;
  cache_write_per_million_micros: number;
  output_per_million_micros: number;
  embedding_input_per_million_micros?: number;
  moderation_input_per_million_micros?: number;
  rerank_document_per_thousand_micros?: number;
  effective_at: number;
  source: string;
}

export interface TenantVisibilityPolicy {
  all_tenants?: boolean;
  tenant_ids?: string[];
  limit_policy_revisions?: Record<string, number>;
}

export interface ManagedRoute {
  route_id: string;
  public_model: string;
  provider_connection_id: string;
  provider_model: string;
  execution_region: string;
  home_region: string;
  capability_profile_revision: number;
  capabilities: Record<string, CapabilitySupport>;
  provider_cost_snapshot: PriceSnapshot;
  administrative_status: string;
  selection_policy: {
    priority: number;
    weight: number;
    max_concurrency?: number;
    sticky_routing_eligible?: boolean;
  };
  tenant_visibility_policy: TenantVisibilityPolicy;
  cache_usage_reliable: boolean;
  cache_protection_policy: { enabled: boolean; ttl_seconds?: number };
  embedding_path?: string;
  moderation_path?: string;
  rerank_path?: string;
  embedding_dimensions?: number;
}

export interface CatalogRevision {
  revision: number;
  document: { routes: ManagedRoute[] };
  validation_hash: string;
  source_revision?: number;
  created_by: string;
  created_at: string;
}

/* ------------------------------------------------------------------ */
/* Join                                                                */
/* ------------------------------------------------------------------ */

/** A row per public model from /llm/models; routes may be empty (no route visible). */
export interface ModelRow {
  publicModel: string;
  model: LLMModel | null;
  routes: ManagedRoute[];
  /** Representative route for list display: active first, then region/id order. */
  primary: ManagedRoute | null;
}

/** Active routes first, then draining, then disabled; ties by region/id. */
export function sortRoutes(routes: ManagedRoute[]): ManagedRoute[] {
  const rank = (status: string) => (status === "active" ? 0 : status === "draining" ? 1 : 2);
  return [...routes].sort(
    (a, b) =>
      rank(a.administrative_status) - rank(b.administrative_status) ||
      a.execution_region.localeCompare(b.execution_region) ||
      a.route_id.localeCompare(b.route_id),
  );
}

export function joinModels(
  models: LLMModel[],
  routes: ManagedRoute[],
): { rows: ModelRow[]; hiddenRoutes: ManagedRoute[] } {
  const byModel = new Map<string, ManagedRoute[]>();
  for (const route of routes) {
    const list = byModel.get(route.public_model) ?? [];
    list.push(route);
    byModel.set(route.public_model, list);
  }
  const visibleIds = new Set(models.map((m) => m.id));
  const rows: ModelRow[] = [...models]
    .sort((a, b) => a.id.localeCompare(b.id))
    .map((model) => {
      const modelRoutes = sortRoutes(byModel.get(model.id) ?? []);
      return { publicModel: model.id, model, routes: modelRoutes, primary: modelRoutes[0] ?? null };
    });
  const hiddenRoutes = sortRoutes(routes.filter((r) => !visibleIds.has(r.public_model)));
  return { rows, hiddenRoutes };
}

/* ------------------------------------------------------------------ */
/* Filter options + row filtering (all client-side over the join)      */
/* ------------------------------------------------------------------ */

export interface ModelFilters {
  provider: string;
  region: string;
  capability: string;
  status: string;
  search: string;
}

export const EMPTY_FILTERS: ModelFilters = { provider: "", region: "", capability: "", status: "", search: "" };

const PREFERRED_CAPABILITIES = ["text", "streaming", "tools", "embeddings", "embedding", "moderation", "rerank"];

/** Capability keys present in the catalog, preferred order first, then alpha. */
export function capabilityOrder(routes: ManagedRoute[]): string[] {
  const present = new Set<string>();
  for (const route of routes) for (const key of Object.keys(route.capabilities ?? {})) present.add(key);
  const ordered = PREFERRED_CAPABILITIES.filter((k) => present.has(k));
  const rest = [...present].filter((k) => !PREFERRED_CAPABILITIES.includes(k)).sort();
  return [...ordered, ...rest];
}

export interface FilterOptions {
  providers: string[];
  regions: string[];
  capabilities: string[];
  statuses: string[];
}

export function deriveFilterOptions(rows: ModelRow[], connById: Map<string, ProviderConnection>): FilterOptions {
  const providers = new Set<string>();
  const regions = new Set<string>();
  const statuses = new Set<string>();
  const routes: ManagedRoute[] = [];
  for (const row of rows) {
    for (const route of row.routes) {
      routes.push(route);
      providers.add(connById.get(route.provider_connection_id)?.provider ?? route.provider_connection_id);
      regions.add(route.execution_region);
      statuses.add(route.administrative_status);
    }
  }
  return {
    providers: [...providers].sort(),
    regions: [...regions].sort(),
    capabilities: capabilityOrder(routes),
    statuses: [...statuses].sort(),
  };
}

function routeMatches(route: ManagedRoute, filters: ModelFilters, connById: Map<string, ProviderConnection>): boolean {
  if (filters.provider) {
    const provider = connById.get(route.provider_connection_id)?.provider ?? route.provider_connection_id;
    if (provider !== filters.provider) return false;
  }
  if (filters.region && route.execution_region !== filters.region) return false;
  if (filters.capability) {
    const support = route.capabilities?.[filters.capability];
    if (support !== "native" && support !== "translated") return false;
  }
  if (filters.status && route.administrative_status !== filters.status) return false;
  return true;
}

export function filterRows(rows: ModelRow[], filters: ModelFilters, connById: Map<string, ProviderConnection>): ModelRow[] {
  const query = filters.search.trim().toLowerCase();
  return rows.filter((row) => {
    if (query) {
      const haystack = [row.publicModel, ...row.routes.flatMap((r) => [r.provider_model, r.route_id])]
        .join("\n")
        .toLowerCase();
      if (!haystack.includes(query)) return false;
    }
    const constrained = filters.provider !== "" || filters.region !== "" || filters.capability !== "" || filters.status !== "";
    if (constrained) {
      // Models without a visible route can only satisfy the free-text search.
      if (row.routes.length === 0) return false;
      return row.routes.some((route) => routeMatches(route, filters, connById));
    }
    return true;
  });
}
