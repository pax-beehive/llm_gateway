import { useCallback, useEffect, useState } from "react";
import { Badge, Button, Tabs } from "../../components/ui";
import { useReadiness } from "./hooks";
import { ConsumersTab, GatewaysTab, JobsTab, MeteringTab, OutboxTab, PublicationsTab } from "./tabs";
import type { ReadinessResult } from "./types";

const TABS = ["Gateways", "Metering", "Publications", "Outbox", "Consumers", "Jobs"] as const;

/**
 * Operations: diagnosis-first, read-only views over the control plane
 * /operations endpoints, with process-level readiness chips and a 10s
 * auto-refresh.
 */
export default function OperationsPage() {
  const [tab, setTab] = useState<string>(TABS[0]);
  const [auto, setAuto] = useState(true);
  const [tick, setTick] = useState(0);
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null);

  useEffect(() => {
    if (!auto) return;
    const timer = window.setInterval(() => setTick((t) => t + 1), 10_000);
    return () => window.clearInterval(timer);
  }, [auto]);

  const onUpdated = useCallback((at: Date) => setUpdatedAt(at), []);

  const controlReady = useReadiness("/control/readyz", tick);
  const gatewayReady = useReadiness("/llm/readyz", tick);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 14, maxWidth: 1360 }}>
      <div style={{ display: "flex", alignItems: "flex-start", gap: 16, flexWrap: "wrap" }}>
        <div style={{ flex: 1, minWidth: 240 }}>
          <h1 style={{ margin: 0, fontSize: 18, fontWeight: 600 }}>Operations</h1>
          <div style={{ color: "var(--ink2)", marginTop: 2, fontSize: 12 }}>
            Diagnosis-first views of gateways, metering, publications, and background jobs.
          </div>
        </div>
        <ReadinessChip label="control plane" result={controlReady} />
        <ReadinessChip label="gateway" result={gatewayReady} />
        <label style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 12, color: "var(--ink2)", cursor: "pointer" }}>
          <input type="checkbox" checked={auto} onChange={(e) => setAuto(e.target.checked)} />
          Auto-refresh 10s
        </label>
        <Button onClick={() => setTick((t) => t + 1)}>Refresh</Button>
        <span style={{ fontSize: 11, color: "var(--ink3)", alignSelf: "center" }}>
          {updatedAt ? `last updated ${updatedAt.toLocaleTimeString("en-US", { hour12: false })}` : "not loaded yet"}
        </span>
      </div>

      <Tabs tabs={[...TABS]} active={tab} onChange={setTab} />

      {tab === "Gateways" && <GatewaysTab tick={tick} onUpdated={onUpdated} />}
      {tab === "Metering" && <MeteringTab tick={tick} onUpdated={onUpdated} />}
      {tab === "Publications" && <PublicationsTab tick={tick} onUpdated={onUpdated} />}
      {tab === "Outbox" && <OutboxTab tick={tick} onUpdated={onUpdated} />}
      {tab === "Consumers" && <ConsumersTab tick={tick} onUpdated={onUpdated} />}
      {tab === "Jobs" && <JobsTab tick={tick} onUpdated={onUpdated} />}
    </div>
  );
}

function ReadinessChip({ label, result }: { label: string; result: ReadinessResult | null }) {
  const failed = result?.checks.filter((c) => c.status !== "ready") ?? [];
  const title = result
    ? result.ready
      ? `ready · checked ${result.checked_at}`
      : `not ready · ${failed.map((c) => `${c.name}: ${c.status}`).join(", ") || "unavailable"}`
    : "checking…";
  return (
    <span title={title} style={{ alignSelf: "center" }}>
      <Badge tone={result === null ? "neutral" : result.ready ? "green" : "red"}>
        {label} {result === null ? "…" : result.ready ? "ready" : "not ready"}
      </Badge>
    </span>
  );
}
