import { useState } from "react";
import { ErrorBanner } from "../../components/feedback";
import { Badge, CodeBlock, KeyValueList, Tabs, type BadgeTone } from "../../components/ui";
import { formatNumber } from "../../lib/format";
import { codeSample, SAMPLE_LANGS, type SampleLang } from "./codeSamples";
import type {
  ChatMessage,
  EventLogEntry,
  ResponseObject,
  ResponsesRequest,
  RunError,
  RunStatus,
} from "./types";

const DISABLED_APIS = ["Chat Completions", "Embeddings", "Moderations", "Rerank"];
const NOT_EXPOSED = "Not exposed by this gateway yet";

const STATUS_BADGE: Record<Exclude<RunStatus, "idle">, { label: string; tone: BadgeTone }> = {
  streaming: { label: "Running", tone: "blue" },
  completed: { label: "Completed", tone: "green" },
  failed: { label: "Failed", tone: "red" },
  cancelled: { label: "Cancelled", tone: "amber" },
};

const selectStyle = {
  width: "100%",
  padding: "5px 8px",
  border: "1px solid var(--line)",
  borderRadius: 8,
  background: "var(--panel)",
  fontSize: 12,
} as const;

function Toggle({
  label,
  desc,
  on,
  locked,
  disabled,
  tooltip,
  onChange,
}: {
  label: string;
  desc: string;
  on: boolean;
  locked?: boolean;
  disabled?: boolean;
  tooltip?: string;
  onChange?: (on: boolean) => void;
}) {
  const inert = locked || disabled;
  return (
    <div
      title={tooltip}
      style={{ display: "flex", alignItems: "center", gap: 8, opacity: inert ? 0.6 : 1 }}
    >
      <button
        role="switch"
        aria-checked={on}
        aria-label={label}
        disabled={inert}
        onClick={() => onChange?.(!on)}
        style={{
          width: 26,
          height: 15,
          flex: "none",
          borderRadius: "var(--radius-pill)",
          border: "none",
          background: on ? "var(--blue)" : "var(--line)",
          position: "relative",
          cursor: inert ? "not-allowed" : "pointer",
          padding: 0,
        }}
      >
        <span
          style={{
            position: "absolute",
            top: 2,
            left: on ? 13 : 2,
            width: 11,
            height: 11,
            borderRadius: "50%",
            background: "#fff",
            transition: "left .12s ease",
          }}
        />
      </button>
      <div style={{ minWidth: 0 }}>
        <div style={{ fontSize: 12, fontWeight: 600, display: "flex", alignItems: "center", gap: 6 }}>
          {label}
          {locked && <Badge tone="neutral">locked</Badge>}
        </div>
        <div style={{ fontSize: 11, color: "var(--ink3)" }}>{desc}</div>
      </div>
    </div>
  );
}

function EventsLog({ events }: { events: EventLogEntry[] }) {
  if (events.length === 0) {
    return (
      <div style={{ fontSize: 12, color: "var(--ink3)" }}>
        No events yet — send with Stream on to see SSE frames.
      </div>
    );
  }
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
      {events.map((e, i) => (
        <div
          key={i}
          style={{
            display: "grid",
            gridTemplateColumns: "44px minmax(0, 1fr)",
            columnGap: 8,
            rowGap: 1,
            padding: "5px 8px",
            border: "1px solid var(--line)",
            borderRadius: "var(--radius-sm)",
            background: "var(--panel)",
          }}
        >
          <span style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--ink3)" }}>
            {e.seq ?? "—"}
          </span>
          <span style={{ fontFamily: "var(--font-mono)", fontSize: 11, fontWeight: 600, overflowWrap: "anywhere" }}>
            {e.event}
          </span>
          <span style={{ fontFamily: "var(--font-mono)", fontSize: 10, color: "var(--ink3)" }}>{e.at}</span>
          <span style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--ink2)", overflowWrap: "anywhere" }}>
            {e.payload}
          </span>
        </div>
      ))}
    </div>
  );
}

export interface InspectorPanelProps {
  models: Array<{ id: string; ownedBy: string }>;
  model: string;
  onModelChange: (model: string) => void;
  modelMissing: boolean;
  stream: boolean;
  onStreamChange: (on: boolean) => void;
  status: RunStatus;
  /** Last assistant message — source of the Rendered tab. */
  assistant: ChatMessage | undefined;
  events: EventLogEntry[];
  finalResponse: ResponseObject | null;
  requestBody: ResponsesRequest | null;
  error: RunError | null;
  durationMs: number | null;
}

export function InspectorPanel({
  models,
  model,
  onModelChange,
  modelMissing,
  stream,
  onStreamChange,
  status,
  assistant,
  events,
  finalResponse,
  requestBody,
  error,
  durationMs,
}: InspectorPanelProps) {
  const [tab, setTab] = useState("Rendered");
  const [lang, setLang] = useState<SampleLang>("curl");

  const usage = finalResponse?.usage ?? null;
  const origin = window.location.origin;

  const metaParts: string[] = [];
  if (status !== "idle" && status !== "streaming") {
    if (finalResponse?.id) metaParts.push(finalResponse.id);
    if (usage) metaParts.push(`${formatNumber(usage.input_tokens)} in / ${formatNumber(usage.output_tokens)} out`);
    if (durationMs !== null) metaParts.push(`${(durationMs / 1000).toFixed(2)}s`);
    metaParts.push(status);
  }

  return (
    <aside
      style={{
        width: 400,
        flex: "none",
        display: "flex",
        flexDirection: "column",
        minHeight: 0,
        background: "var(--panel)",
        border: "1px solid var(--line)",
        borderRadius: "var(--radius-lg)",
        boxShadow: "var(--shadow)",
      }}
      aria-label="Request inspector"
    >
      <header
        style={{
          display: "flex",
          alignItems: "center",
          justifyContent: "space-between",
          padding: "12px 16px",
          borderBottom: "1px solid var(--line)",
          flex: "none",
        }}
      >
        <h2 style={{ fontSize: 13, fontWeight: 600 }}>Request inspector</h2>
        {status !== "idle" && <Badge tone={STATUS_BADGE[status].tone}>{STATUS_BADGE[status].label}</Badge>}
      </header>

      <div style={{ flex: "none", padding: "12px 16px", borderBottom: "1px solid var(--line)", display: "flex", flexDirection: "column", gap: 10 }}>
        <div>
          <label style={{ display: "block", fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 4 }}>
            API
          </label>
          <select value="Responses" onChange={() => undefined} aria-label="API" style={selectStyle}>
            <option value="Responses">Responses — POST /llm/responses</option>
            {DISABLED_APIS.map((api) => (
              <option key={api} value={api} disabled title={NOT_EXPOSED}>
                {api}
              </option>
            ))}
          </select>
          <div style={{ fontSize: 11, color: "var(--ink3)", marginTop: 3 }}>
            Chat Completions, Embeddings, Moderations, Rerank — {NOT_EXPOSED.toLowerCase()}.
          </div>
        </div>
        <div>
          <label style={{ display: "block", fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 4 }}>
            Model
          </label>
          <select
            value={model}
            onChange={(e) => onModelChange(e.target.value)}
            aria-label="Model"
            style={{ ...selectStyle, fontFamily: "var(--font-mono)", fontSize: 12 }}
          >
            {modelMissing && (
              <option value={model} disabled>
                {model} (not in list)
              </option>
            )}
            {models.map((m) => (
              <option key={m.id} value={m.id} title={`owned_by: ${m.ownedBy}`}>
                {m.id}
              </option>
            ))}
          </select>
        </div>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 10 }}>
          <Toggle label="Stream" desc="Server-sent events" on={stream} onChange={onStreamChange} />
          <Toggle
            label="Store"
            desc="Retrievable via GET /v1/responses/:id"
            on={false}
            locked
            tooltip="Tenant policy requires store:false"
          />
          <Toggle label="Background" desc="Execute async; poll for completion" on={false} disabled tooltip="Not supported yet" />
          <Toggle label="JSON mode" desc="Force valid JSON output" on={false} disabled tooltip="Not supported yet" />
        </div>
      </div>

      <div style={{ flex: 1, overflowY: "auto", minHeight: 0, padding: "12px 16px 16px", display: "flex", flexDirection: "column", gap: 12 }}>
        {error &&
          (error.apiError ? (
            <ErrorBanner error={error.apiError} />
          ) : (
            <div
              role="alert"
              style={{
                padding: "12px 14px",
                borderRadius: "var(--radius)",
                background: "var(--red-bg)",
                color: "var(--red)",
                fontSize: 12,
              }}
            >
              <span style={{ fontWeight: 700 }}>{error.code}</span>{" "}
              <span>
                {error.friendly} — {error.message}
              </span>
            </div>
          ))}

        <div>
          <Tabs tabs={["Rendered", "Raw JSON", "Events", "Usage", "Request"]} active={tab} onChange={setTab} />
          {tab === "Rendered" &&
            (assistant && assistant.text.length > 0 ? (
              <div style={{ fontSize: 13, lineHeight: 1.6, whiteSpace: "pre-wrap" }}>
                {assistant.text}
                {assistant.status === "streaming" && (
                  <span style={{ color: "var(--blue)", animation: "pg-blink 1s steps(1) infinite" }} aria-hidden>
                    ▍
                  </span>
                )}
              </div>
            ) : (
              <div style={{ fontSize: 12, color: "var(--ink3)" }}>
                {status === "streaming" ? "Waiting for the first delta…" : "Send a request to see the rendered output."}
              </div>
            ))}
          {tab === "Raw JSON" && (
            <CodeBlock
              code={finalResponse ? JSON.stringify(finalResponse, null, 2) : "// Send a request to see the response body"}
              lang="json"
            />
          )}
          {tab === "Events" && <EventsLog events={events} />}
          {tab === "Usage" &&
            (usage ? (
              <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
                <KeyValueList
                  items={[
                    { key: "Input tokens", value: formatNumber(usage.input_tokens), mono: true },
                    { key: "Output tokens", value: formatNumber(usage.output_tokens), mono: true },
                    { key: "Total tokens", value: formatNumber(usage.total_tokens), mono: true },
                    {
                      key: "Cached input tokens",
                      value: usage.cached_input_tokens !== undefined ? formatNumber(usage.cached_input_tokens) : "—",
                      mono: true,
                    },
                  ]}
                />
                <div style={{ fontSize: 11, color: "var(--ink3)" }}>
                  Cost: n/a — pricing is not wired into this console. Authoritative spend lives in Usage &amp; Metering.
                </div>
              </div>
            ) : (
              <div style={{ fontSize: 12, color: "var(--ink3)" }}>No usage yet — usage arrives with the final response.</div>
            ))}
          {tab === "Request" && (
            <CodeBlock
              code={requestBody ? JSON.stringify(requestBody, null, 2) : "// Send a request to see the exact body"}
              lang="json"
            />
          )}
        </div>

        {metaParts.length > 0 && (
          <div style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--ink3)" }}>
            {metaParts.join(" · ")}
          </div>
        )}

        <div>
          <div style={{ display: "flex", alignItems: "center", gap: 4, marginBottom: 8 }}>
            <span style={{ fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginRight: 4 }}>Code samples</span>
            {SAMPLE_LANGS.map((l) => (
              <button
                key={l}
                onClick={() => setLang(l)}
                style={{
                  border: "none",
                  padding: "3px 10px",
                  fontSize: 11,
                  borderRadius: "var(--radius-sm)",
                  background: lang === l ? "var(--blue-bg)" : "transparent",
                  color: lang === l ? "var(--blue)" : "var(--ink2)",
                  fontWeight: lang === l ? 600 : 400,
                }}
              >
                {l}
              </button>
            ))}
          </div>
          <CodeBlock code={codeSample(lang, origin, model || "<model>")} lang={lang === "curl" ? "bash" : lang} />
        </div>
      </div>
    </aside>
  );
}
