import type { ApiError } from "../../api/client";

/* Shared types for the Playground page — Responses API shapes (subset, all
 * tolerant of missing fields) plus page-local run/chat state. */

export interface ModelList {
  object: "list";
  data: Array<{ id: string; object: string; created: number; owned_by: string }>;
}

export interface Usage {
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  cached_input_tokens?: number;
}

export interface ResponseError {
  code: string;
  message: string;
  type?: string;
  param?: string;
}

export interface OutputContent {
  type: string;
  text?: string;
}

export interface OutputItem {
  type: string;
  role?: string;
  content?: OutputContent[];
}

export interface ResponseObject {
  id: string;
  object: "response";
  status: string;
  model?: string;
  output?: OutputItem[];
  usage?: Usage | null;
  error?: ResponseError | null;
  created_at?: number;
  completed_at?: number | null;
  revision?: number;
}

/**
 * Extract assistant text by walking output items of type "message" → content
 * entries of type "output_text" → text. Never index fixed positions.
 */
export function extractOutputText(resp: ResponseObject): string {
  let out = "";
  for (const item of resp.output ?? []) {
    if (item.type !== "message") continue;
    for (const c of item.content ?? []) {
      if (c.type === "output_text" && typeof c.text === "string") out += c.text;
    }
  }
  return out;
}

/**
 * The exact request body shape the playground is allowed to send. Canary
 * constraints: store:false always; never temperature/top_p/max_output_tokens/
 * stop; no conversation/previous_response_id/background/tools/reasoning.
 */
export interface ResponsesRequest {
  model: string;
  input: string;
  store: false;
  stream: boolean;
}

export type MessageStatus = "streaming" | "completed" | "failed" | "cancelled";

export interface ChatMessage {
  id: number;
  role: "user" | "assistant" | "system";
  text: string;
  /** Assistant-only lifecycle state. */
  status?: MessageStatus;
  /** Mono meta line under an assistant bubble (id · tokens · duration · status). */
  meta?: string;
}

export interface EventLogEntry {
  seq: number | null;
  event: string;
  /** Truncated raw payload. */
  payload: string;
  at: string;
}

export type RunStatus = "idle" | "streaming" | "completed" | "failed" | "cancelled";

export interface RunError {
  code: string;
  /** Raw gateway/BFF message. */
  message: string;
  /** Operator-friendly mapping of the code/status. */
  friendly: string;
  /** Present for HTTP envelope errors so ErrorBanner can special-case 503 upstream_not_configured. */
  apiError?: ApiError;
}
