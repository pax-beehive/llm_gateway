import { useMemo, useState } from "react";
import { ErrorBanner, Loading } from "../../components/feedback";
import { Badge, Button, Card, EmptyState, StatCard } from "../../components/ui";
import { BarChart } from "../../components/charts";
import { formatMicrosUSD, formatNumber, timeAgo } from "../../lib/format";
import { ExportJobs } from "./exports";
import { useQuery } from "./hooks";
import { EventLedger } from "./ledger";
import { UsageLookups } from "./lookups";
import {
  combineTotals,
  usageQuery,
  type MeteringStatus,
  type Summary,
  type TimePoint,
  type UsageFilter,
} from "./types";

const TENANT_KEY = "ugw.usage.tenant";

type RangePreset = "24h" | "7d" | "30d";
type Granularity = "hour" | "day";

const RANGE_HOURS: Record<RangePreset, number> = { "24h": 24, "7d": 7 * 24, "30d": 30 * 24 };

const inputStyle = {
  padding: "6px 10px",
  border: "1px solid var(--line)",
  borderRadius: "var(--radius)",
  background: "var(--panel)",
  fontSize: 12,
} as const;

const monoInputStyle = { ...inputStyle, fontFamily: "var(--font-mono)", fontSize: 11 } as const;

interface AppliedFilters {
  tenantId: string;
  range: RangePreset;
  granularity: Granularity;
  apiKeyId: string;
  provider: string;
  publicModel: string;
  outcome: string;
  from: Date;
  through: Date;
}

function buildApplied(tenantId: string, range: RangePreset, granularity: Granularity): AppliedFilters {
  const through = new Date();
  const from = new Date(through.getTime() - RANGE_HOURS[range] * 3_600_000);
  return { tenantId, range, granularity, apiKeyId: "", provider: "", publicModel: "", outcome: "", from, through };
}

export default function UsagePage() {
  const [tenantDraft, setTenantDraft] = useState(() => window.localStorage.getItem(TENANT_KEY) ?? "");
  const [range, setRange] = useState<RangePreset>("24h");
  const [granularity, setGranularity] = useState<Granularity>("hour");
  const [apiKeyId, setApiKeyId] = useState("");
  const [provider, setProvider] = useState("");
  const [publicModel, setPublicModel] = useState("");
  const [outcome, setOutcome] = useState("");
  const [applied, setApplied] = useState<AppliedFilters | null>(() => {
    const stored = window.localStorage.getItem(TENANT_KEY)?.trim();
    return stored ? buildApplied(stored, "24h", "hour") : null;
  });
  const [refreshKey, setRefreshKey] = useState(0);

  const status = useQuery<MeteringStatus>("/metering/v1/operations/status", refreshKey);

  const apply = () => {
    const tenantId = tenantDraft.trim();
    if (!tenantId) return;
    window.localStorage.setItem(TENANT_KEY, tenantId);
    setApplied({ ...buildApplied(tenantId, range, granularity), apiKeyId, provider, publicModel, outcome });
  };

  const filter: UsageFilter | null = useMemo(
    () =>
      applied && {
        tenantId: applied.tenantId,
        apiKeyId: applied.apiKeyId || undefined,
        provider: applied.provider || undefined,
        publicModel: applied.publicModel || undefined,
        outcome: applied.outcome || undefined,
        from: applied.from,
        through: applied.through,
      },
    [applied],
  );

  const query = filter ? usageQuery(filter) : null;
  const summary = useQuery<Summary>(query ? `/metering/v1/usage/summary?${query}` : null, refreshKey);
  const series = useQuery<{ data: TimePoint[] | null }>(
    query && applied ? `/metering/v1/usage/timeseries?${query}&granularity=${applied.granularity}` : null,
    refreshKey,
  );

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 14, maxWidth: 1360 }}>
      <div style={{ display: "flex", alignItems: "flex-start", gap: 16 }}>
        <div style={{ flex: 1 }}>
          <h1 style={{ margin: 0, fontSize: 18, fontWeight: 600 }}>Usage &amp; Metering</h1>
          <div style={{ color: "var(--ink2)", marginTop: 2, fontSize: 12 }}>
            Financial evidence from the immutable Usage Ledger. Aggregates are projections
            {summary.data ? ` — cutoff ${timeAgo(summary.data.data_cutoff)}` : ""}.
          </div>
        </div>
        {status.data && <ProjectionStatus status={status.data} />}
        <Button onClick={() => setRefreshKey((k) => k + 1)}>Refresh</Button>
      </div>
      {status.error && <ErrorBanner error={status.error} retry={status.retry} />}

      <Card>
        <div style={{ display: "flex", flexWrap: "wrap", gap: 8, alignItems: "center" }}>
          <input
            placeholder="Tenant ID (required)"
            value={tenantDraft}
            onChange={(e) => setTenantDraft(e.target.value)}
            style={{ ...monoInputStyle, width: 200 }}
          />
          <select
            value={range}
            onChange={(e) => {
              const value = e.target.value as RangePreset;
              setRange(value);
              setGranularity(value === "24h" ? "hour" : "day");
            }}
            style={inputStyle}
          >
            <option value="24h">Last 24 hours</option>
            <option value="7d">Last 7 days</option>
            <option value="30d">Last 30 days</option>
          </select>
          <input
            placeholder="API key ID"
            value={apiKeyId}
            onChange={(e) => setApiKeyId(e.target.value)}
            style={{ ...monoInputStyle, width: 140 }}
          />
          <input
            placeholder="Provider"
            value={provider}
            onChange={(e) => setProvider(e.target.value)}
            style={{ ...inputStyle, width: 110 }}
          />
          <input
            placeholder="Public model"
            value={publicModel}
            onChange={(e) => setPublicModel(e.target.value)}
            style={{ ...monoInputStyle, width: 130 }}
          />
          <input
            placeholder="Outcome (e.g. committed)"
            value={outcome}
            onChange={(e) => setOutcome(e.target.value)}
            style={{ ...inputStyle, width: 150 }}
          />
          <GranularityToggle value={granularity} onChange={setGranularity} />
          <span style={{ flex: 1 }} />
          <Button variant="primary" disabled={tenantDraft.trim() === ""} onClick={apply}>
            Apply
          </Button>
        </div>
      </Card>

      {!applied && (
        <Card>
          <EmptyState
            title="Select a tenant to view usage"
            hint="The metering API is tenant-scoped: every usage query requires a tenant ID."
            action={
              tenantDraft.trim() !== "" ? (
                <Button variant="primary" onClick={apply}>
                  Apply
                </Button>
              ) : undefined
            }
          />
        </Card>
      )}

      {applied && filter && query && (
        <>
          {summary.loading && <Loading />}
          {summary.error && <ErrorBanner error={summary.error} retry={summary.retry} />}
          {summary.data && <SummaryCards summary={summary.data} />}

          <ChartsRow series={series.data?.data ?? null} currency={summary.data?.totals[0]?.currency ?? "USD"} />

          <EventLedger query={query} refreshKey={refreshKey} />
          <div style={{ display: "grid", gridTemplateColumns: "minmax(0,1.5fr) minmax(0,1fr)", gap: 14, alignItems: "start" }}>
            <ExportJobs filter={filter} refreshKey={refreshKey} />
            <UsageLookups tenantId={filter.tenantId} />
          </div>
        </>
      )}
    </div>
  );
}

function ProjectionStatus({ status }: { status: MeteringStatus }) {
  return (
    <div style={{ display: "flex", gap: 6, flexWrap: "wrap", alignItems: "center", justifyContent: "flex-end" }}>
      <Badge tone="purple">projection gen {status.projection_generation}</Badge>
      <Badge tone="neutral">cutoff {timeAgo(status.projection_cutoff)}</Badge>
      <Badge tone={status.pending_events > 0 ? "amber" : "green"}>{formatNumber(status.pending_events)} pending</Badge>
      {status.poison_events > 0 && <Badge tone="red">{formatNumber(status.poison_events)} poison</Badge>}
      {status.queued_exports > 0 && <Badge tone="blue">{formatNumber(status.queued_exports)} exports queued</Badge>}
    </div>
  );
}

function GranularityToggle({ value, onChange }: { value: Granularity; onChange: (g: Granularity) => void }) {
  return (
    <div style={{ display: "flex", border: "1px solid var(--line)", borderRadius: "var(--radius)", overflow: "hidden" }}>
      {(["hour", "day"] as const).map((g) => (
        <button
          key={g}
          onClick={() => onChange(g)}
          style={{
            border: "none",
            padding: "5px 12px",
            fontSize: 11,
            fontWeight: value === g ? 600 : 500,
            background: value === g ? "var(--blue-bg)" : "transparent",
            color: value === g ? "var(--blue)" : "var(--ink2)",
            cursor: "pointer",
          }}
        >
          {g === "hour" ? "Hourly" : "Daily"}
        </button>
      ))}
    </div>
  );
}

function SummaryCards({ summary }: { summary: Summary }) {
  const totals = combineTotals(summary.totals);
  const cutoff = `projection · cutoff ${timeAgo(summary.data_cutoff)}`;
  return (
    <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit,minmax(150px,1fr))", gap: 10 }}>
      <StatCard label="Requests" value={formatNumber(totals.operation_count)} sub={cutoff} />
      <StatCard label="Input tokens" value={formatNumber(totals.input_tokens)} sub={cutoff} />
      <StatCard label="Cached input tokens" value={formatNumber(totals.cached_input_tokens)} sub="served from cache" />
      <StatCard label="Cache write tokens" value={formatNumber(totals.cache_write_input_tokens)} sub="written to cache" />
      <StatCard label="Output tokens" value={formatNumber(totals.output_tokens)} sub={cutoff} />
      <StatCard label="Input units" value={formatNumber(totals.input_units)} sub="capability operations" />
      <StatCard label="Documents" value={formatNumber(totals.documents)} sub={cutoff} />
      {summary.totals.length === 0 ? (
        <StatCard label="Spend" value={formatMicrosUSD(0)} sub={cutoff} />
      ) : (
        summary.totals.map((row) => (
          <StatCard
            key={row.currency}
            label={summary.totals.length > 1 ? `Spend (${row.currency})` : "Spend"}
            value={row.currency === "USD" ? formatMicrosUSD(row.amount_micros) : `${formatNumber(row.amount_micros)} micros`}
            sub={cutoff}
          />
        ))
      )}
    </div>
  );
}

function ChartsRow({ series, currency }: { series: TimePoint[] | null; currency: string }) {
  const buckets = useMemo(() => {
    const byStart = new Map<string, { spend: number; tokens: number }>();
    for (const point of series ?? []) {
      const bucket = byStart.get(point.start) ?? { spend: 0, tokens: 0 };
      if (point.totals.currency === currency) bucket.spend += point.totals.amount_micros / 1_000_000;
      bucket.tokens += point.totals.input_tokens + point.totals.output_tokens;
      byStart.set(point.start, bucket);
    }
    return [...byStart.entries()]
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([start, value]) => ({ start, ...value }));
  }, [series, currency]);

  const label = (start: string) => {
    const d = new Date(start);
    return Number.isNaN(d.getTime())
      ? start
      : d.toLocaleString("en-US", { month: "short", day: "numeric", hour: "2-digit", hour12: false });
  };

  return (
    <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit,minmax(320px,1fr))", gap: 14 }}>
      <Card title={<>Spend over time <span style={{ fontWeight: 400, color: "var(--ink3)" }}>· {currency}/bucket</span></>}>
        {buckets.length === 0 ? (
          <EmptyState title="No usage in range" hint="Widen the time range or relax filters" />
        ) : (
          <BarChart data={buckets.map((b) => ({ label: label(b.start), value: b.spend }))} height={120} color="var(--blue)" />
        )}
      </Card>
      <Card title={<>Tokens over time <span style={{ fontWeight: 400, color: "var(--ink3)" }}>· in+out/bucket</span></>}>
        {buckets.length === 0 ? (
          <EmptyState title="No usage in range" hint="Widen the time range or relax filters" />
        ) : (
          <BarChart data={buckets.map((b) => ({ label: label(b.start), value: b.tokens }))} height={120} color="var(--purple)" />
        )}
      </Card>
    </div>
  );
}
