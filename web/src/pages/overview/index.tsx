/**
 * Overview — operational readiness across gateways, providers, and metering.
 * Every card loads independently: a failing or unconfigured upstream shows an
 * inline error (or the amber configuration banner) in that card only.
 */
import type { ReactNode } from "react";
import { apiGet } from "../../api/client";
import { BarChart, LineChart } from "../../components/charts";
import { ErrorBanner } from "../../components/feedback";
import { Badge, Button, Card, EmptyState, Spinner, StatCard, Table, Td, Th } from "../../components/ui";
import { formatMicrosUSD, formatNumber, timeAgo } from "../../lib/format";
import { navigate } from "../../router";
import {
  aggregateUsageSeries24h,
  aggregateUsageToday,
  deriveWarnings,
  formatRevision,
  latestPublication,
  loadReadiness,
  statusTone,
  useAutoRefresh,
  useResource,
  type GatewayPage,
  type LLMModelList,
  type MeteringPage,
  type MeteringSummary,
  type ProviderConnection,
  type ProviderConnectionPage,
  type PublicationPage,
  type PublicationSummary,
  type Resource,
  type ServiceReadiness,
  type UsageAggregate,
  type UsageSeries,
} from "./lib";

/* ------------------------------------------------------------------ */
/* Card body: loading / error / content                                */
/* ------------------------------------------------------------------ */

function CardBody<T>({ res, children }: { res: Resource<T>; children: (data: T) => ReactNode }) {
  if (res.loading && !res.data) {
    return (
      <div style={{ display: "flex", justifyContent: "center", padding: "20px 0" }}>
        <Spinner size={16} />
      </div>
    );
  }
  if (res.error) return <ErrorBanner error={res.error} retry={res.reload} />;
  if (!res.data) return null;
  return <>{children(res.data)}</>;
}

/* ------------------------------------------------------------------ */
/* Stat cards                                                          */
/* ------------------------------------------------------------------ */

function ReadinessCard({ res }: { res: Resource<ServiceReadiness[]> }) {
  return (
    <Card>
      <div style={{ fontSize: 11, color: "var(--ink3)", fontWeight: 600, textTransform: "uppercase", letterSpacing: ".04em" }}>
        Service readiness
      </div>
      <CardBody res={res}>
        {(services) => {
          const checks = services.flatMap((s) => (s.readiness?.checks ?? []).map((c) => ({ service: s.service, ...c })));
          const ready = checks.filter((c) => c.status === "ready").length;
          return (
            <>
              <div style={{ fontSize: 22, fontWeight: 700, marginTop: 4 }}>
                {ready} / {checks.length} <span style={{ fontSize: 12, fontWeight: 500, color: "var(--ink2)" }}>checks ready</span>
              </div>
              <div style={{ display: "flex", flexWrap: "wrap", gap: 4, marginTop: 8 }}>
                {checks.map((c) => (
                  <Badge key={`${c.service}:${c.name}`} tone={c.status === "ready" ? "green" : "red"}>
                    {c.service}·{c.name}
                  </Badge>
                ))}
              </div>
              {services
                .filter((s) => s.error)
                .map((s) =>
                  s.error?.isUpstreamNotConfigured ? (
                    <div key={s.service} style={{ fontSize: 11, color: "var(--amber)", marginTop: 6 }}>
                      {s.service} upstream is not configured on the BFF — set the corresponding BFF_*_TOKEN and reload
                    </div>
                  ) : (
                    <div key={s.service} style={{ fontSize: 11, color: "var(--red)", marginTop: 6 }}>
                      {s.service}: {s.error?.code}
                    </div>
                  ),
                )}
            </>
          );
        }}
      </CardBody>
    </Card>
  );
}

function UsageStatCards({ res }: { res: Resource<UsageAggregate> }) {
  const sub = "platform projection";
  if (res.error) {
    return (
      <>
        <StatCard label="Requests today" value="—" sub={<span style={{ color: "var(--red)" }}>{res.error.code}</span>} />
        <StatCard label="Spend today" value="—" sub={<span style={{ color: "var(--red)" }}>{res.error.code}</span>} />
      </>
    );
  }
  if (res.loading && !res.data) {
    return (
      <>
        <StatCard label="Requests today" value={<Spinner size={16} />} />
        <StatCard label="Spend today" value={<Spinner size={16} />} />
      </>
    );
  }
  const a = res.data;
  if (!a) return null;
  return (
    <>
      <StatCard label="Requests today" value={formatNumber(a.requests)} sub={sub} />
      <StatCard
        label="Spend today"
        value={formatMicrosUSD(a.spendMicros)}
        sub={a.cutoff ? `${sub} · cutoff ${timeAgo(a.cutoff)}` : `${sub} · no events yet`}
      />
    </>
  );
}

/* ------------------------------------------------------------------ */
/* Request volume & spend chart                                        */
/* ------------------------------------------------------------------ */

function UsageChartCard({ res }: { res: Resource<UsageSeries> }) {
  return (
    <Card
      title="Request volume & spend"
      actions={
        <span style={{ display: "flex", gap: 14, fontSize: 11, color: "var(--ink2)" }}>
          <span style={{ display: "flex", alignItems: "center", gap: 5 }}>
            <span style={{ width: 9, height: 9, borderRadius: 2, background: "var(--blue)", opacity: 0.35 }} />
            requests (bars)
          </span>
          <span style={{ display: "flex", alignItems: "center", gap: 5 }}>
            <span style={{ width: 12, borderTop: "2px solid var(--blue)" }} />
            spend (line)
          </span>
        </span>
      }
    >
      <CardBody res={res}>
        {(series) => {
          const hasData = series.buckets.some((b) => b.requests > 0 || b.spendMicros > 0);
          if (!hasData) {
            return (
              <EmptyState
                title="No usage in the last 24 hours"
                hint="Metering recorded no usage events in this window"
              />
            );
          }
          const peakRequests = Math.max(...series.buckets.map((b) => b.requests));
          const peakMicros = Math.max(...series.buckets.map((b) => b.spendMicros));
          return (
            <>
              <BarChart data={series.buckets.map((b) => ({ label: b.label, value: b.requests }))} height={130} color="var(--blue)" />
              <div style={{ display: "flex", justifyContent: "space-between", fontFamily: "var(--font-mono)", fontSize: 10, color: "var(--ink3)", margin: "2px 0 8px" }}>
                <span>{series.buckets[0]?.label}</span>
                <span>{series.buckets[series.buckets.length - 1]?.label}</span>
              </div>
              <LineChart data={series.buckets.map((b) => b.spendMicros / 1_000_000)} height={70} color="var(--blue)" />
              <div style={{ fontSize: 11, color: "var(--ink3)", marginTop: 6, fontFamily: "var(--font-mono)" }}>
                peak {formatNumber(peakRequests)} req/h · spend peak {formatMicrosUSD(peakMicros)}/h
              </div>
            </>
          );
        }}
      </CardBody>
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/* Warnings                                                            */
/* ------------------------------------------------------------------ */

function WarningsCard({ warnings }: { warnings: ReturnType<typeof deriveWarnings> }) {
  return (
    <Card
      title={
        <span style={{ display: "flex", alignItems: "center", gap: 8 }}>
          <span style={{ width: 7, height: 7, borderRadius: "50%", background: warnings.length > 0 ? "var(--amber)" : "var(--green)" }} />
          Warnings requiring attention
        </span>
      }
    >
      {warnings.length === 0 ? (
        <div style={{ fontSize: 12, color: "var(--ink3)", padding: "4px 0" }}>No warnings</div>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
          {warnings.map((w, i) => (
            <div key={i} style={{ display: "flex", alignItems: "baseline", gap: 10, fontSize: 12 }}>
              <Badge tone={w.tone}>{w.label}</Badge>
              <span style={{ color: "var(--ink)" }}>{w.text}</span>
              <span style={{ flex: 1 }} />
              <a
                href="#/operations"
                onClick={(e) => {
                  e.preventDefault();
                  navigate("operations");
                }}
                style={{ flex: "none", fontSize: 12, color: "var(--blue)", textDecoration: "none" }}
              >
                Inspect →
              </a>
            </div>
          ))}
        </div>
      )}
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/* Provider health matrix                                              */
/* ------------------------------------------------------------------ */

function ProviderHealthCard({ res }: { res: Resource<ProviderConnectionPage> }) {
  return (
    <Card
      title={
        <span>
          Provider health by region <span style={{ fontWeight: 400, color: "var(--ink3)" }}>· administrative status per connection</span>
        </span>
      }
    >
      <CardBody res={res}>
        {(page) => {
          const connections = page.data ?? [];
          if (connections.length === 0) {
            return <EmptyState title="No provider connections" hint="Register a Provider Connection to see the health matrix" />;
          }
          const providers = [...new Set(connections.map((c) => c.provider))].sort();
          const regions = [...new Set(connections.map((c) => c.region))].sort();
          const cell = (provider: string, region: string): ProviderConnection[] =>
            connections.filter((c) => c.provider === provider && c.region === region);
          return (
            <Table>
              <thead>
                <tr>
                  <Th>Provider</Th>
                  {regions.map((r) => (
                    <Th key={r}>
                      <span style={{ fontFamily: "var(--font-mono)" }}>{r}</span>
                    </Th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {providers.map((provider) => (
                  <tr key={provider}>
                    <Td>
                      <span style={{ fontWeight: 500 }}>{provider}</span>
                    </Td>
                    {regions.map((region) => {
                      const conns = cell(provider, region);
                      return (
                        <Td key={region}>
                          {conns.length === 0 ? (
                            <span style={{ color: "var(--ink3)" }}>—</span>
                          ) : (
                            <span style={{ display: "inline-flex", flexDirection: "column", gap: 4, alignItems: "flex-start" }}>
                              {conns.map((c) => (
                                <span key={c.id} title={`${c.display_name} (${c.id})`}>
                                  <Badge tone={statusTone(c.administrative_status)}>{c.administrative_status}</Badge>
                                </span>
                              ))}
                            </span>
                          )}
                        </Td>
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </Table>
          );
        }}
      </CardBody>
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/* Gateway region status                                               */
/* ------------------------------------------------------------------ */

function GatewayStatusCard({ res }: { res: Resource<GatewayPage> }) {
  return (
    <Card title="Gateway region status">
      <CardBody res={res}>
        {(page) => {
          const gateways = [...(page.data ?? [])].sort((a, b) => a.region.localeCompare(b.region) || a.gateway_id.localeCompare(b.gateway_id));
          if (gateways.length === 0) {
            return <EmptyState title="No gateway observations" hint="Gateways report observations to the control plane on an interval" />;
          }
          return (
            <Table>
              <thead>
                <tr>
                  <Th>Gateway</Th>
                  <Th>Region</Th>
                  <Th>Heartbeat</Th>
                  <Th>Routing revision</Th>
                  <Th>
                    <span style={{ float: "right" }}>Outbox backlog</span>
                  </Th>
                </tr>
              </thead>
              <tbody>
                {gateways.map((g) => {
                  const backlog = g.backlogs?.outbox_pending_count ?? 0;
                  return (
                    <tr key={g.gateway_id}>
                      <Td mono>{g.gateway_id}</Td>
                      <Td mono>{g.region}</Td>
                      <Td>
                        <Badge tone={statusTone(g.heartbeat_status)}>{g.heartbeat_status}</Badge>{" "}
                        <span style={{ fontFamily: "var(--font-mono)", fontSize: 10.5, color: "var(--ink3)" }}>
                          {Math.round(g.heartbeat_lag_seconds)}s
                        </span>
                      </Td>
                      <Td>
                        <span style={{ fontFamily: "var(--font-mono)", color: "var(--purple)" }}>{formatRevision(g.routing_catalog_revision)}</span>{" "}
                        {g.routing_revision_lag > 0 && <Badge tone="amber">−{g.routing_revision_lag}</Badge>}
                      </Td>
                      <Td mono>
                        <span style={{ float: "right", color: backlog > 0 ? "var(--red)" : "var(--ink2)" }}>{formatNumber(backlog)}</span>
                      </Td>
                    </tr>
                  );
                })}
              </tbody>
            </Table>
          );
        }}
      </CardBody>
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/* Routing Catalog rollout                                             */
/* ------------------------------------------------------------------ */

function PublicationCard({ res }: { res: Resource<PublicationPage> }) {
  return (
    <Card title="Routing Catalog">
      <CardBody res={res}>
        {(page) => {
          const latest: PublicationSummary | null = latestPublication(page);
          if (!latest) return <EmptyState title="No publications yet" hint="Publish a Routing Catalog draft to start a rollout" />;
          return (
            <>
              <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                <span style={{ fontFamily: "var(--font-mono)", fontSize: 13, fontWeight: 600, color: "var(--purple)" }}>
                  {formatRevision(latest.catalog_revision)}
                </span>
                <Badge tone={statusTone(latest.status)}>{latest.status.replace(/_/g, " ")}</Badge>
              </div>
              <div style={{ fontSize: 11.5, color: "var(--ink2)", margin: "6px 0 10px" }}>
                Publication <span style={{ fontFamily: "var(--font-mono)" }}>{latest.id}</span> · created {timeAgo(latest.created_at)} · regions:{" "}
                <span style={{ fontFamily: "var(--font-mono)" }}>{latest.required_regions.join(", ") || "—"}</span>
              </div>
              {(latest.receipts ?? []).length === 0 ? (
                <div style={{ fontSize: 11.5, color: "var(--ink3)" }}>No rollout receipts yet</div>
              ) : (
                <div style={{ display: "flex", flexDirection: "column", gap: 5 }}>
                  {(latest.receipts ?? []).map((r, i) => (
                    <div key={`${r.gateway_id}-${i}`} style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 11.5 }}>
                      <span style={{ fontFamily: "var(--font-mono)", width: 100, color: "var(--ink2)" }}>{r.region}</span>
                      <Badge tone={statusTone(r.status)}>{r.status}</Badge>
                      <span style={{ flex: 1 }} />
                      <span style={{ fontFamily: "var(--font-mono)", color: "var(--ink3)", fontSize: 10.5 }}>{timeAgo(r.applied_at)}</span>
                    </div>
                  ))}
                </div>
              )}
              <Button style={{ marginTop: 12, width: "100%" }} onClick={() => navigate("routing")}>
                Open Routing Catalog
              </Button>
            </>
          );
        }}
      </CardBody>
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/* Metering projection                                                 */
/* ------------------------------------------------------------------ */

function MeteringNode({ node }: { node: MeteringSummary }) {
  const rows: Array<[string, ReactNode, boolean?]> = [
    ["Projection cutoff", node.projection_cutoff ? timeAgo(node.projection_cutoff) : "—"],
    ["Heartbeat", `${node.heartbeat_status} · ${Math.round(node.heartbeat_lag_seconds)}s`],
    ["Pending events", formatNumber(node.pending_events)],
    ["Poison events", formatNumber(node.poison_events), node.poison_events > 0],
    ["Queued exports", formatNumber(node.queued_exports)],
  ];
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 7, fontSize: 12 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
        <span style={{ fontFamily: "var(--font-mono)", fontWeight: 600 }}>{node.metering_id}</span>
        <span style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--ink3)" }}>{node.region}</span>
        <Badge tone={statusTone(node.projection_status)}>{node.projection_status}</Badge>
      </div>
      {rows.map(([label, value, alert]) => (
        <div key={label} style={{ display: "flex", justifyContent: "space-between" }}>
          <span style={{ color: "var(--ink2)" }}>{label}</span>
          <span style={{ fontFamily: "var(--font-mono)", color: alert ? "var(--red)" : undefined }}>{value}</span>
        </div>
      ))}
    </div>
  );
}

function MeteringProjectionCard({ res }: { res: Resource<MeteringPage> }) {
  return (
    <Card title="Metering projection">
      <CardBody res={res}>
        {(page) => {
          const nodes = page.data ?? [];
          if (nodes.length === 0) {
            return <EmptyState title="No metering observations" hint="Metering nodes report projections to the control plane on an interval" />;
          }
          return (
            <>
              <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
                {nodes.map((n) => (
                  <MeteringNode key={n.metering_id} node={n} />
                ))}
              </div>
              <div style={{ marginTop: 10, padding: "8px 10px", borderRadius: 8, background: "var(--chip)", fontSize: 11.5, color: "var(--ink2)" }}>
                Spend figures on this page are projections and may trail the immutable Usage Ledger.
              </div>
            </>
          );
        }}
      </CardBody>
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/* Models available                                                    */
/* ------------------------------------------------------------------ */

function ModelsCard({ res }: { res: Resource<LLMModelList> }) {
  return (
    <Card title="Models available">
      <CardBody res={res}>
        {(list) => {
          const models = list.data ?? [];
          if (models.length === 0) {
            return <EmptyState title="No models available" hint="No public models are visible on the active catalog revision" />;
          }
          return (
            <>
              <div style={{ fontSize: 22, fontWeight: 700 }}>
                {formatNumber(models.length)} <span style={{ fontSize: 12, fontWeight: 500, color: "var(--ink2)" }}>public model(s)</span>
              </div>
              <div style={{ display: "flex", flexWrap: "wrap", gap: 4, marginTop: 8 }}>
                {models.map((m) => (
                  <span
                    key={m.id}
                    style={{
                      fontFamily: "var(--font-mono)",
                      fontSize: 10.5,
                      padding: "1px 7px",
                      borderRadius: "var(--radius-pill)",
                      background: "var(--purple-bg)",
                      color: "var(--purple)",
                    }}
                  >
                    {m.id}
                  </span>
                ))}
              </div>
            </>
          );
        }}
      </CardBody>
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/* Page                                                                */
/* ------------------------------------------------------------------ */

export default function OverviewPage() {
  const { tick, auto, setAuto, refresh, observedAt } = useAutoRefresh(15_000);

  const readiness = useResource(loadReadiness, [tick]);
  const usageToday = useResource(aggregateUsageToday, [tick]);
  const series = useResource(aggregateUsageSeries24h, [tick]);
  const connections = useResource(() => apiGet<ProviderConnectionPage>("/control/v1/provider-connections?limit=100"), [tick]);
  const gateways = useResource(() => apiGet<GatewayPage>("/control/v1/operations/gateways"), [tick]);
  const publications = useResource(() => apiGet<PublicationPage>("/control/v1/operations/publications"), [tick]);
  const metering = useResource(() => apiGet<MeteringPage>("/control/v1/operations/metering"), [tick]);
  const models = useResource(() => apiGet<LLMModelList>("/llm/models"), [tick]);

  const warnings = deriveWarnings(readiness.data, gateways.data, publications.data, metering.data);

  return (
    <div style={{ padding: "20px 24px", maxWidth: 1360, margin: "0 auto" }}>
      <div style={{ display: "flex", alignItems: "flex-start", gap: 16, marginBottom: 16 }}>
        <div style={{ flex: 1 }}>
          <h1 style={{ margin: 0, fontSize: 18, fontWeight: 600 }}>Overview</h1>
          <div style={{ color: "var(--ink2)", marginTop: 2, fontSize: 12.5 }}>
            Operational readiness across gateways, providers, and metering. Liveness and readiness are separate signals.
          </div>
        </div>
        {observedAt && (
          <span style={{ fontFamily: "var(--font-mono)", fontSize: 11.5, color: "var(--ink3)", paddingTop: 4 }}>
            observed {observedAt.toISOString().slice(11, 19)} UTC
          </span>
        )}
        <Button onClick={refresh}>Refresh</Button>
        <Button variant={auto ? "primary" : "ghost"} onClick={() => setAuto(!auto)} title="Reload all panels every 15 seconds">
          Auto-refresh 15s
        </Button>
      </div>

      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit,minmax(200px,1fr))", gap: 10, marginBottom: 14 }}>
        <ReadinessCard res={readiness} />
        <UsageStatCards res={usageToday} />
        <ModelsCard res={models} />
      </div>

      <div style={{ marginBottom: 14 }}>
        <WarningsCard warnings={warnings} />
      </div>

      <div style={{ display: "grid", gridTemplateColumns: "minmax(0,1.9fr) minmax(0,1fr)", gap: 14, alignItems: "start" }}>
        <div style={{ display: "flex", flexDirection: "column", gap: 14, minWidth: 0 }}>
          <UsageChartCard res={series} />
          <ProviderHealthCard res={connections} />
          <GatewayStatusCard res={gateways} />
        </div>
        <div style={{ display: "flex", flexDirection: "column", gap: 14, minWidth: 0 }}>
          <PublicationCard res={publications} />
          <MeteringProjectionCard res={metering} />
        </div>
      </div>
    </div>
  );
}
