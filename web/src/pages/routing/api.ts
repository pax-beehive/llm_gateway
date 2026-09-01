/**
 * Typed wrappers for the Routing Catalog control-plane API, all under
 * /api/control/v1 (the BFF strips /api and injects auth + Idempotency-Key).
 */
import { apiGet, apiSend } from "../../api/client";
import type {
  Draft,
  Operation,
  Publication,
  PublicationResult,
  Revision,
  RevisionPage,
  RoutingDocument,
} from "./types";

export interface DraftPage {
  data: Draft[];
  next_cursor?: string;
}

const CATALOG = "/control/v1/routing-catalog";
const PUBLICATIONS = "/control/v1/routing-publications";
const OPERATIONS = "/control/v1/provider-operations";

export function getCurrentRevision(): Promise<Revision> {
  return apiGet<Revision>(CATALOG);
}

export function createDraft(input: { id: string; base_revision: number; document: RoutingDocument; reason: string }): Promise<Draft> {
  return apiSend<Draft>("POST", `${CATALOG}/drafts`, input);
}

export function getDraft(id: string): Promise<Draft> {
  return apiGet<Draft>(`${CATALOG}/drafts/${encodeURIComponent(id)}`);
}

export function listDrafts(cursor = "", limit = 100): Promise<DraftPage> {
  const params = new URLSearchParams({ limit: String(limit) });
  if (cursor) params.set("cursor", cursor);
  return apiGet<DraftPage>(`${CATALOG}/drafts?${params.toString()}`);
}

export function updateDraft(id: string, input: { document: RoutingDocument; expected_revision: number; reason: string }): Promise<Draft> {
  return apiSend<Draft>("PUT", `${CATALOG}/drafts/${encodeURIComponent(id)}`, input);
}

export function validateDraft(id: string, input: { expected_revision: number; required_regions?: string[]; reason: string }): Promise<Draft> {
  return apiSend<Draft>("POST", `${CATALOG}/drafts/${encodeURIComponent(id)}/validate`, input);
}

export function probeDraft(id: string, input: { expected_revision: number; reason: string }): Promise<{ data: Operation[] }> {
  return apiSend<{ data: Operation[] }>("POST", `${CATALOG}/drafts/${encodeURIComponent(id)}/probe`, input);
}

export function publishDraft(
  id: string,
  input: { expected_revision: number; required_regions: string[]; reason: string },
): Promise<PublicationResult> {
  return apiSend<PublicationResult>("POST", `${CATALOG}/drafts/${encodeURIComponent(id)}/publish`, input);
}

export function listRevisions(cursor?: number, limit = 25): Promise<RevisionPage> {
  const params = new URLSearchParams();
  if (cursor !== undefined && cursor > 0) params.set("cursor", String(cursor));
  params.set("limit", String(limit));
  return apiGet<RevisionPage>(`${CATALOG}/revisions?${params.toString()}`);
}

export function restoreRevision(
  revision: number,
  input: { expected_head: number; required_regions: string[]; reason: string },
): Promise<PublicationResult> {
  return apiSend<PublicationResult>("POST", `${CATALOG}/revisions/${revision}/restore`, input);
}

export function getPublication(id: string): Promise<Publication> {
  return apiGet<Publication>(`${PUBLICATIONS}/${encodeURIComponent(id)}`);
}

export function getOperation(id: string): Promise<Operation> {
  return apiGet<Operation>(`${OPERATIONS}/${encodeURIComponent(id)}`);
}
