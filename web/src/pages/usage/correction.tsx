import { useEffect, useState } from "react";
import { apiSend, ApiError } from "../../api/client";
import { Modal, useToast } from "../../components/feedback";
import { Button } from "../../components/ui";
import type { UsageEvent } from "./types";

const inputStyle = {
  width: "100%",
  boxSizing: "border-box",
  padding: "6px 10px",
  border: "1px solid var(--line)",
  borderRadius: "var(--radius)",
  background: "var(--bg)",
  fontSize: 12,
} as const;

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label style={{ display: "block" }}>
      <span style={{ display: "block", fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 4 }}>
        {label}
      </span>
      {children}
    </label>
  );
}

/**
 * Append-only usage correction (POST /usage/events/{id}/corrections). The delta
 * event carries only signed numeric deltas; the service copies every other
 * dimension from the original event. Reason and Idempotency-Key are required
 * by the API.
 */
export function ConfirmCorrection({
  event,
  onClose,
  onDone,
}: {
  event: UsageEvent | null;
  onClose: () => void;
  onDone: (created: UsageEvent) => void;
}) {
  const toast = useToast();
  const [reason, setReason] = useState("");
  const [idempotencyKey, setIdempotencyKey] = useState("");
  const [amountMicros, setAmountMicros] = useState("0");
  const [inputTokens, setInputTokens] = useState("0");
  const [outputTokens, setOutputTokens] = useState("0");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (event) {
      setReason("");
      setIdempotencyKey(crypto.randomUUID());
      setAmountMicros("0");
      setInputTokens("0");
      setOutputTokens("0");
      setBusy(false);
    }
  }, [event]);

  const submit = () => {
    if (!event) return;
    setBusy(true);
    apiSend<UsageEvent>(
      "POST",
      `/metering/v1/usage/events/${encodeURIComponent(event.event_id)}/corrections`,
      {
        reason: reason.trim(),
        delta: {
          amount_micros: Number.parseInt(amountMicros, 10) || 0,
          input_tokens: Number.parseInt(inputTokens, 10) || 0,
          output_tokens: Number.parseInt(outputTokens, 10) || 0,
        },
      },
      { idempotencyKey },
    )
      .then(onDone)
      .catch((err: unknown) => {
        setBusy(false);
        if (err instanceof ApiError) toast(`${err.code}: ${err.message}`, "error");
        else throw err;
      });
  };

  const valid = reason.trim().length > 0 && idempotencyKey.trim().length > 0;

  return (
    <Modal
      open={event !== null}
      onClose={onClose}
      title="Append usage correction"
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="danger" disabled={!valid || busy} onClick={submit}>
            {busy ? "Appending…" : "Append correction"}
          </Button>
        </>
      }
    >
      <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
        <p style={{ fontSize: 12, color: "var(--ink2)", margin: 0 }}>
          Appends a compensating event against <code style={{ fontFamily: "var(--font-mono)" }}>{event?.event_id}</code>.
          Deltas may be negative; the original immutable event is never rewritten.
        </p>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr 1fr", gap: 8 }}>
          <Field label="Δ Amount (micros)">
            <input value={amountMicros} onChange={(e) => setAmountMicros(e.target.value)} style={{ ...inputStyle, fontFamily: "var(--font-mono)" }} />
          </Field>
          <Field label="Δ Input tokens">
            <input value={inputTokens} onChange={(e) => setInputTokens(e.target.value)} style={{ ...inputStyle, fontFamily: "var(--font-mono)" }} />
          </Field>
          <Field label="Δ Output tokens">
            <input value={outputTokens} onChange={(e) => setOutputTokens(e.target.value)} style={{ ...inputStyle, fontFamily: "var(--font-mono)" }} />
          </Field>
        </div>
        <Field label="Reason (required)">
          <textarea
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            rows={3}
            placeholder="Why is this correction needed?"
            style={{ ...inputStyle, resize: "vertical" }}
          />
        </Field>
        <Field label="Idempotency key (required)">
          <input
            value={idempotencyKey}
            onChange={(e) => setIdempotencyKey(e.target.value)}
            style={{ ...inputStyle, fontFamily: "var(--font-mono)" }}
          />
        </Field>
      </div>
    </Modal>
  );
}
