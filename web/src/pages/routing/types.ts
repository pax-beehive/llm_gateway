/**
 * Routing Catalog domain types. Field names mirror the JSON tags in
 * internal/routingcatalog (managed.go, administration_types.go) and
 * internal/providerconnection/types.go — keep them in sync.
 */
import type { BadgeTone } from "../../components/ui";

export type CapabilitySupport = "native" | "translated" | "unsupported";

export interface PriceSnapshot {
  source?: string;
  effective_at?: number;
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
}

export interface SelectionPolicy {
  priority: number;
  weight: number;
  max_concurrency?: number;
  sticky_routing_eligible?: boolean;
}

export interface TenantVisibilityPolicy {
  all_tenants?: boolean;
  tenant_ids?: string[];
  limit_policy_revisions?: Record<string, number>;
}

export interface CacheProtectionRoutePolicy {
  enabled: boolean;
  ttl_seconds?: number;
}

export type RouteAdministrativeStatus = "active" | "draining" | "disabled";

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
  administrative_status: RouteAdministrativeStatus;
  selection_policy: SelectionPolicy;
  tenant_visibility_policy: TenantVisibilityPolicy;
  cache_usage_reliable: boolean;
  cache_protection_policy: CacheProtectionRoutePolicy;
  embedding_path?: string;
  moderation_path?: string;
  rerank_path?: string;
  embedding_dimensions?: number;
}

export interface RoutingDocument {
  routes: ManagedRoute[];
}

export interface ValidationIssue {
  code: string;
  path: string;
  message: string;
}

export interface ValidationReport {
  valid: boolean;
  hash: string;
  errors: ValidationIssue[];
  warnings: ValidationIssue[];
}

export interface Revision {
  revision: number;
  document: RoutingDocument;
  validation_report: ValidationReport;
  validation_hash: string;
  source_revision?: number;
  created_by: string;
  created_at: string;
}

export type DraftStatus = "draft" | "validated";

export interface Draft {
  id: string;
  base_revision: number;
  document: RoutingDocument;
  status: DraftStatus;
  revision: number;
  validation_report: ValidationReport;
  validation_hash?: string;
  created_by: string;
  updated_by: string;
  created_at: string;
  updated_at: string;
}

export type PublicationStatus = "published" | "rolling_out" | "active" | "partially_applied" | "failed";

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

export interface Publication {
  id: string;
  catalog_revision: number;
  status: PublicationStatus;
  validation_hash: string;
  required_regions: string[];
  created_by: string;
  created_at: string;
  updated_at: string;
  receipts?: RolloutReceipt[];
}

export interface PublicationResult {
  revision: Revision;
  publication: Publication;
}

export interface RevisionPage {
  data: Revision[];
  next_cursor?: number;
}

export type OperationStatus = "queued" | "running" | "succeeded" | "failed" | "uncertain";

/** Provider Connection operation (internal/providerconnection/types.go). */
export interface Operation {
  id: string;
  type: string;
  connection_id: string;
  expected_revision: number;
  status: OperationStatus;
  result?: Record<string, unknown>;
  error_code?: string;
  error_message?: string;
  created_at: string;
  started_at?: string;
  completed_at?: string;
}

/* ------------------------------------------------------------------ */
/* Helpers                                                             */
/* ------------------------------------------------------------------ */

/** 41 → "rc-000041" (mock style). */
export function formatRevision(n: number): string {
  return `rc-${String(n).padStart(6, "0")}`;
}

export function newDraftId(): string {
  return `rcd_${Math.random().toString(36).slice(2, 10)}`;
}

/** "us-west, us-east" / newline-separated → ["us-west", "us-east"]. */
export function parseRegions(input: string): string[] {
  return input
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}

export function capabilityTone(s: CapabilitySupport): BadgeTone {
  if (s === "native") return "blue";
  if (s === "translated") return "purple";
  return "neutral";
}

export function routeStatusTone(s: RouteAdministrativeStatus): BadgeTone {
  if (s === "active") return "green";
  if (s === "draining") return "amber";
  return "neutral";
}

export function draftStatusTone(s: DraftStatus): BadgeTone {
  return s === "validated" ? "green" : "blue";
}

export function publicationTone(s: PublicationStatus): BadgeTone {
  if (s === "active") return "green";
  if (s === "failed") return "red";
  if (s === "partially_applied") return "amber";
  if (s === "rolling_out") return "amber";
  return "blue"; // published (accepted, not yet rolling)
}

export function isTerminalPublication(s: PublicationStatus): boolean {
  return s === "active" || s === "partially_applied" || s === "failed";
}

export function operationTone(s: OperationStatus): BadgeTone {
  if (s === "succeeded") return "green";
  if (s === "failed") return "red";
  if (s === "running") return "blue";
  if (s === "uncertain") return "amber";
  return "neutral"; // queued
}

export function isTerminalOperation(s: OperationStatus): boolean {
  return s === "succeeded" || s === "failed" || s === "uncertain";
}

/** Rollout receipt status → tone (applied/succeeded green, failed red, else amber). */
export function receiptTone(s: string): BadgeTone {
  if (s === "applied" || s === "succeeded" || s === "active") return "green";
  if (s === "failed") return "red";
  return "amber";
}

/** Failed validation remains status=draft, but still has a persisted report. */
export function hasValidationReport(draft: Draft): boolean {
  return Boolean(draft.validation_hash || draft.validation_report?.hash);
}
