/**
 * Data layer shared by the Overview and Models pages: a per-resource polling
 * hook (independent load/error surfaces per card), readiness probing that
 * tolerates the 503-with-body readiness contract, and platform-scoped
 * Metering aggregation helpers.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { apiGet, ApiError } from "../../api/client";
import type { BadgeTone } from "../../components/ui";

/* ------------------------------------------------------------------ */
/* useResource / useAutoRefresh                                        */
/* ------------------------------------------------------------------ */

export interface Resource<T> {
  data: T | null;
  error: ApiError | null;
  loading: boolean;
  reload: () => void;
}

/**
 * GET-on-mount hook with manual reload. Unlike useApi it takes a fetcher, so a
 * card can compose several endpoints; `deps` re-trigger the load (e.g. a
 * refresh tick). Data is kept while reloading to avoid flicker.
 */
export function useResource<T>(load: () => Promise<T>, deps: readonly unknown[]): Resource<T> {
  const [state, setState] = useState<{ data: T | null; error: ApiError | null; loading: boolean }>({
    data: null,
    error: null,
    loading: true,
  });
  const [attempt, setAttempt] = useState(0);
  const loadRef = useRef(load);
  loadRef.current = load;

  useEffect(() => {
    let cancelled = false;
    setState((s) => ({ ...s, loading: true, error: null }));
    loadRef.current().then(
      (data) => {
        if (!cancelled) setState({ data, error: null, loading: false });
      },
      (err: unknown) => {
        if (cancelled) return;
        setState({
          data: null,
          error: err instanceof ApiError ? err : new ApiError(0, "network", String(err)),
          loading: false,
        });
      },
    );
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, attempt]);

  const reload = useCallback(() => setAttempt((a) => a + 1), []);
  return { ...state, reload };
}

/** Refresh tick + 15s auto-refresh toggle + last-observed timestamp. */
export function useAutoRefresh(intervalMs = 15_000) {
  const [tick, setTick] = useState(0);
  const [auto, setAuto] = useState(false);
  const [observedAt, setObservedAt] = useState<Date | null>(null);

  useEffect(() => {
    if (!auto) return;
    const id = window.setInterval(() => setTick((t) => t + 1), intervalMs);
    return () => window.clearInterval(id);
  }, [auto, intervalMs]);

  useEffect(() => {
    setObservedAt(new Date());
  }, [tick]);

  const refresh = useCallback(() => setTick((t) => t + 1), []);
  return { tick, auto, setAuto, refresh, observedAt };
}

/* ------------------------------------------------------------------ */
/* Status → tone mapping (CONVENTIONS.md)                              */
/* ------------------------------------------------------------------ */

export function statusTone(status: string): BadgeTone {
  switch (status) {
    case "ready":
    case "healthy":
    case "active":
    case "enabled":
    case "succeeded":
    case "applied":
    case "current":
      return "green";
    case "degraded":
    case "pending":
    case "grace":
    case "propagating":
    case "rolling_out":
    case "partially_applied":
    case "draining":
    case "queued":
      return "amber";
    case "notready":
    case "not_ready":
    case "suspended":
    case "revoked":
    case "failed":
    case "unavailable":
      return "red";
    case "running":
    case "draft":
    case "published":
      return "blue";
    default:
      return "neutral"; // unknown / stale / disabled / unsupported
  }
}

/** 41 → "rc-000041". */
export function formatRevision(revision: number): string {
  return `rc-${String(revision).padStart(6, "0")}`;
}

/* ------------------------------------------------------------------ */
/* Readiness (/readyz returns 503 with a ReadinessResult body when     */
/* degraded — that body is data, not an error envelope)                */
/* ------------------------------------------------------------------ */

export interface ReadinessCheck {
  name: string;
  status: string;
}

export interface Readiness {
  ready: boolean;
  checks: ReadinessCheck[];
  checked_at: string;
}

export interface ServiceReadiness {
  service: string;
  readiness: Readiness | null;
  error: ApiError | null;
}

async function fetchReadiness(path: string): Promise<Readiness> {
  const response = await fetch(`/api${path}`, { headers: { Accept: "application/json" } });
  let body: unknown = null;
  try {
    body = await response.json();
  } catch {
    // Non-JSON body; fall through to error handling below.
  }
  const readiness = body as Partial<Readiness> | null;
  if (readiness && Array.isArray(readiness.checks)) {
    return {
      ready: readiness.ready === true,
      checks: readiness.checks as ReadinessCheck[],
      checked_at: typeof readiness.checked_at === "string" ? readiness.checked_at : "",
    };
  }
  const envelope = body as { error?: { code?: string; message?: string } } | null;
  if (envelope?.error?.code) {
    throw new ApiError(
      response.status,
      envelope.error.code,
      envelope.error.message ?? `Request failed with status ${response.status}`,
    );
  }
  throw new ApiError(response.status, `http_${response.status}`, `Readiness probe failed with status ${response.status}`);
}

/** Gateway + control-plane readiness. Throws only when every probe failed, so
 *  one unreachable service never hides the other. */
export async function loadReadiness(): Promise<ServiceReadiness[]> {
  const probe = async (service: string, path: string): Promise<ServiceReadiness> => {
    try {
      return { service, readiness: await fetchReadiness(path), error: null };
    } catch (err) {
      return {
        service,
        readiness: null,
        error: err instanceof ApiError ? err : new ApiError(0, "network", String(err)),
      };
    }
  };
  const results = await Promise.all([probe("gateway", "/llm/readyz"), probe("control plane", "/control/readyz")]);
  if (results.every((r) => r.error)) throw results[0].error ?? new ApiError(0, "network", "readiness unavailable");
  return results;
}

/* ------------------------------------------------------------------ */
/* API payload types                                                   */
/* ------------------------------------------------------------------ */

export interface LLMModel {
  id: string;
  object: string;
  created: number;
  owned_by: string;
}

export interface LLMModelList {
  object: string;
  data: LLMModel[];
}

export interface ProviderConnection {
  id: string;
  provider: string;
  display_name: string;
  region: string;
  administrative_status: string;
  credential_version: number;
  revision: number;
}

export interface ProviderConnectionPage {
  data: ProviderConnection[];
  next_cursor?: string;
}

export interface GatewaySummary {
  gateway_id: string;
  region: string;
  heartbeat_status: string;
  heartbeat_lag_seconds: number;
  routing_catalog_revision: number;
  desired_routing_revision: number;
  routing_revision_lag: number;
  access_revision_lag: number;
  received_at: string;
  backlogs: {
    outbox_pending_count: number;
    metering_projection_status?: string;
  };
}

export interface GatewayPage {
  data: GatewaySummary[];
}

export interface RolloutReceipt {
  publication_id: string;
  gateway_id: string;
  region: string;
  catalog_revision: number;
  status: string;
  error_code?: string;
  applied_at: string;
  lag_milliseconds: number;
}

export interface PublicationSummary {
  id: string;
  catalog_revision: number;
  status: string;
  required_regions: string[];
  created_at: string;
  updated_at: string;
  receipts?: RolloutReceipt[];
}

export interface PublicationPage {
  data: PublicationSummary[];
}

export interface MeteringSummary {
  metering_id: string;
  region: string;
  projection_generation: number;
  projection_cutoff: string;
  pending_events: number;
  oldest_pending_at?: string;
  poison_events: number;
  queued_exports: number;
  observed_at: string;
  received_at: string;
  heartbeat_lag_seconds: number;
  heartbeat_status: string;
  projection_status: string;
  oldest_pending_age_seconds: number;
}

export interface MeteringPage {
  data: MeteringSummary[];
}

export interface UsageTotals {
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

export interface UsageSummary {
  data_cutoff: string;
  totals: UsageTotals[];
}

export interface UsageTimePoint {
  start: string;
  totals: UsageTotals;
}

/* ------------------------------------------------------------------ */
/* Platform Metering aggregation                                      */
/* ------------------------------------------------------------------ */

export interface UsageAggregate {
  requests: number;
  spendMicros: number;
  currencies: string[];
  /** Newest data_cutoff seen, or null when metering has no events yet. */
  cutoff: string | null;
}

/** Metering reports "epoch" (1970) as the cutoff when the inbox is empty. */
function saneCutoff(iso: string | null | undefined): string | null {
  if (!iso) return null;
  const time = new Date(iso).getTime();
  return Number.isNaN(time) || time < 946_684_800_000 ? null : iso;
}

/** Aggregate today's usage (UTC day boundary) in one platform query. */
export async function aggregateUsageToday(): Promise<UsageAggregate> {
  const now = new Date();
  const from = new Date(now);
  from.setUTCHours(0, 0, 0, 0);
  const result = await apiGet<UsageSummary>(
    `/metering/v1/usage/summary?from=${encodeURIComponent(from.toISOString())}&through=${encodeURIComponent(now.toISOString())}`,
  );
  const aggregate: UsageAggregate = {
    requests: 0,
    spendMicros: 0,
    currencies: [],
    cutoff: saneCutoff(result.data_cutoff),
  };
  for (const totals of result.totals ?? []) {
    aggregate.requests += totals.operation_count;
    aggregate.spendMicros += totals.amount_micros;
    if (!aggregate.currencies.includes(totals.currency)) aggregate.currencies.push(totals.currency);
  }
  return aggregate;
}

export interface SeriesBucket {
  /** Bucket start epoch ms (local hour-aligned). */
  at: number;
  label: string;
  requests: number;
  spendMicros: number;
}

export interface UsageSeries {
  buckets: SeriesBucket[];
}

/** Hourly request volume + spend over the last 24 hours, all tenants summed by Metering. */
export async function aggregateUsageSeries24h(): Promise<UsageSeries> {
  const now = new Date();
  const endHour = new Date(now);
  endHour.setMinutes(0, 0, 0);
  const from = new Date(endHour.getTime() - 23 * 3_600_000);

  const buckets: SeriesBucket[] = [];
  const byStart = new Map<number, SeriesBucket>();
  for (let i = 23; i >= 0; i--) {
    const at = endHour.getTime() - i * 3_600_000;
    const label = `${String(new Date(at).getHours()).padStart(2, "0")}:00`;
    const bucket: SeriesBucket = { at, label, requests: 0, spendMicros: 0 };
    buckets.push(bucket);
    byStart.set(at, bucket);
  }

  const result = await apiGet<{ data: UsageTimePoint[] }>(
    `/metering/v1/usage/timeseries?from=${encodeURIComponent(from.toISOString())}&through=${encodeURIComponent(now.toISOString())}&granularity=hour`,
  );
  for (const point of result.data ?? []) {
    const start = new Date(point.start);
    if (Number.isNaN(start.getTime())) continue;
    start.setMinutes(0, 0, 0);
    const bucket = byStart.get(start.getTime());
    if (!bucket) continue;
    bucket.requests += point.totals.operation_count;
    bucket.spendMicros += point.totals.amount_micros;
  }
  return { buckets };
}

/* ------------------------------------------------------------------ */
/* Warnings (derived client-side from already-fetched data only)       */
/* ------------------------------------------------------------------ */

export interface Warning {
  tone: "red" | "amber";
  label: string;
  text: string;
}

export function latestPublication(page: PublicationPage | null): PublicationSummary | null {
  const publications = page?.data ?? [];
  if (publications.length === 0) return null;
  return [...publications].sort((a, b) => b.created_at.localeCompare(a.created_at))[0];
}

export function deriveWarnings(
  readiness: ServiceReadiness[] | null,
  gateways: GatewayPage | null,
  publications: PublicationPage | null,
  metering: MeteringPage | null,
): Warning[] {
  const warnings: Warning[] = [];

  for (const service of readiness ?? []) {
    if (service.error && !service.error.isUpstreamNotConfigured) {
      warnings.push({ tone: "red", label: "unavailable", text: `${service.service} readiness probe failed (${service.error.code})` });
    }
    for (const check of service.readiness?.checks ?? []) {
      if (check.status !== "ready") {
        warnings.push({ tone: "red", label: check.status, text: `${service.service} readiness check "${check.name}" is ${check.status}` });
      }
    }
  }

  for (const gateway of gateways?.data ?? []) {
    if (gateway.heartbeat_status === "stale") {
      warnings.push({
        tone: "amber",
        label: "stale",
        text: `Gateway ${gateway.gateway_id} (${gateway.region}) heartbeat is stale — ${Math.round(gateway.heartbeat_lag_seconds)}s behind`,
      });
    }
    if (gateway.routing_revision_lag > 0) {
      warnings.push({
        tone: "amber",
        label: "propagating",
        text: `Gateway ${gateway.gateway_id} (${gateway.region}) is ${gateway.routing_revision_lag} routing catalog revision(s) behind ${formatRevision(gateway.desired_routing_revision)}`,
      });
    }
    if ((gateway.backlogs?.outbox_pending_count ?? 0) > 0) {
      warnings.push({
        tone: "amber",
        label: "pending",
        text: `Gateway ${gateway.gateway_id} (${gateway.region}) has ${gateway.backlogs.outbox_pending_count} unpublished usage outbox event(s)`,
      });
    }
  }

  const latest = latestPublication(publications);
  if (latest && latest.status !== "active") {
    warnings.push({
      tone: latest.status === "failed" ? "red" : "amber",
      label: latest.status,
      text: `Routing publication ${latest.id} (${formatRevision(latest.catalog_revision)}) is ${latest.status.replace(/_/g, " ")}`,
    });
  }

  for (const node of metering?.data ?? []) {
    if (node.poison_events > 0) {
      warnings.push({ tone: "red", label: "poison", text: `Metering node ${node.metering_id} has ${node.poison_events} poison event(s) parked` });
    }
    if (node.pending_events > 0) {
      warnings.push({ tone: "amber", label: "pending", text: `Metering node ${node.metering_id} has ${node.pending_events} pending usage event(s)` });
    }
    if (node.projection_status === "degraded") {
      warnings.push({ tone: "amber", label: "degraded", text: `Metering projection on ${node.metering_id} is degraded` });
    }
  }

  const rank = { red: 0, amber: 1 };
  return warnings.sort((a, b) => rank[a.tone] - rank[b.tone]);
}
