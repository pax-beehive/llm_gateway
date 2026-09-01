import { useCallback, useEffect, useState } from "react";
import { apiGet, ApiError } from "../../api/client";
import { useAuth } from "../../auth/AuthProvider";
import { PERMISSIONS } from "../../auth/permissions";
import { ConfirmCorrection } from "./correction";
import { Drawer, ErrorBanner, useToast } from "../../components/feedback";
import { Badge, Button, Card, CodeBlock, EmptyState, Spinner, Table, Td, Th } from "../../components/ui";
import { formatMicrosUSD, formatNumber, timeAgo, truncateId } from "../../lib/format";
import { outcomeTone, type EventPage, type UsageEvent } from "./types";

const PAGE_SIZE = 50;

/**
 * Immutable usage event ledger with cursor pagination. Rows open a drawer with
 * the full event payload and the append-only correction form.
 */
export function EventLedger({ query, refreshKey }: { query: string; refreshKey: number }) {
  const toast = useToast();
  const [events, setEvents] = useState<UsageEvent[]>([]);
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [cutoff, setCutoff] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [selected, setSelected] = useState<UsageEvent | null>(null);
  const [correcting, setCorrecting] = useState<UsageEvent | null>(null);
  const { can } = useAuth();
  const canWrite = can(PERMISSIONS.meteringWrite);

  const load = useCallback(
    (after: string | undefined, append: boolean) => {
      const params = new URLSearchParams(query);
      params.set("limit", String(PAGE_SIZE));
      if (after) params.set("cursor", after);
      const setBusy = append ? setLoadingMore : setLoading;
      setBusy(true);
      setError(null);
      apiGet<EventPage>(`/metering/v1/usage/events?${params.toString()}`)
        .then((page) => {
          setEvents((current) => (append ? [...current, ...(page.data ?? [])] : (page.data ?? [])));
          setCursor(page.next_cursor || undefined);
          setCutoff(page.data_cutoff);
        })
        .catch((err: unknown) => {
          setError(err instanceof ApiError ? err : new ApiError(0, "network", String(err)));
        })
        .finally(() => setBusy(false));
    },
    [query, refreshKey],
  );

  useEffect(() => {
    setEvents([]);
    setCursor(undefined);
    load(undefined, false);
  }, [load]);

  return (
    <Card
      title={
        <>
          Usage events <span style={{ fontWeight: 400, color: "var(--ink3)" }}>· immutable ledger entries</span>
        </>
      }
      actions={<Badge tone="purple">immutable</Badge>}
    >
      {error && <ErrorBanner error={error} retry={() => load(undefined, false)} />}
      {!error && loading && (
        <div style={{ display: "flex", justifyContent: "center", padding: 32 }}>
          <Spinner />
        </div>
      )}
      {!error && !loading && events.length === 0 && (
        <EmptyState title="No usage events" hint="No ledger entries match the current filters" />
      )}
      {events.length > 0 && (
        <>
          <Table>
            <thead>
              <tr>
                <Th>Time</Th>
                <Th>Event</Th>
                <Th>Type</Th>
                <Th>Model</Th>
                <Th>Provider</Th>
                <Th>Tokens (in/out)</Th>
                <Th>Cost</Th>
                <Th>Outcome</Th>
              </tr>
            </thead>
            <tbody>
              {events.map((event) => (
                <tr
                  key={event.event_id}
                  onClick={() => setSelected(event)}
                  style={{ cursor: "pointer" }}
                >
                  <Td mono>{timeAgo(event.occurred_at)}</Td>
                  <Td mono>
                    <span style={{ color: "var(--purple)" }}>{truncateId(event.event_id)}</span>
                  </Td>
                  <Td>
                    <Badge tone={event.type === "UsageCorrected" ? "purple" : "neutral"}>{event.type}</Badge>
                  </Td>
                  <Td mono>{event.public_model}</Td>
                  <Td>{event.provider}</Td>
                  <Td mono>
                    {formatNumber(event.input_tokens)} / {formatNumber(event.output_tokens)}
                  </Td>
                  <Td mono>{formatMicrosUSD(event.amount_micros)}</Td>
                  <Td>
                    <Badge tone={outcomeTone(event.outcome)}>{event.outcome}</Badge>
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
          <div
            style={{
              display: "flex",
              alignItems: "center",
              gap: 12,
              paddingTop: 10,
              fontSize: 11,
              color: "var(--ink3)",
            }}
          >
            {cursor ? (
              <Button disabled={loadingMore} onClick={() => load(cursor, true)}>
                {loadingMore ? "Loading…" : "Load more"}
              </Button>
            ) : (
              <span>End of ledger for these filters</span>
            )}
            <span style={{ flex: 1 }} />
            {cutoff && <span>projection cutoff {timeAgo(cutoff)}</span>}
          </div>
        </>
      )}

      <Drawer open={selected !== null} onClose={() => setSelected(null)} title="Usage event" width={520}>
        {selected && (
          <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
            <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
              <Badge tone={outcomeTone(selected.outcome)}>{selected.outcome}</Badge>
              <Badge tone="neutral">{selected.type}</Badge>
              <span style={{ flex: 1 }} />
              <Button
                variant="primary"
                disabled={!canWrite}
                title={canWrite ? undefined : "Requires platform:metering:write"}
                onClick={() => {
                  setCorrecting(selected);
                }}
              >
                Append correction
              </Button>
            </div>
            <CodeBlock code={JSON.stringify(selected, null, 2)} lang="json" />
            <div style={{ fontSize: 11, color: "var(--ink3)" }}>
              Ledger entries are immutable. Corrections append a new compensating event; the original is never
              rewritten.
            </div>
          </div>
        )}
      </Drawer>

      <ConfirmCorrection
        event={correcting}
        onClose={() => setCorrecting(null)}
        onDone={(created) => {
          setCorrecting(null);
          setSelected(null);
          toast(`Correction ${truncateId(created.event_id)} appended`, "success");
          load(undefined, false);
        }}
      />
    </Card>
  );
}
