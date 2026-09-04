/**
 * Revisions section: immutable revision history with cursor pagination
 * (Load more), a per-revision detail drawer, and Restore — which creates a
 * NEW publication whose content equals the chosen revision (history is
 * never rewritten).
 */
import { useCallback, useEffect, useState } from "react";
import { ApiError } from "../../api/client";
import { useAuth } from "../../auth/AuthProvider";
import { PERMISSIONS } from "../../auth/permissions";
import { ConfirmDialog, Drawer, useToast } from "../../components/feedback";
import { Badge, Button, Card, CodeBlock, CopyButton, EmptyState, KeyValueList, Spinner, Table, Td, Th } from "../../components/ui";
import { formatDateTime, truncateId } from "../../lib/format";
import { listRevisions, restoreRevision } from "./api";
import { formatRevision, parseRegions, type Revision } from "./types";

const PAGE_SIZE = 25;

const inputStyle = {
  padding: "6px 10px",
  borderRadius: "var(--radius)",
  border: "1px solid var(--line)",
  background: "var(--bg)",
  color: "var(--ink)",
  fontSize: 12,
  fontFamily: "var(--font-mono)",
} as const;

export function RevisionsSection({
  headRevision,
  refreshSignal,
  onPublished,
  onHeadChanged,
}: {
  headRevision: number | null;
  /** Bumped by the parent after a publish/restore so the history refetches. */
  refreshSignal: number;
  onPublished: (publicationId: string) => void;
  onHeadChanged: () => void;
}) {
  const toast = useToast();
  const { can } = useAuth();
  const canWrite = can(PERMISSIONS.routingWrite);
  const [revisions, setRevisions] = useState<Revision[]>([]);
  const [nextCursor, setNextCursor] = useState<number | undefined>(undefined);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [selected, setSelected] = useState<Revision | null>(null);
  const [restoreTarget, setRestoreTarget] = useState<Revision | null>(null);
  const [restoreRegions, setRestoreRegions] = useState("");
  const [restoring, setRestoring] = useState(false);

  const loadFirst = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const page = await listRevisions(undefined, PAGE_SIZE);
      setRevisions(page.data ?? []);
      setNextCursor(page.next_cursor || undefined);
    } catch (err) {
      setError(err instanceof ApiError ? err : new ApiError(0, "network", String(err)));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void loadFirst();
  }, [loadFirst, refreshSignal]);

  const loadMore = async () => {
    if (nextCursor === undefined) return;
    setLoadingMore(true);
    try {
      const page = await listRevisions(nextCursor, PAGE_SIZE);
      setRevisions((current) => [...current, ...(page.data ?? [])]);
      setNextCursor(page.next_cursor || undefined);
    } catch (err) {
      if (err instanceof ApiError) toast(`${err.code}: ${err.message}`, "error");
      else throw err;
    } finally {
      setLoadingMore(false);
    }
  };

  const restore = async (reason: string) => {
    if (!restoreTarget || headRevision === null) return;
    setRestoring(true);
    try {
      const result = await restoreRevision(restoreTarget.revision, {
        expected_head: headRevision,
        required_regions: parseRegions(restoreRegions),
        reason,
      });
      toast(
        `Restore publication created — ${formatRevision(result.publication.catalog_revision)} now rolling out`,
        "success",
      );
      setRestoreTarget(null);
      setRestoreRegions("");
      onPublished(result.publication.id);
      onHeadChanged();
    } catch (err) {
      if (err instanceof ApiError) toast(`${err.code}: ${err.message}`, "error");
      else throw err;
    } finally {
      setRestoring(false);
    }
  };

  return (
    <Card
      title={
        <span>
          Revision history{" "}
          <span style={{ fontWeight: 400, color: "var(--ink3)" }}>· immutable — restore creates a new publication</span>
        </span>
      }
    >
      {loading ? (
        <div style={{ display: "flex", justifyContent: "center", padding: 24 }}>
          <Spinner />
        </div>
      ) : error ? (
        <div role="alert" style={{ padding: "10px 12px", borderRadius: "var(--radius)", background: "var(--red-bg)", color: "var(--red)", fontSize: 12 }}>
          {error.code}: {error.message}
        </div>
      ) : revisions.length === 0 ? (
        <EmptyState title="No revisions" hint="Publish a draft to create the first immutable revision." />
      ) : (
        <>
          <Table>
            <thead>
              <tr>
                <Th>Revision</Th>
                <Th>Created</Th>
                <Th>Actor</Th>
                <Th>Routes</Th>
                <Th>Validation</Th>
                <Th>Source</Th>
                <Th></Th>
              </tr>
            </thead>
            <tbody>
              {revisions.map((rev) => (
                <tr key={rev.revision} onClick={() => setSelected(rev)} style={{ cursor: "pointer" }}>
                  <Td mono>
                    <span style={{ color: "var(--purple)", fontWeight: 600 }}>{formatRevision(rev.revision)}</span>
                    {rev.revision === headRevision && (
                      <>
                        {" "}
                        <Badge tone="green">head</Badge>
                      </>
                    )}
                  </Td>
                  <Td mono>{formatDateTime(rev.created_at)}</Td>
                  <Td>{rev.created_by}</Td>
                  <Td mono>{rev.document.routes.length}</Td>
                  <Td>
                    <span style={{ fontSize: 12, color: rev.validation_report.valid ? "var(--green)" : "var(--red)" }}>
                      {rev.validation_report.valid ? "Valid" : "Failed"}
                    </span>
                    <span style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--ink3)", marginLeft: 6 }}>
                      {truncateId(rev.validation_hash, 8, 4)}
                    </span>
                  </Td>
                  <Td mono>{rev.source_revision ? formatRevision(rev.source_revision) : "—"}</Td>
                  <Td>
                    <div style={{ display: "flex", justifyContent: "flex-end" }}>
                      <Button
                        disabled={!canWrite || headRevision === null || rev.revision === headRevision}
                        title={
                          !canWrite
                            ? "Requires platform:routing:write"
                            : rev.revision === headRevision
                              ? "This revision is already the head"
                              : undefined
                        }
                        onClick={(e) => {
                          e.stopPropagation();
                          setRestoreTarget(rev);
                        }}
                      >
                        Restore…
                      </Button>
                    </div>
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
          {nextCursor !== undefined && (
            <div style={{ display: "flex", justifyContent: "center", padding: "12px 0 4px" }}>
              <Button disabled={loadingMore} onClick={() => void loadMore()}>
                {loadingMore ? <Spinner size={12} /> : "Load more"}
              </Button>
            </div>
          )}
        </>
      )}

      <RevisionDrawer revision={selected} onClose={() => setSelected(null)} />

      <ConfirmDialog
        open={restoreTarget !== null}
        onClose={() => setRestoreTarget(null)}
        onConfirm={(reason) => void restore(reason)}
        title={restoreTarget ? `Restore ${formatRevision(restoreTarget.revision)}` : "Restore revision"}
        description={
          restoreTarget && (
            <span>
              <span style={{ display: "block", marginBottom: 8 }}>
                Restore creates a NEW publication whose content equals {formatRevision(restoreTarget.revision)}.
                Revision history is immutable and is never rewritten. The request carries expected_head ={" "}
                {headRevision !== null ? formatRevision(headRevision) : "—"} and fails if the head has moved.
              </span>
              <span style={{ display: "block", fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 3 }}>
                Required regions (comma-separated, optional)
              </span>
              <input
                style={{ ...inputStyle, width: "100%" }}
                placeholder="us-west, us-east, eu-west, ap-southeast"
                value={restoreRegions}
                onChange={(e) => setRestoreRegions(e.target.value)}
              />
              {restoring && <Spinner size={12} />}
            </span>
          )
        }
        confirmLabel="Create restore publication"
      />
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/* Revision detail drawer                                              */
/* ------------------------------------------------------------------ */

function RevisionDrawer({ revision, onClose }: { revision: Revision | null; onClose: () => void }) {
  return (
    <Drawer
      open={revision !== null}
      onClose={onClose}
      title={revision ? `Revision ${formatRevision(revision.revision)}` : "Revision"}
      width={560}
    >
      {revision && (
        <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
          <KeyValueList
            items={[
              { key: "Revision", value: formatRevision(revision.revision), mono: true },
              { key: "Created", value: `${formatDateTime(revision.created_at)} · ${revision.created_by}` },
              {
                key: "Source revision",
                value: revision.source_revision ? formatRevision(revision.source_revision) : "—",
                mono: true,
              },
              {
                key: "Validation hash",
                value: (
                  <span style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
                    {truncateId(revision.validation_hash, 12, 8)}
                    <CopyButton text={revision.validation_hash} label="Copy" />
                  </span>
                ),
                mono: true,
              },
              { key: "Routes", value: String(revision.document.routes.length), mono: true },
            ]}
          />
          <div>
            <div style={{ fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 6 }}>ROUTES</div>
            <div style={{ display: "flex", flexDirection: "column", gap: 4, fontSize: 12 }}>
              {revision.document.routes.map((r) => (
                <div key={r.route_id} style={{ display: "flex", gap: 8, fontFamily: "var(--font-mono)" }}>
                  <span style={{ color: "var(--purple)" }}>{r.public_model}</span>
                  <span style={{ color: "var(--ink3)" }}>→</span>
                  <span>{r.provider_model}</span>
                  <span style={{ color: "var(--ink3)" }}>{r.execution_region}</span>
                </div>
              ))}
            </div>
          </div>
          <div>
            <div style={{ fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 6 }}>DOCUMENT JSON</div>
            <CodeBlock code={JSON.stringify(revision.document, null, 2)} lang="json" />
          </div>
        </div>
      )}
    </Drawer>
  );
}
