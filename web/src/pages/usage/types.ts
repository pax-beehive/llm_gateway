/**
 * Page-local mirrors of the metering service contracts (internal/metering).
 * The BFF identity is platform-scoped, so every usage query requires an
 * explicit tenant_id — the API returns 403 policy_denied without one.
 */

export interface Totals {
  currency: string;
  operation_count: number;
  input_tokens: number;
  cached_input_tokens: number;
  cache_write_input_tokens: number;
  output_tokens: number;
  input_units: number;
  documents: number;
  amount_micros: number;
}

export interface Summary {
  data_cutoff: string;
  totals: Totals[];
}

export interface TimePoint {
  start: string;
  totals: Totals;
}

export interface UsageEvent {
  event_id: string;
  schema_version: number;
  type: string;
  usage_id: string;
  tenant_id: string;
  api_key_id?: string;
  response_id?: string;
  attempt_id?: string;
  operation_id?: string;
  capability?: string;
  route_id?: string;
  provider: string;
  public_model: string;
  provider_model: string;
  region: string;
  price_snapshot_id: string;
  input_tokens: number;
  cached_input_tokens: number;
  cache_write_input_tokens: number;
  output_tokens: number;
  input_units: number;
  documents: number;
  amount_micros: number;
  currency: string;
  outcome: string;
  corrects_event_id?: string;
  correction_actor_id?: string;
  reason?: string;
  occurred_at: string;
}

export interface EventPage {
  data: UsageEvent[];
  next_cursor?: string;
  data_cutoff: string;
}

export interface ExportJob {
  id: string;
  tenant_id: string;
  status: string;
  filter: unknown;
  cutoff: string;
  object_key?: string;
  sha256?: string;
  row_count: number;
  error_code?: string;
  created_at: string;
  completed_at?: string;
}

export interface MeteringStatus {
  projection_generation: number;
  projection_cutoff: string;
  pending_events: number;
  oldest_pending_at?: string;
  poison_events: number;
  queued_exports: number;
}

/** Filters accepted by the metering query API (filterFromRequest). */
export interface UsageFilter {
  tenantId: string;
  apiKeyId?: string;
  provider?: string;
  publicModel?: string;
  outcome?: string;
  from?: Date;
  through?: Date;
}

export function usageQuery(filter: UsageFilter): string {
  const params = new URLSearchParams();
  params.set("tenant_id", filter.tenantId);
  if (filter.apiKeyId) params.set("api_key_id", filter.apiKeyId);
  if (filter.provider) params.set("provider", filter.provider);
  if (filter.publicModel) params.set("public_model", filter.publicModel);
  if (filter.outcome) params.set("outcome", filter.outcome);
  if (filter.from) params.set("from", filter.from.toISOString());
  if (filter.through) params.set("through", filter.through.toISOString());
  return params.toString();
}

/** Sum token/count columns across per-currency rows (counts are currency-independent). */
export function combineTotals(totals: Totals[]): Totals {
  const base: Totals = {
    currency: totals[0]?.currency ?? "USD",
    operation_count: 0,
    input_tokens: 0,
    cached_input_tokens: 0,
    cache_write_input_tokens: 0,
    output_tokens: 0,
    input_units: 0,
    documents: 0,
    amount_micros: 0,
  };
  for (const row of totals) {
    base.operation_count += row.operation_count;
    base.input_tokens += row.input_tokens;
    base.cached_input_tokens += row.cached_input_tokens;
    base.cache_write_input_tokens += row.cache_write_input_tokens;
    base.output_tokens += row.output_tokens;
    base.input_units += row.input_units;
    base.documents += row.documents;
  }
  return base;
}

export type Tone = "green" | "amber" | "red" | "blue" | "purple" | "neutral";

export function outcomeTone(outcome: string): Tone {
  switch (outcome) {
    case "committed":
    case "succeeded":
      return "green";
    case "policy_denied":
    case "quota_denied":
      return "amber";
    case "failed":
    case "provider_failure":
      return "red";
    default:
      return "neutral";
  }
}

export function exportStatusTone(status: string): Tone {
  switch (status) {
    case "succeeded":
      return "green";
    case "queued":
      return "amber";
    case "running":
      return "blue";
    case "failed":
      return "red";
    default:
      return "neutral";
  }
}
