/**
 * Tenant detail (routed sub-view #/tenants/{id}) with tabs:
 * Overview, Limit Policy, Policy Revisions, Gateway API Keys, Quota Snapshot.
 */
import { useEffect, useState } from "react";
import { apiSend } from "../../api/client";
import { ErrorBanner, Loading, useToast } from "../../components/feedback";
import { Badge, Button, Card, CodeBlock, EmptyState, KeyValueList, Tabs } from "../../components/ui";
import { useApi } from "../../api/useApi";
import { navigate } from "../../router";
import { Field, inputStyle, monoInputStyle, parseJsonObject, ReasonModal, statusTone, toastMutationError } from "./common";
import { GatewayKeysTab } from "./keys";
import { PolicyRevisionsTab, QuotaSnapshotPanel, TenantPolicyTab } from "./policy";
import type { Tenant } from "./types";

const TABS = ["Overview", "Limit Policy", "Policy Revisions", "Gateway API Keys", "Quota Snapshot"];

function OverviewTab({ tenant, onChanged }: { tenant: Tenant; onChanged: () => void }) {
  const toast = useToast();
  const [displayName, setDisplayName] = useState(tenant.display_name);
  const [metadata, setMetadata] = useState(tenant.metadata ? JSON.stringify(tenant.metadata, null, 2) : "");
  const [reason, setReason] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    setDisplayName(tenant.display_name);
    setMetadata(tenant.metadata ? JSON.stringify(tenant.metadata, null, 2) : "");
  }, [tenant]);

  const save = async () => {
    const meta = parseJsonObject(metadata);
    if (meta.error) {
      toast(`Metadata: ${meta.error}`, "error");
      return;
    }
    setSaving(true);
    try {
      await apiSend("PATCH", `/control/v1/tenants/${encodeURIComponent(tenant.id)}`, {
        display_name: displayName,
        ...(meta.value ? { metadata: meta.value } : {}),
        expected_revision: tenant.revision,
        reason,
      });
      toast("Tenant updated", "success");
      setReason("");
      onChanged();
    } catch (err) {
      toastMutationError(toast, err, onChanged);
    } finally {
      setSaving(false);
    }
  };

  return (
    <div
      style={{ display: "grid", gridTemplateColumns: "minmax(0,1.6fr) minmax(0,1fr)", gap: 14, alignItems: "start" }}
    >
      <Card title="Identity">
        <KeyValueList
          items={[
            { key: "ID", value: tenant.id, mono: true },
            { key: "Slug", value: tenant.slug, mono: true },
            { key: "Display name", value: tenant.display_name },
            {
              key: "Status",
              value: <Badge tone={statusTone(tenant.status)}>{tenant.status}</Badge>,
            },
            { key: "Home region", value: tenant.home_region, mono: true },
            { key: "Execution epoch", value: String(tenant.execution_epoch), mono: true },
            { key: "Policy revision", value: String(tenant.policy?.revision ?? 0), mono: true },
            { key: "Revision", value: String(tenant.revision), mono: true },
          ]}
        />
        {tenant.metadata && (
          <div style={{ marginTop: 12 }}>
            <div style={{ fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 6 }}>Metadata</div>
            <CodeBlock code={JSON.stringify(tenant.metadata, null, 2)} lang="json" />
          </div>
        )}
      </Card>
      {tenant.status !== "closed" && (
        <Card title="Edit tenant">
          <Field label="Display name">
            <input value={displayName} onChange={(e) => setDisplayName(e.target.value)} style={inputStyle} />
          </Field>
          <Field label="Metadata (JSON object)">
            <textarea
              value={metadata}
              onChange={(e) => setMetadata(e.target.value)}
              rows={4}
              style={{ ...monoInputStyle, resize: "vertical" }}
            />
          </Field>
          <Field label="Reason (required)">
            <textarea
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              rows={2}
              style={{ ...inputStyle, resize: "vertical" }}
            />
          </Field>
          <div style={{ display: "flex", justifyContent: "flex-end" }}>
            <Button variant="primary" disabled={!reason.trim() || saving} onClick={() => void save()}>
              Save changes
            </Button>
          </div>
        </Card>
      )}
    </div>
  );
}

type TransitionTarget = "suspended" | "active" | "closed";

const TRANSITION_LABEL: Record<TransitionTarget, string> = {
  suspended: "Suspend tenant",
  active: "Reactivate tenant",
  closed: "Close tenant",
};

const TRANSITION_DESCRIPTION: Record<TransitionTarget, string> = {
  suspended: "Suspension stops new requests for this tenant — they are policy-denied until reactivation.",
  active: "Reactivation restores request authorization for this tenant.",
  closed: "Closing is terminal. The tenant can no longer be reactivated or mutated. This cannot be undone.",
};

export default function TenantDetail({ tenantId }: { tenantId: string }) {
  const toast = useToast();
  const { data: tenant, error, loading, retry } = useApi<Tenant>(
    `/control/v1/tenants/${encodeURIComponent(tenantId)}`,
  );
  const [tab, setTab] = useState("Overview");
  const [transition, setTransition] = useState<TransitionTarget | null>(null);
  const [transitionBusy, setTransitionBusy] = useState(false);

  const runTransition = async (reason: string) => {
    if (!tenant || !transition) return;
    setTransitionBusy(true);
    try {
      await apiSend("POST", `/control/v1/tenants/${encodeURIComponent(tenant.id)}/transitions`, {
        target: transition,
        expected_revision: tenant.revision,
        reason,
      });
      toast(`Tenant ${transition === "active" ? "reactivated" : transition}`, "success");
      setTransition(null);
      retry();
    } catch (err) {
      toastMutationError(toast, err, retry);
    } finally {
      setTransitionBusy(false);
    }
  };

  if (loading) return <Loading />;
  if (error) return <ErrorBanner error={error} retry={retry} />;
  if (!tenant) return <EmptyState title="Tenant not found" />;

  const transitions: TransitionTarget[] =
    tenant.status === "active" ? ["suspended", "closed"] : tenant.status === "suspended" ? ["active", "closed"] : [];

  return (
    <div style={{ padding: "20px 24px", maxWidth: 1360, margin: "0 auto" }}>
      <Button onClick={() => navigate("tenants")} style={{ border: "none", padding: 0, marginBottom: 8, color: "var(--blue)", background: "transparent" }}>
        ← All tenants
      </Button>
      <div style={{ display: "flex", alignItems: "flex-start", gap: 12, marginBottom: 14, flexWrap: "wrap" }}>
        <div style={{ flex: 1 }}>
          <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
            <h1 style={{ margin: 0, fontSize: 18, fontWeight: 600 }}>{tenant.display_name}</h1>
            <Badge tone={statusTone(tenant.status)}>{tenant.status}</Badge>
          </div>
          <div style={{ marginTop: 3, fontFamily: "var(--font-mono)", fontSize: 11.5, color: "var(--ink3)" }}>
            {tenant.id} · slug {tenant.slug}
          </div>
        </div>
        {transitions.map((target) => (
          <Button
            key={target}
            variant={target === "closed" ? "danger" : target === "suspended" ? "ghost" : "primary"}
            style={target === "suspended" ? { border: "1px solid var(--red)", color: "var(--red)" } : undefined}
            onClick={() => setTransition(target)}
          >
            {TRANSITION_LABEL[target]}…
          </Button>
        ))}
      </div>
      <Tabs tabs={TABS} active={tab} onChange={setTab} />
      {tab === "Overview" && <OverviewTab tenant={tenant} onChanged={retry} />}
      {tab === "Limit Policy" && <TenantPolicyTab tenantId={tenantId} />}
      {tab === "Policy Revisions" && (
        <PolicyRevisionsTab
          policyPath={`/control/v1/tenants/${encodeURIComponent(tenantId)}/policy`}
          revisionsPath={`/control/v1/tenants/${encodeURIComponent(tenantId)}/policy-revisions`}
        />
      )}
      {tab === "Gateway API Keys" && <GatewayKeysTab tenantId={tenantId} />}
      {tab === "Quota Snapshot" && (
        <QuotaSnapshotPanel path={`/control/v1/tenants/${encodeURIComponent(tenantId)}/quota-snapshot`} />
      )}
      <ReasonModal
        open={transition !== null}
        onClose={() => setTransition(null)}
        onConfirm={(reason) => void runTransition(reason)}
        title={transition ? TRANSITION_LABEL[transition] : ""}
        description={transition ? TRANSITION_DESCRIPTION[transition] : undefined}
        confirmLabel={transition ? TRANSITION_LABEL[transition] : "Confirm"}
        danger={transition === "closed" || transition === "suspended"}
        busy={transitionBusy}
      />
    </div>
  );
}
