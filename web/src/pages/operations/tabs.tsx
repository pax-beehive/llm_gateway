import { useEffect, useState, type ReactNode } from "react";
import { apiGet, ApiError } from "../../api/client";
import { Drawer, ErrorBanner, Loading } from "../../components/feedback";
import { Badge, Card, EmptyState, KeyValueList, StatCard, Table, Td, Th } from "../../components/ui";
import { formatDateTime, formatNumber, timeAgo, truncateId } from "../../lib/format";
import { useOps } from "./hooks";
import {
  formatSeconds,
  heartbeatTone,
  jobTone,
  publicationTone,
  receiptTone,
  type AccessRolloutReceipt,
  type ConsumerObservation,
  type GatewaySummary,
  type JobSummary,
  type MeteringSummary,
  type OutboxStatus,
  type PublicationSummary,
  type RolloutReceipt,
  type Tone,
} from "./types";

/* ------------------------------------------------------------------ */
/* Shared pieces                                                       */
/* ------------------------------------------------------------------ */

/** Fetches a detail resource when a row is selected and renders it in a drawer. */
function DetailDrawer<T>({
  id,
  path,
  title,
  onClose,
  render,
}: {
  id: string | null;
  path: (id: string) => string;
  title: string;
  onClose: () => void;
  render: (detail: T) => ReactNode;
}) {
  const [detail, setDetail] = useState<T | null>(null);
  const [error, setError] = useState<ApiError | null>(null);

  useEffect(() => {
    if (id === null) return;
    const controller = new AbortController();
    setDetail(null);
    setError(null);
    apiGet<T>(path(id), { signal: controller.signal })
      .then((value) => {
        if (!controller.signal.aborted) setDetail(value);
      })
      .catch((err: unknown) => {
        if (!controller.signal.aborted) {
          setError(err instanceof ApiError ? err : new ApiError(0, "network", String(err)));
        }
      });
    return () => controller.abort();
  }, [id, path]);

  return (
    <Drawer open={id !== null} onClose={onClose} title={title} width={560}>
      {error && <ErrorBanner error={error} />}
      {!error && detail === null && <Loading />}
      {detail !== null && render(detail)}
    </Drawer>
  );
}

function LagBadge({ lag, warnAt }: { lag: number; warnAt: number }) {
  const tone: Tone = lag <= 0 ? "green" : lag >= warnAt ? "red" : "amber";
  return <Badge tone={tone}>{lag <= 0 ? "caught up" : `lag ${formatNumber(lag)}`}</Badge>;
}

function Hint({ children }: { children: ReactNode }) {
  return <div style={{ fontSize: 11, color: "var(--ink3)" }}>{children}</div>;
}

export interface TabProps {
  tick: number;
  onUpdated?: (at: Date) => void;
}

/* ------------------------------------------------------------------ */
/* Gateways                                                            */
/* ------------------------------------------------------------------ */

export function GatewaysTab({ tick, onUpdated }: TabProps) {
  const { data, error, loading } = useOps<{ data: GatewaySummary[] }>("/control/v1/operations/gateways", tick, onUpdated);
  const [selected, setSelected] = useState<string | null>(null);

  if (loading) return <Loading />;
  if (error) return <ErrorBanner error={error} />;
  const gateways = data?.data ?? [];
  if (gateways.length === 0) {
    return <EmptyState title="No gateways reporting" hint="Gateway heartbeats appear here once processes report in" />;
  }

  return (
    <>
      <Card style={{ padding: 0, overflow: "hidden" }}>
        <Table>
          <thead>
            <tr>
              <Th>Gateway</Th>
              <Th>Region</Th>
              <Th>Heartbeat</Th>
              <Th>Routing revision</Th>
              <Th>Access projection</Th>
              <Th>Backlog</Th>
              <Th>Observed</Th>
            </tr>
          </thead>
          <tbody>
            {gateways.map((g) => (
              <tr key={g.gateway_id} onClick={() => setSelected(g.gateway_id)} style={{ cursor: "pointer" }}>
                <Td mono>{truncateId(g.gateway_id, 10, 6)}</Td>
                <Td mono>{g.region}</Td>
                <Td>
                  <Badge tone={heartbeatTone(g.heartbeat_status)}>
                    {g.heartbeat_status} · {formatSeconds(g.heartbeat_lag_seconds)}
                  </Badge>
                </Td>
                <Td>
                  <span style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--purple)" }}>
                    r{g.routing_catalog_revision}
                  </span>{" "}
                  <LagBadge lag={g.routing_revision_lag} warnAt={100} />
                </Td>
                <Td>
                  <span style={{ fontFamily: "var(--font-mono)", fontSize: 11 }}>r{g.access_projection_revision}</span>{" "}
                  <LagBadge lag={g.access_revision_lag} warnAt={100} />
                </Td>
                <Td mono>{formatNumber(g.backlogs.outbox_pending_count)}</Td>
                <Td mono>{timeAgo(g.observed_at)}</Td>
              </tr>
            ))}
          </tbody>
        </Table>
      </Card>
      <Hint>Heartbeat lag indicates liveness; readiness is the process /readyz.</Hint>

      <DetailDrawer<GatewaySummary>
        id={selected}
        path={(id) => `/control/v1/operations/gateways/${encodeURIComponent(id)}`}
        title="Gateway detail"
        onClose={() => setSelected(null)}
        render={(g) => (
          <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
            <KeyValueList
              items={[
                { key: "Gateway ID", value: g.gateway_id, mono: true },
                { key: "Region", value: g.region, mono: true },
                { key: "Build", value: g.build_sha || "—", mono: true },
                { key: "Schema", value: `db v${g.database_schema_version} · obs v${g.event_schema_version}`, mono: true },
                {
                  key: "Heartbeat",
                  value: `${g.heartbeat_status} · ${formatSeconds(g.heartbeat_lag_seconds)} ago`,
                  mono: true,
                },
                {
                  key: "Routing revision",
                  value: `r${g.routing_catalog_revision} (desired r${g.desired_routing_revision}, lag ${g.routing_revision_lag})`,
                  mono: true,
                },
                {
                  key: "Access projection",
                  value: `r${g.access_projection_revision} (desired r${g.desired_access_revision}, lag ${g.access_revision_lag})`,
                  mono: true,
                },
                { key: "Execution epoch floor", value: String(g.execution_epoch_floor), mono: true },
                { key: "Last usage outbox ID", value: formatNumber(g.last_usage_outbox_id), mono: true },
                { key: "Started", value: formatDateTime(g.started_at), mono: true },
                { key: "Observed", value: formatDateTime(g.observed_at), mono: true },
                {
                  key: "Metering projection",
                  value: `${g.backlogs.metering_projection_status}${
                    g.backlogs.metering_projection_cutoff ? ` · cutoff ${timeAgo(g.backlogs.metering_projection_cutoff)}` : ""
                  }`,
                  mono: true,
                },
                { key: "Outbox pending", value: formatNumber(g.backlogs.outbox_pending_count), mono: true },
                { key: "Quota reconciliation backlog", value: formatNumber(g.backlogs.quota_reconciliation_backlog), mono: true },
                { key: "Cache refresh due", value: formatNumber(g.backlogs.cache_refresh_due_backlog), mono: true },
                { key: "Retention scrub backlog", value: formatNumber(g.backlogs.retention_scrub_backlog), mono: true },
                {
                  key: "Key revocation propagation",
                  value: formatSeconds(g.backlogs.key_revocation_propagation_seconds),
                  mono: true,
                },
              ]}
            />
            <ConsumersTable consumers={g.consumers} />
            <RoutingReceipts receipts={g.routing_receipts ?? []} />
            <AccessReceipts receipts={g.access_receipts ?? []} />
          </div>
        )}
      />
    </>
  );
}

function ConsumersTable({ consumers }: { consumers: ConsumerObservation[] }) {
  return (
    <section>
      <h3 style={{ fontSize: 12, fontWeight: 600, margin: "0 0 8px" }}>Consumers</h3>
      {consumers.length === 0 ? (
        <Hint>No consumer observations reported.</Hint>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Name</Th>
              <Th>Lag</Th>
              <Th>Pending</Th>
              <Th>Last succeeded</Th>
              <Th>Error</Th>
            </tr>
          </thead>
          <tbody>
            {consumers.map((c) => (
              <tr key={c.name}>
                <Td mono>{c.name}</Td>
                <Td mono>{formatSeconds(c.lag_seconds)}</Td>
                <Td mono>{formatNumber(c.pending_count)}</Td>
                <Td mono>{c.last_succeeded_at ? timeAgo(c.last_succeeded_at) : "—"}</Td>
                <Td mono>
                  {c.error_code ? <span style={{ color: "var(--red)" }}>{c.error_code}</span> : "—"}
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
    </section>
  );
}

function RoutingReceipts({ receipts }: { receipts: RolloutReceipt[] }) {
  return (
    <section>
      <h3 style={{ fontSize: 12, fontWeight: 600, margin: "0 0 8px" }}>Routing receipts</h3>
      {receipts.length === 0 ? (
        <Hint>No routing receipts reported.</Hint>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Publication</Th>
              <Th>Revision</Th>
              <Th>Status</Th>
              <Th>Applied</Th>
              <Th>Lag</Th>
            </tr>
          </thead>
          <tbody>
            {receipts.map((r) => (
              <tr key={`${r.publication_id}:${r.catalog_revision}`}>
                <Td mono>{truncateId(r.publication_id)}</Td>
                <Td mono>r{r.catalog_revision}</Td>
                <Td>
                  <Badge tone={receiptTone(r.status)}>{r.status}</Badge>
                  {r.error_code && (
                    <span style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--red)", marginLeft: 6 }}>
                      {r.error_code}
                    </span>
                  )}
                </Td>
                <Td mono>{timeAgo(r.applied_at)}</Td>
                <Td mono>{formatNumber(r.lag_milliseconds)}ms</Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
    </section>
  );
}

function AccessReceipts({ receipts }: { receipts: AccessRolloutReceipt[] }) {
  return (
    <section>
      <h3 style={{ fontSize: 12, fontWeight: 600, margin: "0 0 8px" }}>Access receipts</h3>
      {receipts.length === 0 ? (
        <Hint>No access receipts reported.</Hint>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Aggregate</Th>
              <Th>Revision</Th>
              <Th>Status</Th>
              <Th>Applied</Th>
              <Th>Lag</Th>
            </tr>
          </thead>
          <tbody>
            {receipts.map((r) => (
              <tr key={`${r.event_id}`}>
                <Td mono>
                  {r.aggregate_type}/{truncateId(r.aggregate_id)}
                </Td>
                <Td mono>r{r.aggregate_revision}</Td>
                <Td>
                  <Badge tone={receiptTone(r.status)}>{r.status}</Badge>
                  {r.error_code && (
                    <span style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--red)", marginLeft: 6 }}>
                      {r.error_code}
                    </span>
                  )}
                </Td>
                <Td mono>{timeAgo(r.applied_at)}</Td>
                <Td mono>{formatNumber(r.lag_milliseconds)}ms</Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
    </section>
  );
}

/* ------------------------------------------------------------------ */
/* Metering                                                            */
/* ------------------------------------------------------------------ */

export function MeteringTab({ tick, onUpdated }: TabProps) {
  const { data, error, loading } = useOps<{ data: MeteringSummary[] }>("/control/v1/operations/metering", tick, onUpdated);
  const [selected, setSelected] = useState<string | null>(null);

  if (loading) return <Loading />;
  if (error) return <ErrorBanner error={error} />;
  const rows = data?.data ?? [];
  if (rows.length === 0) {
    return <EmptyState title="No metering processes reporting" hint="Metering heartbeats appear here once processes report in" />;
  }

  return (
    <>
      <Card style={{ padding: 0, overflow: "hidden" }}>
        <Table>
          <thead>
            <tr>
              <Th>Metering</Th>
              <Th>Region</Th>
              <Th>Generation</Th>
              <Th>Cutoff</Th>
              <Th>Pending</Th>
              <Th>Poison</Th>
              <Th>Queued exports</Th>
              <Th>Heartbeat</Th>
              <Th>Projection</Th>
            </tr>
          </thead>
          <tbody>
            {rows.map((m) => (
              <tr key={m.metering_id} onClick={() => setSelected(m.metering_id)} style={{ cursor: "pointer" }}>
                <Td mono>{truncateId(m.metering_id, 10, 6)}</Td>
                <Td mono>{m.region}</Td>
                <Td mono>{m.projection_generation}</Td>
                <Td mono>{timeAgo(m.projection_cutoff)}</Td>
                <Td mono>
                  {formatNumber(m.pending_events)}
                  {m.pending_events > 0 && m.oldest_pending_at && (
                    <span style={{ color: "var(--ink3)" }}> · oldest {formatSeconds(m.oldest_pending_age_seconds)}</span>
                  )}
                </Td>
                <Td mono>
                  {m.poison_events > 0 ? (
                    <span style={{ color: "var(--red)", fontWeight: 600 }}>{formatNumber(m.poison_events)}</span>
                  ) : (
                    "0"
                  )}
                </Td>
                <Td mono>{formatNumber(m.queued_exports)}</Td>
                <Td>
                  <Badge tone={heartbeatTone(m.heartbeat_status)}>
                    {m.heartbeat_status} · {formatSeconds(m.heartbeat_lag_seconds)}
                  </Badge>
                </Td>
                <Td>
                  <Badge tone={m.projection_status === "current" ? "green" : "amber"}>{m.projection_status}</Badge>
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      </Card>
      <Hint>Heartbeat lag indicates liveness; readiness is the process /readyz.</Hint>

      <DetailDrawer<MeteringSummary>
        id={selected}
        path={(id) => `/control/v1/operations/metering/${encodeURIComponent(id)}`}
        title="Metering detail"
        onClose={() => setSelected(null)}
        render={(m) => (
          <KeyValueList
            items={[
              { key: "Metering ID", value: m.metering_id, mono: true },
              { key: "Region", value: m.region, mono: true },
              { key: "Projection generation", value: String(m.projection_generation), mono: true },
              { key: "Projection cutoff", value: `${formatDateTime(m.projection_cutoff)} (${timeAgo(m.projection_cutoff)})`, mono: true },
              { key: "Projection status", value: m.projection_status, mono: true },
              { key: "Pending events", value: formatNumber(m.pending_events), mono: true },
              {
                key: "Oldest pending",
                value: m.oldest_pending_at ? `${formatDateTime(m.oldest_pending_at)} (${formatSeconds(m.oldest_pending_age_seconds)})` : "—",
                mono: true,
              },
              { key: "Poison events", value: formatNumber(m.poison_events), mono: true },
              { key: "Queued exports", value: formatNumber(m.queued_exports), mono: true },
              { key: "Heartbeat", value: `${m.heartbeat_status} · ${formatSeconds(m.heartbeat_lag_seconds)} ago`, mono: true },
              { key: "Started", value: formatDateTime(m.started_at), mono: true },
              { key: "Observed", value: formatDateTime(m.observed_at), mono: true },
            ]}
          />
        )}
      />
    </>
  );
}

/* ------------------------------------------------------------------ */
/* Publications                                                        */
/* ------------------------------------------------------------------ */

export function PublicationsTab({ tick, onUpdated }: TabProps) {
  const { data, error, loading } = useOps<{ data: PublicationSummary[] }>("/control/v1/operations/publications", tick, onUpdated);
  const [selected, setSelected] = useState<string | null>(null);

  if (loading) return <Loading />;
  if (error) return <ErrorBanner error={error} />;
  const rows = data?.data ?? [];
  if (rows.length === 0) {
    return <EmptyState title="No publications" hint="Publish a routing catalog revision to see rollout status here" />;
  }

  return (
    <>
      <Card style={{ padding: 0, overflow: "hidden" }}>
        <Table>
          <thead>
            <tr>
              <Th>Publication</Th>
              <Th>Catalog revision</Th>
              <Th>Status</Th>
              <Th>Required regions</Th>
              <Th>Updated</Th>
            </tr>
          </thead>
          <tbody>
            {rows.map((p) => (
              <tr key={p.id} onClick={() => setSelected(p.id)} style={{ cursor: "pointer" }}>
                <Td mono>
                  <span style={{ color: "var(--purple)" }}>{truncateId(p.id)}</span>
                </Td>
                <Td mono>r{p.catalog_revision}</Td>
                <Td>
                  <Badge tone={publicationTone(p.status)}>{p.status}</Badge>
                </Td>
                <Td mono>{p.required_regions.join(", ")}</Td>
                <Td mono>{timeAgo(p.updated_at)}</Td>
              </tr>
            ))}
          </tbody>
        </Table>
      </Card>
      <Hint>Enqueue is durable; application is confirmed only by per-region receipts.</Hint>

      <DetailDrawer<PublicationSummary>
        id={selected}
        path={(id) => `/control/v1/operations/publications/${encodeURIComponent(id)}`}
        title="Publication detail"
        onClose={() => setSelected(null)}
        render={(p) => (
          <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
            <KeyValueList
              items={[
                { key: "Publication ID", value: p.id, mono: true },
                { key: "Catalog revision", value: `r${p.catalog_revision}`, mono: true },
                { key: "Status", value: p.status, mono: true },
                { key: "Required regions", value: p.required_regions.join(", "), mono: true },
                { key: "Created", value: formatDateTime(p.created_at), mono: true },
                { key: "Updated", value: formatDateTime(p.updated_at), mono: true },
              ]}
            />
            <section>
              <h3 style={{ fontSize: 12, fontWeight: 600, margin: "0 0 8px" }}>Receipts</h3>
              {(p.receipts ?? []).length === 0 ? (
                <Hint>No receipts yet — gateways confirm application asynchronously.</Hint>
              ) : (
                <Table>
                  <thead>
                    <tr>
                      <Th>Gateway</Th>
                      <Th>Region</Th>
                      <Th>Status</Th>
                      <Th>Applied</Th>
                      <Th>Lag</Th>
                    </tr>
                  </thead>
                  <tbody>
                    {(p.receipts ?? []).map((r) => (
                      <tr key={r.gateway_id}>
                        <Td mono>{truncateId(r.gateway_id, 10, 6)}</Td>
                        <Td mono>{r.region}</Td>
                        <Td>
                          <Badge tone={receiptTone(r.status)}>{r.status}</Badge>
                          {r.error_code && (
                            <span style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--red)", marginLeft: 6 }}>
                              {r.error_code}
                            </span>
                          )}
                        </Td>
                        <Td mono>{timeAgo(r.applied_at)}</Td>
                        <Td mono>{formatNumber(r.lag_milliseconds)}ms</Td>
                      </tr>
                    ))}
                  </tbody>
                </Table>
              )}
            </section>
          </div>
        )}
      />
    </>
  );
}

/* ------------------------------------------------------------------ */
/* Outbox                                                              */
/* ------------------------------------------------------------------ */

export function OutboxTab({ tick, onUpdated }: TabProps) {
  const { data, error, loading } = useOps<OutboxStatus>("/control/v1/operations/outbox", tick, onUpdated);

  if (loading) return <Loading />;
  if (error) return <ErrorBanner error={error} />;
  if (!data) return <EmptyState title="Outbox status unavailable" />;

  return (
    <>
      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit,minmax(200px,1fr))", gap: 10, maxWidth: 1000 }}>
        <StatCard
          label="Pending events"
          value={formatNumber(data.outbox_pending_count)}
          sub={data.outbox_pending_count > 0 ? "awaiting publication" : "relay is caught up"}
        />
        <StatCard
          label="Oldest unpublished age"
          value={formatSeconds(data.oldest_unpublished_outbox_age_seconds)}
          sub="time since the oldest pending event occurred"
        />
        <StatCard
          label="Oldest occurred at"
          value={data.oldest_occurred_at ? formatDateTime(data.oldest_occurred_at) : "—"}
          sub={data.oldest_occurred_at ? timeAgo(data.oldest_occurred_at) : "no pending events"}
        />
      </div>
      <Hint>Control-event relay: outbox → regional consumers.</Hint>
    </>
  );
}

/* ------------------------------------------------------------------ */
/* Consumers                                                           */
/* ------------------------------------------------------------------ */

export function ConsumersTab({ tick, onUpdated }: TabProps) {
  const { data, error, loading } = useOps<{ data: GatewaySummary[] }>("/control/v1/operations/consumers", tick, onUpdated);

  if (loading) return <Loading />;
  if (error) return <ErrorBanner error={error} />;
  const gateways = data?.data ?? [];
  const rows = gateways.flatMap((g) => g.consumers.map((consumer) => ({ gateway: g, consumer })));
  if (rows.length === 0) {
    return <EmptyState title="No consumer observations" hint="Consumer group summaries appear here once gateways report in" />;
  }

  const consumerTone = (c: ConsumerObservation): Tone =>
    c.error_code ? "red" : c.pending_count > 0 || c.lag_seconds > 60 ? "amber" : "green";
  const consumerStatus = (c: ConsumerObservation): string =>
    c.error_code ? "error" : c.pending_count > 0 || c.lag_seconds > 60 ? "lagging" : "current";

  return (
    <>
      <Card style={{ padding: 0, overflow: "hidden" }}>
        <Table>
          <thead>
            <tr>
              <Th>Consumer</Th>
              <Th>Gateway</Th>
              <Th>Region</Th>
              <Th>Status</Th>
              <Th>Lag</Th>
              <Th>Pending</Th>
              <Th>Last succeeded</Th>
              <Th>Stable error</Th>
            </tr>
          </thead>
          <tbody>
            {rows.map(({ gateway, consumer }) => (
              <tr key={`${gateway.gateway_id}:${consumer.name}`}>
                <Td mono>{consumer.name}</Td>
                <Td mono>{truncateId(gateway.gateway_id, 10, 6)}</Td>
                <Td mono>{gateway.region}</Td>
                <Td>
                  <Badge tone={consumerTone(consumer)}>{consumerStatus(consumer)}</Badge>
                </Td>
                <Td mono>{formatSeconds(consumer.lag_seconds)}</Td>
                <Td mono>{formatNumber(consumer.pending_count)}</Td>
                <Td mono>{consumer.last_succeeded_at ? timeAgo(consumer.last_succeeded_at) : "—"}</Td>
                <Td mono>
                  {consumer.error_code ? <span style={{ color: "var(--red)" }}>{consumer.error_code}</span> : "—"}
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      </Card>
      <Hint>Consumer lag/pending is reported per gateway; an error code marks the last stable failure.</Hint>
    </>
  );
}

/* ------------------------------------------------------------------ */
/* Jobs                                                                */
/* ------------------------------------------------------------------ */

export function JobsTab({ tick, onUpdated }: TabProps) {
  const { data, error, loading } = useOps<{ data: JobSummary[] }>("/control/v1/operations/jobs", tick, onUpdated);
  const [selected, setSelected] = useState<string | null>(null);

  if (loading) return <Loading />;
  if (error) return <ErrorBanner error={error} />;
  const rows = data?.data ?? [];
  if (rows.length === 0) {
    return <EmptyState title="No jobs" hint="Background jobs (rebuilds, backfills, repairs) appear here" />;
  }

  return (
    <>
      <Card style={{ padding: 0, overflow: "hidden" }}>
        <Table>
          <thead>
            <tr>
              <Th>Job</Th>
              <Th>Kind</Th>
              <Th>Requested by</Th>
              <Th>Tenant</Th>
              <Th>Status</Th>
              <Th>Progress</Th>
              <Th>Created</Th>
              <Th>Finished</Th>
            </tr>
          </thead>
          <tbody>
            {rows.map((j) => (
              <tr key={j.id} onClick={() => setSelected(j.id)} style={{ cursor: "pointer" }}>
                <Td mono>
                  <span style={{ color: "var(--purple)" }}>{truncateId(j.id)}</span>
                </Td>
                <Td mono>{j.kind}</Td>
                <Td mono>{j.requested_by}</Td>
                <Td mono>{j.tenant_id ? truncateId(j.tenant_id) : "—"}</Td>
                <Td>
                  <Badge tone={jobTone(j.status)}>{j.status}</Badge>
                </Td>
                <Td>
                  <span style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
                    <span style={{ width: 60, height: 6, borderRadius: 99, background: "var(--chip)", overflow: "hidden" }}>
                      <span
                        style={{
                          display: "block",
                          height: "100%",
                          width: `${Math.min(100, Math.max(0, j.progress))}%`,
                          background: j.status === "failed" ? "var(--red)" : "var(--blue)",
                        }}
                      />
                    </span>
                    <span style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--ink2)" }}>{j.progress}%</span>
                  </span>
                </Td>
                <Td mono>{timeAgo(j.created_at)}</Td>
                <Td mono>{j.finished_at ? timeAgo(j.finished_at) : "—"}</Td>
              </tr>
            ))}
          </tbody>
        </Table>
      </Card>
      <Hint>Jobs are read-only here — no retry actions exist on this API.</Hint>

      <DetailDrawer<JobSummary>
        id={selected}
        path={(id) => `/control/v1/operations/jobs/${encodeURIComponent(id)}`}
        title="Job detail"
        onClose={() => setSelected(null)}
        render={(j) => (
          <KeyValueList
            items={[
              { key: "Job ID", value: j.id, mono: true },
              { key: "Kind", value: j.kind, mono: true },
              { key: "Requested by", value: j.requested_by, mono: true },
              { key: "Tenant", value: j.tenant_id ?? "—", mono: true },
              { key: "Status", value: j.status, mono: true },
              { key: "Progress", value: `${j.progress}%`, mono: true },
              { key: "Result reference", value: j.result_ref ?? "—", mono: true },
              { key: "Error code", value: j.error_code ?? "—", mono: true },
              { key: "Created", value: formatDateTime(j.created_at), mono: true },
              { key: "Started", value: j.started_at ? formatDateTime(j.started_at) : "—", mono: true },
              { key: "Finished", value: j.finished_at ? formatDateTime(j.finished_at) : "—", mono: true },
            ]}
          />
        )}
      />
    </>
  );
}
