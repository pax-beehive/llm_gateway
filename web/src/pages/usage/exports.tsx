import { useState } from "react";
import { apiSend, ApiError } from "../../api/client";
import { ErrorBanner, Loading, Modal, useToast } from "../../components/feedback";
import { Badge, Button, Card, EmptyState } from "../../components/ui";
import { formatDateTime, formatNumber, truncateId } from "../../lib/format";
import { useQuery } from "./hooks";
import { exportStatusTone, type ExportJob, type UsageFilter, usageQuery } from "./types";

/**
 * Export jobs: request a CSV export at a fixed projection cutoff, list jobs,
 * and download through the short-lived signed URL exposed via the Link header
 * of GET /usage/exports/{id}.
 */
export function ExportJobs({ filter, refreshKey }: { filter: UsageFilter; refreshKey: number }) {
  const toast = useToast();
  const path = `/metering/v1/usage/exports?tenant_id=${encodeURIComponent(filter.tenantId)}&limit=50`;
  const { data, error, loading, retry } = useQuery<{ data: ExportJob[] }>(path, refreshKey);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);

  const createExport = () => {
    setBusy(true);
    apiSend<ExportJob>("POST", `/metering/v1/usage/exports?${usageQuery(filter)}`)
      .then((job) => {
        setBusy(false);
        setConfirming(false);
        toast(`Export ${truncateId(job.id)} queued at cutoff ${formatDateTime(job.cutoff)}`, "success");
        retry();
      })
      .catch((err: unknown) => {
        setBusy(false);
        if (err instanceof ApiError) toast(`${err.code}: ${err.message}`, "error");
        else throw err;
      });
  };

  const download = (job: ExportJob) => {
    // The signed content URL is only exposed via the Link response header, so
    // this needs the raw Response rather than apiGet.
    fetch(`/api/metering/v1/usage/exports/${encodeURIComponent(job.id)}?tenant_id=${encodeURIComponent(job.tenant_id)}`, {
      headers: { Accept: "application/json" },
    })
      .then(async (response) => {
        if (!response.ok) {
          let code = `http_${response.status}`;
          try {
            const body = (await response.json()) as { error?: { code?: string } };
            code = body.error?.code ?? code;
          } catch {
            // Non-JSON body; keep the HTTP status code.
          }
          throw new ApiError(response.status, code, `Export detail failed with status ${response.status}`);
        }
        const link = response.headers.get("Link");
        const match = link?.match(/<([^>]+)>/);
        if (!match) {
          toast("Export is not ready for download yet", "info");
          return;
        }
        window.open(`/api${match[1]}`, "_blank", "noopener");
      })
      .catch((err: unknown) => {
        if (err instanceof ApiError) toast(`${err.code}: ${err.message}`, "error");
        else throw err;
      });
  };

  const jobs = data?.data ?? [];

  return (
    <Card
      title={
        <>
          Export jobs <span style={{ fontWeight: 400, color: "var(--ink3)" }}>· fixed cutoffs, signed downloads</span>
        </>
      }
      actions={
        <Button variant="primary" onClick={() => setConfirming(true)}>
          Request CSV export
        </Button>
      }
    >
      {loading && <Loading />}
      {error && <ErrorBanner error={error} retry={retry} />}
      {!loading && !error && jobs.length === 0 && (
        <EmptyState title="No exports" hint="Request a CSV export of the ledger at a fixed cutoff" />
      )}
      {jobs.length > 0 && (
        <div style={{ display: "flex", flexDirection: "column" }}>
          {jobs.map((job) => (
            <div
              key={job.id}
              style={{
                display: "flex",
                alignItems: "center",
                gap: 10,
                padding: "9px 0",
                borderBottom: "1px solid var(--line)",
                fontSize: 12,
                flexWrap: "wrap",
              }}
            >
              <span style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--purple)" }}>
                {truncateId(job.id)}
              </span>
              <span style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--ink2)" }}>
                cutoff {formatDateTime(job.cutoff)}
              </span>
              <Badge tone={exportStatusTone(job.status)}>{job.status}</Badge>
              {job.error_code && (
                <span style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--red)" }}>{job.error_code}</span>
              )}
              <span style={{ flex: 1 }} />
              <span style={{ fontSize: 11, color: "var(--ink3)" }}>
                {formatNumber(job.row_count)} rows
                {job.sha256 ? ` · sha256 ${job.sha256.slice(0, 8)}…` : ""}
              </span>
              {job.status === "succeeded" && (
                <Button onClick={() => download(job)}>Download · signed, 5 min</Button>
              )}
            </div>
          ))}
          <div style={{ paddingTop: 9, fontSize: 11, color: "var(--ink3)" }}>
            Downloads are available only after success, via short-lived signed URLs. Integrity is verified against the
            ledger before release.
          </div>
        </div>
      )}

      <Modal
        open={confirming}
        onClose={() => setConfirming(false)}
        title="Request CSV export"
        footer={
          <>
            <Button onClick={() => setConfirming(false)}>Cancel</Button>
            <Button variant="primary" disabled={busy} onClick={createExport}>
              {busy ? "Requesting…" : "Request export"}
            </Button>
          </>
        }
      >
        <p style={{ fontSize: 12, color: "var(--ink2)", marginTop: 0 }}>
          Queues an export of usage events for tenant{" "}
          <code style={{ fontFamily: "var(--font-mono)" }}>{filter.tenantId}</code> at the current projection cutoff,
          using the active filter bar selection. Events after the cutoff land in the next export — cutoffs never move.
        </p>
      </Modal>
    </Card>
  );
}
