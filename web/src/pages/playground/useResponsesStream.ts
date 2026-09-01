import { useCallback, useRef, useState } from "react";
import { apiSend, ApiError, streamSSE } from "../../api/client";
import { formatNumber } from "../../lib/format";
import { friendlyFor } from "./errors";
import {
  extractOutputText,
  type ChatMessage,
  type EventLogEntry,
  type ResponseObject,
  type ResponsesRequest,
  type RunError,
  type RunStatus,
} from "./types";

export interface PlaygroundRun {
  messages: ChatMessage[];
  status: RunStatus;
  events: EventLogEntry[];
  finalResponse: ResponseObject | null;
  /** The exact JSON body of the in-flight / most recent send. */
  requestBody: ResponsesRequest | null;
  error: RunError | null;
  /** Client-measured wall time of the last run. */
  durationMs: number | null;
}

const INITIAL: PlaygroundRun = {
  messages: [],
  status: "idle",
  events: [],
  finalResponse: null,
  requestBody: null,
  error: null,
  durationMs: null,
};

let nextMsgId = 1;

/** Payloads may wrap the Response object (`{type, sequence_number, response}`) or be it directly. */
function asResponseObject(parsed: Record<string, unknown> | null): ResponseObject {
  const inner = parsed?.response;
  if (inner && typeof inner === "object") return inner as ResponseObject;
  return (parsed ?? {}) as unknown as ResponseObject;
}

function formatDuration(ms: number): string {
  return `${(ms / 1000).toFixed(2)}s`;
}

/**
 * Owns one chat session's run lifecycle: send (streamed or not), stop via
 * AbortController, reset. Never auto-retries. History is client-side only.
 */
export function useResponsesStream() {
  const [state, setState] = useState<PlaygroundRun>(INITIAL);
  const abortRef = useRef<AbortController | null>(null);
  /** Bumped per send and on reset, so stale async completions can't touch cleared state. */
  const sessionRef = useRef(0);

  const send = useCallback(async (model: string, input: string, stream: boolean): Promise<void> => {
    if (abortRef.current) return; // a run is already in flight
    const session = ++sessionRef.current;
    const alive = () => sessionRef.current === session;
    const controller = new AbortController();
    abortRef.current = controller;

    const body: ResponsesRequest = { model, input, store: false, stream };
    const userId = nextMsgId++;
    const assistantId = nextMsgId++;
    const startedAt = performance.now();
    const elapsed = () => Math.round(performance.now() - startedAt);

    const logEvent = (event: string, seq: number | null, payload: string) =>
      setState((s) => ({
        ...s,
        events: [
          ...s.events,
          {
            event,
            seq,
            payload: payload.length > 160 ? `${payload.slice(0, 160)}…` : payload,
            at: new Date().toISOString().slice(11, 23),
          },
        ],
      }));

    const appendDelta = (delta: string) =>
      setState((s) => ({
        ...s,
        messages: s.messages.map((m) => (m.id === assistantId ? { ...m, text: m.text + delta } : m)),
      }));

    setState((s) => ({
      ...s,
      messages: [
        ...s.messages,
        { id: userId, role: "user", text: input },
        { id: assistantId, role: "assistant", text: "", status: "streaming" },
      ],
      status: "streaming",
      events: [],
      finalResponse: null,
      requestBody: body,
      error: null,
      durationMs: null,
    }));

    const finishCompleted = (resp: ResponseObject) => {
      if (!alive()) return;
      const usage = resp.usage ?? null;
      const parts = [resp.id];
      if (usage) parts.push(`${formatNumber(usage.input_tokens)} in / ${formatNumber(usage.output_tokens)} out`);
      parts.push(formatDuration(elapsed()), resp.status || "completed");
      const fallbackText = extractOutputText(resp);
      setState((s) => ({
        ...s,
        status: "completed",
        finalResponse: resp,
        durationMs: elapsed(),
        messages: s.messages.map((m) =>
          m.id === assistantId
            ? { ...m, text: m.text || fallbackText, status: "completed" as const, meta: parts.join(" · ") }
            : m,
        ),
      }));
    };

    const finishFailed = (code: string, message: string, resp?: ResponseObject) => {
      if (!alive()) return;
      setState((s) => ({
        ...s,
        status: "failed",
        finalResponse: resp ?? s.finalResponse,
        durationMs: elapsed(),
        error: { code, message, friendly: friendlyFor(0, code, message) },
        // Keep any already-rendered text; just flip the bubble to failed.
        messages: s.messages.map((m) =>
          m.id === assistantId
            ? { ...m, status: "failed" as const, meta: `${code}: ${message} · ${formatDuration(elapsed())}` }
            : m,
        ),
      }));
    };

    const failHttp = (err: ApiError) => {
      if (!alive()) return;
      const friendly = friendlyFor(err.status, err.code, err.message);
      setState((s) => ({
        ...s,
        status: "failed",
        durationMs: elapsed(),
        error: { code: err.code, message: err.message, friendly, apiError: err },
        messages: [
          // Keep the assistant bubble only if partial output was already rendered.
          ...s.messages
            .filter((m) => m.id !== assistantId || m.text.length > 0)
            .map((m) =>
              m.id === assistantId
                ? { ...m, status: "failed" as const, meta: `${err.code} · ${formatDuration(elapsed())}` }
                : m,
            ),
          { id: nextMsgId++, role: "system" as const, text: `${friendly} (${err.code}: ${err.message})` },
        ],
      }));
    };

    const markCancelled = () => {
      if (!alive()) return;
      logEvent("response.cancelled", null, "client cancel — partial output retained");
      setState((s) => ({
        ...s,
        status: "cancelled",
        durationMs: elapsed(),
        messages: s.messages.map((m) =>
          m.id === assistantId
            ? { ...m, status: "cancelled" as const, meta: "cancelled — partial output retained" }
            : m,
        ),
      }));
    };

    try {
      if (stream) {
        let finalized = false;
        // Only monotonically increasing delta frames may change rendered text.
        // Replayed or out-of-order frames remain visible in the event inspector
        // but cannot duplicate or reorder the assistant message.
        let lastDeltaSeq = -1;
        for await (const frame of streamSSE("/llm/responses", body, controller.signal)) {
          if (!alive()) return;
          let parsed: Record<string, unknown> | null = null;
          try {
            parsed = JSON.parse(frame.data) as Record<string, unknown>;
          } catch {
            // Non-JSON payload — still surfaced in the event log.
          }
          const seq = typeof parsed?.sequence_number === "number" ? parsed.sequence_number : null;
          logEvent(frame.event || "message", seq, frame.data);
          if (frame.event === "response.output_text.delta") {
            const delta = typeof parsed?.delta === "string" ? parsed.delta : "";
            if (delta && (seq === null || seq > lastDeltaSeq)) {
              if (seq !== null) lastDeltaSeq = seq;
              appendDelta(delta);
            }
          } else if (frame.event === "response.completed") {
            finalized = true;
            finishCompleted(asResponseObject(parsed));
          } else if (frame.event === "response.failed") {
            finalized = true;
            const resp = asResponseObject(parsed);
            finishFailed(resp.error?.code ?? "response_failed", resp.error?.message ?? "the response failed", resp);
          }
        }
        if (!finalized && alive() && !controller.signal.aborted) {
          finishFailed("stream_truncated", "stream ended before response.completed");
        }
      } else {
        const resp = await apiSend<ResponseObject>("POST", "/llm/responses", body, {
          signal: controller.signal,
        });
        if (!alive()) return;
        if (resp.status === "failed") {
          finishFailed(resp.error?.code ?? "response_failed", resp.error?.message ?? "the response failed", resp);
        } else {
          finishCompleted(resp);
        }
      }
    } catch (err) {
      if (!alive()) return;
      if (controller.signal.aborted) {
        markCancelled();
      } else if (err instanceof ApiError) {
        failHttp(err);
      } else {
        failHttp(new ApiError(0, "network", err instanceof Error ? err.message : String(err)));
      }
    } finally {
      if (abortRef.current === controller) abortRef.current = null;
    }
  }, []);

  const stop = useCallback(() => {
    abortRef.current?.abort();
  }, []);

  const reset = useCallback(() => {
    sessionRef.current++;
    abortRef.current?.abort();
    abortRef.current = null;
    setState(INITIAL);
  }, []);

  return { ...state, send, stop, reset };
}
