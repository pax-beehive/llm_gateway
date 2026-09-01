/**
 * Live publication tracker. Polls GET /control/v1/routing-publications/{id}
 * every 2s until the publication reaches a terminal state
 * (active | partially_applied | failed), rendering per-region rollout receipts.
 */
import { useEffect, useRef, useState } from "react";
import { ApiError } from "../../api/client";
import { ErrorBanner } from "../../components/feedback";
import { Badge, Button, Card, CopyButton, KeyValueList, Spinner } from "../../components/ui";
import { formatDateTime, truncateId } from "../../lib/format";
import { getPublication } from "./api";
import {
  formatRevision,
  isTerminalPublication,
  publicationTone,
  type Publication,
} from "./types";
import { ReceiptsTable } from "./widgets";

const POLL_MS = 2000;

export function PublicationTracker({
  publicationId,
  onTerminal,
  onClose,
}: {
  publicationId: string;
  onTerminal?: (publication: Publication) => void;
  onClose?: () => void;
}) {
  const [publication, setPublication] = useState<Publication | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const terminalFired = useRef(false);

  useEffect(() => {
    terminalFired.current = false;
    let cancelled = false;
    let timer: number | undefined;

    const tick = async () => {
      try {
        const pub = await getPublication(publicationId);
        if (cancelled) return;
        setPublication(pub);
        setError(null);
        if (isTerminalPublication(pub.status)) {
          if (!terminalFired.current) {
            terminalFired.current = true;
            onTerminal?.(pub);
          }
          return; // stop polling
        }
        timer = window.setTimeout(tick, POLL_MS);
      } catch (err) {
        if (cancelled) return;
        setError(err instanceof ApiError ? err : new ApiError(0, "network", String(err)));
        timer = window.setTimeout(tick, POLL_MS);
      }
    };
    void tick();
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
    };
  }, [publicationId, onTerminal]);

  const polling = publication !== null && !isTerminalPublication(publication.status);

  return (
    <Card
      title={
        <span style={{ display: "inline-flex", alignItems: "center", gap: 8 }}>
          Publication tracker
          {polling && <Spinner size={12} />}
        </span>
      }
      actions={onClose && <Button onClick={onClose}>Dismiss</Button>}
    >
      {error && !publication && <ErrorBanner error={error} />}
      {publication && (
        <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
          <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
            <span style={{ fontFamily: "var(--font-mono)", fontWeight: 600, color: "var(--purple)" }}>
              {publication.id}
            </span>
            <CopyButton text={publication.id} label="Copy id" />
            <Badge tone={publicationTone(publication.status)}>{publication.status.replace(/_/g, " ")}</Badge>
            <span style={{ fontSize: 11.5, color: "var(--ink2)" }}>
              publishes <b style={{ fontFamily: "var(--font-mono)" }}>{formatRevision(publication.catalog_revision)}</b>
            </span>
          </div>
          <KeyValueList
            items={[
              { key: "Validation hash", value: truncateId(publication.validation_hash, 12, 8), mono: true },
              {
                key: "Required regions",
                value: publication.required_regions.length > 0 ? publication.required_regions.join(", ") : "—",
                mono: true,
              },
              { key: "Created", value: `${formatDateTime(publication.created_at)} · ${publication.created_by}` },
              { key: "Updated", value: formatDateTime(publication.updated_at) },
            ]}
          />
          {polling && (
            <div style={{ fontSize: 11.5, color: "var(--ink3)" }}>
              Acceptance of the publish request does not mean every gateway applied the revision — watching per-region
              receipts until the rollout reaches a terminal state.
            </div>
          )}
          {error && <ErrorBanner error={error} />}
          <ReceiptsTable receipts={publication.receipts ?? []} />
        </div>
      )}
      {!publication && !error && (
        <div style={{ display: "flex", justifyContent: "center", padding: 16 }}>
          <Spinner />
        </div>
      )}
    </Card>
  );
}
