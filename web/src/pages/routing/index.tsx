/**
 * Routing Catalog page — versioned routing published to gateways.
 *
 * Real integration against the Control Plane via the BFF
 * (/api/control/v1/routing-catalog + /routing-publications). Layout follows
 * the fe1 mock: active revision header, drafts with visual/JSON editor,
 * immutable revision history with Restore, and a live publication tracker.
 */
import { useState } from "react";
import { useApi } from "../../api/useApi";
import { useAuth } from "../../auth/AuthProvider";
import { PERMISSIONS } from "../../auth/permissions";
import { ErrorBanner, Loading } from "../../components/feedback";
import { Badge, Button, Card, CopyButton, EmptyState, Tabs } from "../../components/ui";
import { formatDateTime, truncateId } from "../../lib/format";
import { DraftsSection } from "./drafts";
import { PublicationTracker } from "./publication";
import { RevisionsSection } from "./revisions";
import { RoutesTable } from "./routes";
import { formatRevision, type Revision } from "./types";

const TABS = ["Active routes", "Drafts", "Revisions"];

export default function RoutingPage() {
  const { data: head, error, loading, retry } = useApi<Revision>("/control/v1/routing-catalog");
  const { can } = useAuth();
  const canWrite = can(PERMISSIONS.routingWrite);
  const [tab, setTab] = useState(TABS[0]);
  const [trackedPubId, setTrackedPubId] = useState<string | null>(null);
  const [inspectId, setInspectId] = useState("");
  const [createSignal, setCreateSignal] = useState(0);
  const [revisionsSignal, setRevisionsSignal] = useState(0);

  // 404 not_found means no catalog has ever been published — an empty state,
  // not an error surface.
  const noCatalog = error !== null && error.status === 404;

  const afterPublicationAccepted = (publicationId: string) => {
    setTrackedPubId(publicationId);
    retry(); // head moves as soon as the publication is accepted
    setRevisionsSignal((n) => n + 1);
  };

  return (
    <div style={{ padding: "20px 24px", maxWidth: 1360, margin: "0 auto" }}>
      {/* ---- page header ---- */}
      <div style={{ display: "flex", alignItems: "flex-start", gap: 16, marginBottom: 14, flexWrap: "wrap" }}>
        <div style={{ flex: 1, minWidth: 260 }}>
          <h1 style={{ margin: 0, fontSize: 18, fontWeight: 600 }}>Routing Catalog</h1>
          <div style={{ color: "var(--ink2)", marginTop: 2, fontSize: 13 }}>
            Routing is versioned and published — never edited in place. A publication is durable on acceptance and
            confirmed per region by receipt.
          </div>
        </div>
        <div style={{ display: "flex", gap: 8, alignItems: "center", flexWrap: "wrap" }}>
          <input
            placeholder="Inspect publication rpub_…"
            value={inspectId}
            onChange={(e) => setInspectId(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && inspectId.trim()) {
                setTrackedPubId(inspectId.trim());
                setInspectId("");
              }
            }}
            style={{
              padding: "6px 10px",
              borderRadius: "var(--radius)",
              border: "1px solid var(--line)",
              background: "var(--panel)",
              color: "var(--ink)",
              fontFamily: "var(--font-mono)",
              fontSize: 12,
              width: 200,
            }}
          />
          <Button
            disabled={!inspectId.trim()}
            onClick={() => {
              setTrackedPubId(inspectId.trim());
              setInspectId("");
            }}
          >
            Inspect
          </Button>
          <Button
            variant="primary"
            disabled={!canWrite || !head}
            title={!canWrite ? "Requires platform:routing:write" : head ? undefined : "No published revision to base a draft on"}
            onClick={() => {
              setTab("Drafts");
              setCreateSignal((n) => n + 1);
            }}
          >
            Create draft
          </Button>
        </div>
      </div>

      {loading && <Loading />}
      {!loading && error && !noCatalog && <ErrorBanner error={error} retry={retry} />}

      {!loading && (head || noCatalog) && (
        <>
          {/* ---- active revision header card ---- */}
          {head ? (
            <Card title="Active published revision" style={{ marginBottom: 14 }}>
              <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap", marginBottom: 8 }}>
                <span style={{ fontFamily: "var(--font-mono)", fontWeight: 600, fontSize: 14, color: "var(--purple)" }}>
                  {formatRevision(head.revision)}
                </span>
                <Badge tone={head.validation_report.valid ? "green" : "red"}>
                  {head.validation_report.valid ? "Valid" : "Invalid"}
                </Badge>
                {head.source_revision !== undefined && head.source_revision > 0 && (
                  <Badge tone="blue">restored from {formatRevision(head.source_revision)}</Badge>
                )}
              </div>
              <div style={{ fontSize: 12, color: "var(--ink2)", marginBottom: 10 }}>
                {formatDateTime(head.created_at)} · {head.created_by} · {head.document.routes.length} routes · immutable
                once published
              </div>
              <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                <span style={{ fontSize: 11, fontWeight: 600, color: "var(--ink3)" }}>VALIDATION HASH</span>
                <span style={{ fontFamily: "var(--font-mono)", fontSize: 12, color: "var(--ink2)" }}>
                  {truncateId(head.validation_hash, 12, 8)}
                </span>
                <CopyButton text={head.validation_hash} label="Copy hash" />
              </div>
              <div
                style={{
                  marginTop: 10,
                  padding: "8px 10px",
                  borderRadius: "var(--radius)",
                  background: "var(--chip)",
                  fontSize: 12,
                  color: "var(--ink2)",
                }}
              >
                Gateways confirm application asynchronously per region. A successful publish request never proves every
                gateway applied the revision — publish or restore to track rollout receipts live, or inspect a
                publication id above.
              </div>
            </Card>
          ) : (
            <Card style={{ marginBottom: 14 }}>
              <EmptyState
                title="No routing catalog published yet"
                hint="The catalog is seeded out of band; once a first revision exists, drafts can be created from it here."
              />
            </Card>
          )}

          {/* ---- publication tracker (after publish/restore or manual inspect) ---- */}
          {trackedPubId && (
            <div style={{ marginBottom: 14 }}>
              <PublicationTracker
                publicationId={trackedPubId}
                onClose={() => setTrackedPubId(null)}
                onTerminal={() => {
                  retry();
                  setRevisionsSignal((n) => n + 1);
                }}
              />
            </div>
          )}

          {/* ---- sections ---- */}
          <Tabs tabs={TABS} active={tab} onChange={setTab} />
          {tab === "Active routes" &&
            (head ? (
              <Card title={`Routes in ${formatRevision(head.revision)}`}>
                <RoutesTable routes={head.document.routes} />
              </Card>
            ) : (
              <Card>
                <EmptyState title="No active routes" hint="Nothing has been published yet." />
              </Card>
            ))}
          {tab === "Drafts" && (
            <DraftsSection
              head={head}
              createSignal={createSignal}
              onPublished={afterPublicationAccepted}
              onHeadChanged={retry}
            />
          )}
          {tab === "Revisions" && (
            <RevisionsSection
              headRevision={head?.revision ?? null}
              refreshSignal={revisionsSignal}
              onPublished={afterPublicationAccepted}
              onHeadChanged={retry}
            />
          )}
        </>
      )}
    </div>
  );
}
