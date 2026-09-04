import { useEffect, useRef, type KeyboardEvent } from "react";
import { Button, Spinner } from "../../components/ui";
import type { ChatMessage, RunStatus } from "./types";

const SUGGESTIONS = [
  "Explain liveness vs readiness",
  "Why was my request quota-denied?",
  "What does store:false mean?",
];

interface ChatPanelProps {
  model: string;
  /** Capability guard: selected model is absent from the current model list. */
  modelMissing: boolean;
  messages: ChatMessage[];
  status: RunStatus;
  input: string;
  onInput: (value: string) => void;
  onSend: (text?: string) => void;
  onStop: () => void;
  onNewChat: () => void;
}

function Cursor() {
  return (
    <span style={{ color: "var(--blue)", animation: "pg-blink 1s steps(1) infinite" }} aria-hidden>
      ▍
    </span>
  );
}

export function ChatPanel({
  model,
  modelMissing,
  messages,
  status,
  input,
  onInput,
  onSend,
  onStop,
  onNewChat,
}: ChatPanelProps) {
  const boxRef = useRef<HTMLDivElement>(null);
  const streaming = status === "streaming";
  const canSend = !streaming && input.trim().length > 0 && model.length > 0 && !modelMissing;

  useEffect(() => {
    const el = boxRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [messages, status]);

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      onSend();
    }
  };

  return (
    <div style={{ flex: 1, minWidth: 0, display: "flex", flexDirection: "column", minHeight: 0 }}>
      <style>{"@keyframes pg-blink{0%,100%{opacity:1}50%{opacity:0}}"}</style>
      <div
        style={{
          display: "flex",
          alignItems: "center",
          gap: 8,
          flexWrap: "wrap",
          paddingBottom: 12,
          borderBottom: "1px solid var(--line)",
          flex: "none",
        }}
      >
        <div style={{ flex: 1, minWidth: 200 }}>
          <h1 style={{ margin: 0, fontSize: 16, fontWeight: 600 }}>Playground</h1>
          <div style={{ color: "var(--ink3)", fontSize: 12 }}>
            Chats run through the gateway as a tenant workload — metered and quota-checked.
          </div>
        </div>
        <Button onClick={onNewChat}>New chat</Button>
      </div>

      {modelMissing && (
        <div
          role="alert"
          style={{
            flex: "none",
            marginTop: 10,
            padding: "8px 11px",
            borderRadius: 8,
            background: "var(--amber-bg)",
            border: "1px solid var(--amber)",
            fontSize: 12,
          }}
        >
          <b style={{ color: "var(--amber)" }}>Model unavailable</b>{" "}
          <span style={{ color: "var(--ink2)" }}>
            <span style={{ fontFamily: "var(--font-mono)" }}>{model}</span> is not in the current model list — sending
            is disabled.
          </span>
        </div>
      )}

      <div
        ref={boxRef}
        style={{ flex: 1, overflowY: "auto", minHeight: 0, padding: "16px 2px", display: "flex", flexDirection: "column", gap: 14 }}
      >
        {messages.length === 0 ? (
          <div style={{ margin: "auto", textAlign: "center", color: "var(--ink3)", fontSize: 13, padding: "30px 0" }}>
            <div
              style={{
                width: 38,
                height: 38,
                borderRadius: 11,
                background: "var(--blue-bg)",
                color: "var(--blue)",
                display: "grid",
                placeItems: "center",
                margin: "0 auto 12px",
              }}
            >
              <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
                <path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
              </svg>
            </div>
            <div style={{ fontWeight: 600, color: "var(--ink)", fontSize: 13.5, marginBottom: 4 }}>Start a conversation</div>
            <div style={{ marginBottom: 14 }}>
              Messages go to{" "}
              <span style={{ fontFamily: "var(--font-mono)", color: "var(--purple)" }}>{model || "…"}</span> via POST
              /llm/responses.
            </div>
            <div style={{ display: "flex", flexDirection: "column", gap: 6, alignItems: "center" }}>
              {SUGGESTIONS.map((s) => (
                <button
                  key={s}
                  onClick={() => onSend(s)}
                  disabled={modelMissing || !model}
                  style={{
                    padding: "6px 14px",
                    border: "1px solid var(--line)",
                    borderRadius: "var(--radius-pill)",
                    background: "var(--panel)",
                    color: "var(--ink2)",
                    fontSize: 12,
                    cursor: modelMissing || !model ? "not-allowed" : "pointer",
                  }}
                >
                  {s}
                </button>
              ))}
            </div>
          </div>
        ) : (
          messages.map((m) => {
            if (m.role === "system") {
              return (
                <div
                  key={m.id}
                  role="alert"
                  style={{
                    alignSelf: "stretch",
                    padding: "8px 11px",
                    borderRadius: 8,
                    background: "var(--red-bg)",
                    border: "1px solid var(--red)",
                    fontSize: 12,
                  }}
                >
                  <b style={{ color: "var(--red)" }}>Request failed</b>{" "}
                  <span style={{ color: "var(--ink2)" }}>{m.text}</span>
                </div>
              );
            }
            const isUser = m.role === "user";
            return (
              <div key={m.id} style={{ display: "flex", flexDirection: "column", alignItems: isUser ? "flex-end" : "flex-start" }}>
                <div
                  style={{
                    maxWidth: "78%",
                    padding: "9px 13px",
                    borderRadius: 12,
                    background: isUser ? "var(--blue)" : "var(--panel)",
                    color: isUser ? "#fff" : "var(--ink)",
                    border: `1px solid ${
                      isUser ? "var(--blue)" : m.status === "failed" ? "var(--red)" : "var(--line)"
                    }`,
                    whiteSpace: "pre-wrap",
                    fontSize: 13,
                    lineHeight: 1.6,
                  }}
                >
                  {!isUser && m.status === "streaming" && m.text.length === 0 ? (
                    <Spinner size={14} />
                  ) : (
                    <>
                      {m.text}
                      {!isUser && m.status === "streaming" && <Cursor />}
                    </>
                  )}
                </div>
                {m.meta && (
                  <div style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--ink3)", marginTop: 5 }}>
                    {m.meta}
                  </div>
                )}
              </div>
            );
          })
        )}
      </div>

      <div style={{ flex: "none", padding: "8px 0 4px" }}>
        <div
          style={{
            display: "flex",
            gap: 8,
            alignItems: "flex-end",
            background: "var(--panel)",
            border: "1px solid var(--line)",
            borderRadius: 12,
            padding: "8px 8px 8px 14px",
            boxShadow: "var(--shadow)",
          }}
        >
          <textarea
            rows={2}
            value={input}
            onChange={(e) => onInput(e.target.value)}
            onKeyDown={onKeyDown}
            placeholder={model ? `Message ${model}…` : "Message…"}
            aria-label="Message"
            style={{
              flex: 1,
              border: "none",
              background: "transparent",
              resize: "none",
              fontSize: 13,
              lineHeight: 1.5,
              padding: "4px 0",
              maxHeight: 120,
              outline: "none",
            }}
          />
          {streaming ? (
            <button
              onClick={onStop}
              title="Stop"
              aria-label="Stop"
              style={{
                width: 32,
                height: 32,
                flex: "none",
                border: "none",
                borderRadius: 9,
                background: "var(--red)",
                color: "#fff",
                display: "grid",
                placeItems: "center",
              }}
            >
              <svg width="12" height="12" viewBox="0 0 24 24" fill="currentColor" aria-hidden>
                <rect x="6" y="6" width="12" height="12" rx="2" />
              </svg>
            </button>
          ) : (
            <button
              onClick={() => onSend()}
              disabled={!canSend}
              title="Send"
              aria-label="Send"
              style={{
                width: 32,
                height: 32,
                flex: "none",
                border: "none",
                borderRadius: 9,
                background: "var(--blue)",
                color: "#fff",
                display: "grid",
                placeItems: "center",
                opacity: canSend ? 1 : 0.5,
                cursor: canSend ? "pointer" : "not-allowed",
              }}
            >
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
                <path d="M12 19V5M5 12l7-7 7 7" />
              </svg>
            </button>
          )}
        </div>
        <div style={{ display: "flex", gap: 12, marginTop: 6, fontSize: 11, color: "var(--ink3)", flexWrap: "wrap" }}>
          <span>Enter to send · Shift+Enter for a new line</span>
          <span>store:false enforced · single-turn · history is client-side only</span>
        </div>
      </div>
    </div>
  );
}
