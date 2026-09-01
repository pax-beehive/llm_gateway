/** Small shared render pieces for the Routing Catalog page. */
import { Badge, CopyButton, Table, Td, Th } from "../../components/ui";
import { formatDateTime, formatNumber, truncateId } from "../../lib/format";
import {
  capabilityTone,
  receiptTone,
  type CapabilitySupport,
  type RolloutReceipt,
  type ValidationIssue,
  type ValidationReport,
} from "./types";

/* ------------------------------------------------------------------ */
/* Capability badges                                                   */
/* ------------------------------------------------------------------ */

export function CapabilityBadges({ capabilities }: { capabilities: Record<string, CapabilitySupport> }) {
  const entries = Object.entries(capabilities).sort(([a], [b]) => a.localeCompare(b));
  if (entries.length === 0) return <span style={{ color: "var(--ink3)", fontSize: 11 }}>—</span>;
  return (
    <span style={{ display: "inline-flex", flexWrap: "wrap", gap: 4 }}>
      {entries.map(([name, support]) => (
        <Badge key={name} tone={capabilityTone(support)}>
          {name}
        </Badge>
      ))}
    </span>
  );
}

/* ------------------------------------------------------------------ */
/* Validation report                                                   */
/* ------------------------------------------------------------------ */

function IssueTable({ issues }: { issues: ValidationIssue[] }) {
  return (
    <Table>
      <thead>
        <tr>
          <Th>Code</Th>
          <Th>Path</Th>
          <Th>Message</Th>
        </tr>
      </thead>
      <tbody>
        {issues.map((issue, i) => (
          <tr key={`${issue.code}-${issue.path}-${i}`}>
            <Td mono>{issue.code}</Td>
            <Td mono>{issue.path}</Td>
            <Td>{issue.message}</Td>
          </tr>
        ))}
      </tbody>
    </Table>
  );
}

export function ValidationReportView({ report, hash }: { report: ValidationReport; hash?: string }) {
  const errors = report.errors ?? [];
  const warnings = report.warnings ?? [];
  const validationHash = hash ?? report.hash;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
        <Badge tone={report.valid ? "green" : "red"}>{report.valid ? "Valid" : "Invalid"}</Badge>
        {validationHash && (
          <>
            <span style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--ink2)" }}>
              {truncateId(validationHash, 12, 8)}
            </span>
            <CopyButton text={validationHash} label="Copy hash" />
          </>
        )}
      </div>
      <div>
        <div style={{ fontSize: 11, fontWeight: 600, color: "var(--red)", marginBottom: 4 }}>
          ERRORS · {errors.length}
        </div>
        {errors.length === 0 ? <div style={{ color: "var(--ink3)", fontSize: 11.5 }}>None.</div> : <IssueTable issues={errors} />}
      </div>
      <div>
        <div style={{ fontSize: 11, fontWeight: 600, color: "var(--amber)", marginBottom: 4 }}>
          WARNINGS · {warnings.length}
        </div>
        {warnings.length === 0 ? (
          <div style={{ color: "var(--ink3)", fontSize: 11.5 }}>None.</div>
        ) : (
          <IssueTable issues={warnings} />
        )}
      </div>
    </div>
  );
}

/* ------------------------------------------------------------------ */
/* Rollout receipts                                                    */
/* ------------------------------------------------------------------ */

export function ReceiptsTable({ receipts }: { receipts: RolloutReceipt[] }) {
  if (receipts.length === 0) {
    return <div style={{ color: "var(--ink3)", fontSize: 11.5 }}>No receipts yet — gateways confirm asynchronously.</div>;
  }
  return (
    <Table>
      <thead>
        <tr>
          <Th>Gateway</Th>
          <Th>Region</Th>
          <Th>Status</Th>
          <Th>Lag</Th>
          <Th>Applied at</Th>
          <Th>Error</Th>
        </tr>
      </thead>
      <tbody>
        {receipts.map((r) => (
          <tr key={`${r.gateway_id}-${r.region}-${r.applied_at}`}>
            <Td mono>{truncateId(r.gateway_id)}</Td>
            <Td mono>{r.region}</Td>
            <Td>
              <Badge tone={receiptTone(r.status)}>{r.status}</Badge>
            </Td>
            <Td mono>{formatNumber(r.lag_milliseconds)} ms</Td>
            <Td mono>{formatDateTime(r.applied_at)}</Td>
            <Td mono>{r.error_code ?? "—"}</Td>
          </tr>
        ))}
      </tbody>
    </Table>
  );
}
