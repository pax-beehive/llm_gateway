import { useState } from "react";
import { apiGet, ApiError } from "../../api/client";
import { ErrorBanner } from "../../components/feedback";
import { Button, Card, KeyValueList } from "../../components/ui";
import { formatMicrosUSD, formatNumber, timeAgo, truncateId } from "../../lib/format";
import { combineTotals, type EventPage, type Summary, type Totals } from "./types";

const inputStyle = {
  flex: 1,
  minWidth: 0,
  padding: "6px 10px",
  border: "1px solid var(--line)",
  borderRadius: "var(--radius)",
  background: "var(--bg)",
  fontFamily: "var(--font-mono)",
  fontSize: 11,
} as const;

function totalsItems(totals: Totals, cutoff: string) {
  return [
    { key: "Currency", value: totals.currency, mono: true },
    { key: "Requests", value: formatNumber(totals.operation_count), mono: true },
    { key: "Input tokens", value: formatNumber(totals.input_tokens), mono: true },
    { key: "Cached input tokens", value: formatNumber(totals.cached_input_tokens), mono: true },
    { key: "Cache write tokens", value: formatNumber(totals.cache_write_input_tokens), mono: true },
    { key: "Output tokens", value: formatNumber(totals.output_tokens), mono: true },
    { key: "Input units", value: formatNumber(totals.input_units), mono: true },
    { key: "Documents", value: formatNumber(totals.documents), mono: true },
    { key: "Data cutoff", value: cutoff ? timeAgo(cutoff) : "—", mono: true },
  ];
}

/**
 * Scoped usage lookups. The summary API has no provider/model/tenant
 * breakdowns, so per-key and per-response usage are queried through the
 * dedicated scoped endpoints and rendered as key/value totals.
 */
export function UsageLookups({ tenantId }: { tenantId: string }) {
  return (
    <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit,minmax(320px,1fr))", gap: 14 }}>
      <KeyLookup tenantId={tenantId} />
      <ResponseLookup tenantId={tenantId} />
    </div>
  );
}

function KeyLookup({ tenantId }: { tenantId: string }) {
  const [keyId, setKeyId] = useState("");
  const [result, setResult] = useState<Summary | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [busy, setBusy] = useState(false);

  const run = () => {
    setBusy(true);
    setError(null);
    apiGet<Summary>(
      `/metering/v1/tenants/${encodeURIComponent(tenantId)}/gateway-api-keys/${encodeURIComponent(keyId.trim())}/usage`,
    )
      .then(setResult)
      .catch((err: unknown) => {
        setResult(null);
        setError(err instanceof ApiError ? err : new ApiError(0, "network", String(err)));
      })
      .finally(() => setBusy(false));
  };

  const totals = result ? combineTotals(result.totals) : null;
  const spendRows = result?.totals ?? [];

  return (
    <Card title="Usage by gateway API key">
      <div style={{ display: "flex", gap: 8, marginBottom: 12 }}>
        <input
          placeholder="Gateway API key ID"
          value={keyId}
          onChange={(e) => setKeyId(e.target.value)}
          style={inputStyle}
        />
        <Button variant="primary" disabled={keyId.trim() === "" || busy} onClick={run}>
          {busy ? "Loading…" : "Look up"}
        </Button>
      </div>
      {error && <ErrorBanner error={error} />}
      {totals && result && (
        <>
          <KeyValueList items={totalsItems(totals, result.data_cutoff)} />
          {spendRows.map((row) => (
            <div key={row.currency} style={{ marginTop: 8, fontSize: 12 }}>
              <span style={{ color: "var(--ink3)", fontSize: 11 }}>Spend ({row.currency})</span>{" "}
              <span style={{ fontFamily: "var(--font-mono)", fontWeight: 600 }}>
                {row.currency === "USD" ? formatMicrosUSD(row.amount_micros) : `${formatNumber(row.amount_micros)} micros`}
              </span>
            </div>
          ))}
          {spendRows.length === 0 && <div style={{ fontSize: 12, color: "var(--ink2)" }}>No usage recorded for this key.</div>}
        </>
      )}
    </Card>
  );
}

function ResponseLookup({ tenantId }: { tenantId: string }) {
  const [responseId, setResponseId] = useState("");
  const [result, setResult] = useState<EventPage | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [busy, setBusy] = useState(false);

  const run = () => {
    setBusy(true);
    setError(null);
    apiGet<EventPage>(
      `/metering/v1/responses/${encodeURIComponent(responseId.trim())}/usage?tenant_id=${encodeURIComponent(tenantId)}`,
    )
      .then(setResult)
      .catch((err: unknown) => {
        setResult(null);
        setError(err instanceof ApiError ? err : new ApiError(0, "network", String(err)));
      })
      .finally(() => setBusy(false));
  };

  const events = result?.data ?? [];
  const inputTokens = events.reduce((sum, e) => sum + e.input_tokens, 0);
  const outputTokens = events.reduce((sum, e) => sum + e.output_tokens, 0);
  const amountMicros = events.reduce((sum, e) => sum + e.amount_micros, 0);

  return (
    <Card title="Usage by response">
      <div style={{ display: "flex", gap: 8, marginBottom: 12 }}>
        <input
          placeholder="Response ID (rsp_…)"
          value={responseId}
          onChange={(e) => setResponseId(e.target.value)}
          style={inputStyle}
        />
        <Button variant="primary" disabled={responseId.trim() === "" || busy} onClick={run}>
          {busy ? "Loading…" : "Look up"}
        </Button>
      </div>
      {error && <ErrorBanner error={error} />}
      {result && events.length === 0 && (
        <div style={{ fontSize: 12, color: "var(--ink2)" }}>No usage events recorded for this response.</div>
      )}
      {result && events.length > 0 && (
        <KeyValueList
          items={[
            { key: "Events", value: formatNumber(events.length), mono: true },
            {
              key: "Event IDs",
              value: events.map((e) => truncateId(e.event_id)).join(", "),
              mono: true,
            },
            { key: "Input tokens", value: formatNumber(inputTokens), mono: true },
            { key: "Output tokens", value: formatNumber(outputTokens), mono: true },
            { key: "Cost", value: formatMicrosUSD(amountMicros), mono: true },
            { key: "Data cutoff", value: timeAgo(result.data_cutoff), mono: true },
          ]}
        />
      )}
    </Card>
  );
}
