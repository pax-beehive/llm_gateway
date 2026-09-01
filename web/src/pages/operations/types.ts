/**
 * Page-local mirrors of the operations read models (internal/operations).
 * All endpoints are GET-only under /control/v1/operations and return complete
 * result sets — there is no cursor pagination on this API.
 */

export interface CheckResult {
  name: string;
  status: string;
}

export interface ReadinessResult {
  ready: boolean;
  checks: CheckResult[];
  checked_at: string;
}

export interface ConsumerObservation {
  name: string;
  last_event_id?: string;
  lag_seconds: number;
  pending_count: number;
  error_code?: string;
  last_succeeded_at?: string;
}

export interface BacklogSignals {
  oldest_unpublished_outbox_age_seconds: number;
  outbox_pending_count: number;
  quota_reconciliation_backlog: number;
  cache_refresh_due_backlog: number;
  retention_scrub_backlog: number;
  key_revocation_propagation_seconds: number;
  metering_projection_cutoff?: string;
  metering_projection_status: string;
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

export interface AccessRolloutReceipt {
  event_id: string;
  delivery_sequence: number;
  aggregate_type: string;
  aggregate_id: string;
  aggregate_revision: number;
  status: string;
  error_code?: string;
  occurred_at: string;
  applied_at: string;
  lag_milliseconds: number;
}

export interface GatewaySummary {
  event_schema_version: number;
  gateway_id: string;
  region: string;
  build_sha: string;
  database_schema_version: number;
  routing_catalog_revision: number;
  access_projection_revision: number;
  execution_epoch_floor: number;
  last_usage_outbox_id: number;
  started_at: string;
  observed_at: string;
  consumers: ConsumerObservation[];
  backlogs: BacklogSignals;
  routing_receipts?: RolloutReceipt[];
  access_receipts?: AccessRolloutReceipt[];
  received_at: string;
  heartbeat_lag_seconds: number;
  heartbeat_status: string;
  desired_routing_revision: number;
  routing_revision_lag: number;
  desired_access_revision: number;
  access_revision_lag: number;
}

export interface MeteringSummary {
  event_schema_version: number;
  metering_id: string;
  region: string;
  projection_generation: number;
  projection_cutoff: string;
  pending_events: number;
  oldest_pending_at?: string;
  poison_events: number;
  queued_exports: number;
  started_at: string;
  observed_at: string;
  received_at: string;
  heartbeat_lag_seconds: number;
  heartbeat_status: string;
  projection_status: string;
  oldest_pending_age_seconds: number;
}

export interface OutboxStatus {
  outbox_pending_count: number;
  oldest_unpublished_outbox_age_seconds: number;
  oldest_occurred_at?: string;
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

export interface JobSummary {
  id: string;
  kind: string;
  requested_by: string;
  tenant_id?: string;
  status: string;
  progress: number;
  result_ref?: string;
  error_code?: string;
  created_at: string;
  started_at?: string;
  finished_at?: string;
}

export type Tone = "green" | "amber" | "red" | "blue" | "purple" | "neutral";

export function heartbeatTone(status: string): Tone {
  switch (status) {
    case "current":
      return "green";
    case "stale":
      return "neutral";
    default:
      return "neutral";
  }
}

export function publicationTone(status: string): Tone {
  switch (status) {
    case "active":
    case "published":
      return "green";
    case "rolling_out":
    case "partially_applied":
      return "amber";
    case "failed":
      return "red";
    default:
      return "neutral";
  }
}

export function jobTone(status: string): Tone {
  switch (status) {
    case "succeeded":
      return "green";
    case "queued":
    case "pending":
      return "amber";
    case "running":
      return "blue";
    case "failed":
      return "red";
    default:
      return "neutral";
  }
}

export function receiptTone(status: string): Tone {
  switch (status) {
    case "applied":
    case "succeeded":
      return "green";
    case "failed":
      return "red";
    default:
      return "amber";
  }
}

/** 3725.4 → "1h 2m"; compact human rendering of age/lag seconds. */
export function formatSeconds(seconds: number): string {
  const s = Math.max(0, Math.round(seconds));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  const d = Math.floor(h / 24);
  return `${d}d ${h % 24}h`;
}
