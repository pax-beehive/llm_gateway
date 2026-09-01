/**
 * API client for the BFF. All calls hit same-origin `/api/...` (proxied by
 * Vite in dev, served by the BFF in production) with credentials:
 * "same-origin" so the BFF session cookie rides along.
 *
 * Error envelopes handled:
 *   {"error":{"code","message","type"?,"param"?}}   (gateway / control plane)
 *   {"error":{"code"}}                              (metering variant)
 * A 503 with code "upstream_not_configured" is thrown like any other
 * ApiError — pages render a configuration banner for it.
 *
 * Session behavior: 401 session_required / session_expired are reported to the
 * handler registered by the AuthProvider (setSessionLostHandler); everything
 * else, including 403 permission_denied, is a plain ApiError for pages to
 * render. Call sites needing the raw Response use authFetch(), which keeps
 * the same credentials + session-loss behavior.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly type?: string;
  readonly param?: string;

  constructor(status: number, code: string, message: string, type?: string, param?: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.type = type;
    this.param = param;
  }

  get isUpstreamNotConfigured(): boolean {
    return this.status === 503 && this.code === "upstream_not_configured";
  }
}

const BASE = "/api";

/* ------------------------------------------------------------------ */
/* Session loss notification                                           */
/* ------------------------------------------------------------------ */

export type SessionLostReason = "session_required" | "session_expired";
type SessionLostHandler = (reason: SessionLostReason) => void;

let sessionLostHandler: SessionLostHandler | null = null;

/**
 * Registered once by the AuthProvider. Called when any request fails with a
 * 401 session_required / session_expired — the handler transitions the app to
 * the anonymous state once, so parallel failing requests never cause a
 * redirect storm. Other 401s and 403 permission_denied stay plain ApiErrors.
 */
export function setSessionLostHandler(handler: SessionLostHandler | null): void {
  sessionLostHandler = handler;
}

function isSessionLostCode(code: string): code is SessionLostReason {
  return code === "session_required" || code === "session_expired";
}

function notifySessionLost(err: ApiError): void {
  if (err.status === 401 && isSessionLostCode(err.code)) sessionLostHandler?.(err.code);
}

/**
 * fetch() wrapper for call sites that need the raw Response (e.g. readiness
 * 503-with-body, export Link headers). Same-origin credentials are always
 * set, and 401 session errors are reported to the session-lost handler by
 * peeking at a cloned body — the caller's body stream stays untouched.
 */
export async function authFetch(path: string, init?: RequestInit): Promise<Response> {
  const response = await fetch(`${BASE}${path}`, { ...init, credentials: "same-origin" });
  if (response.status === 401 && sessionLostHandler) {
    response
      .clone()
      .json()
      .then((body) => {
        const code = (body as ErrorEnvelope)?.error?.code;
        if (code && isSessionLostCode(code)) sessionLostHandler?.(code);
      })
      .catch(() => undefined);
  }
  return response;
}

interface ErrorEnvelope {
  error?: {
    code?: string;
    message?: string;
    type?: string;
    param?: string;
  };
}

async function parseError(response: Response): Promise<ApiError> {
  let envelope: ErrorEnvelope = {};
  try {
    envelope = (await response.json()) as ErrorEnvelope;
  } catch {
    // Non-JSON error body; fall through to generic message.
  }
  const err = envelope.error ?? {};
  const code = err.code ?? `http_${response.status}`;
  const message = err.message ?? `Request failed with status ${response.status}`;
  return new ApiError(response.status, code, message, err.type, err.param);
}

async function parseJson<T>(response: Response): Promise<T> {
  if (!response.ok) {
    const err = await parseError(response);
    notifySessionLost(err);
    throw err;
  }
  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

export function apiGet<T>(path: string, opts?: { signal?: AbortSignal }): Promise<T> {
  return fetch(`${BASE}${path}`, {
    method: "GET",
    headers: { Accept: "application/json" },
    credentials: "same-origin",
    signal: opts?.signal,
  }).then((r) => parseJson<T>(r));
}

export function apiSend<T>(
  method: "POST" | "PUT" | "PATCH" | "DELETE",
  path: string,
  body?: unknown,
  opts?: { signal?: AbortSignal; idempotencyKey?: string },
): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (opts?.idempotencyKey) headers["Idempotency-Key"] = opts.idempotencyKey;
  return fetch(`${BASE}${path}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: "same-origin",
    signal: opts?.signal,
  }).then((r) => parseJson<T>(r));
}

export interface SSEFrame {
  /** Named event type, e.g. "response.output_text.delta". Empty string when absent. */
  event: string;
  /** Raw `data:` payload (multi-line data frames are joined with \n). Never "[DONE]" special-cased. */
  data: string;
}

/**
 * Minimal SSE consumer over fetch + ReadableStream. Parses frames separated by
 * blank lines, ignores ":" keepalive comments, and yields {event, data} for
 * every frame. The Responses API has no [DONE] sentinel, so none is handled.
 */
export async function* streamSSE(
  url: string,
  body: unknown,
  signal?: AbortSignal,
): AsyncGenerator<SSEFrame> {
  const response = await fetch(`${BASE}${url}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "text/event-stream" },
    body: JSON.stringify(body),
    credentials: "same-origin",
    signal,
  });
  if (!response.ok) {
    const err = await parseError(response);
    // A 401 before the stream starts is a session problem; once frames are
    // flowing, mid-stream disconnects keep the existing partial-output path.
    notifySessionLost(err);
    throw err;
  }
  if (!response.body) throw new ApiError(response.status, "no_body", "Response has no body to stream");

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let boundary: number;
      // Frames are separated by a blank line (\n\n or \r\n\r\n).
      while ((boundary = buffer.search(/\r?\n\r?\n/)) >= 0) {
        const rawFrame = buffer.slice(0, boundary);
        buffer = buffer.slice(buffer[boundary] === "\r" ? boundary + 4 : boundary + 2);
        const frame = parseFrame(rawFrame);
        if (frame) yield frame;
      }
    }
    buffer += decoder.decode();
    if (buffer.trim()) {
      const frame = parseFrame(buffer);
      if (frame) yield frame;
    }
  } finally {
    reader.cancel().catch(() => undefined);
  }
}

function parseFrame(raw: string): SSEFrame | null {
  let event = "";
  const dataLines: string[] = [];
  for (const line of raw.split(/\r?\n/)) {
    if (line.startsWith(":")) continue; // keepalive comment
    if (line.startsWith("event:")) event = line.slice(6).trimStart();
    else if (line.startsWith("data:")) dataLines.push(line.slice(5).replace(/^ /, ""));
  }
  if (!event && dataLines.length === 0) return null;
  return { event, data: dataLines.join("\n") };
}
