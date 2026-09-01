/**
 * Provider Connection detail (#/providers/{id}): configuration edit (PATCH),
 * enable/disable, and asynchronous operations (probe / model discovery /
 * credential rotation) with 1.5s polling until a terminal status.
 */
import { useCallback, useEffect, useState } from "react";
import { apiGet, apiSend } from "../../api/client";
import { ErrorBanner, Loading, useToast } from "../../components/feedback";
import { Badge, Button, Card, CodeBlock, EmptyState, KeyValueList } from "../../components/ui";
import { useApi } from "../../api/useApi";
import { formatDateTime } from "../../lib/format";
import { navigate } from "../../router";
import {
  Field,
  inputStyle,
  monoInputStyle,
  parseJsonObject,
  ReasonModal,
  statusTone,
  toastMutationError,
} from "../tenants/common";
import {
  isTerminalStatus,
  OPERATION_TYPE_LABEL,
  type ProviderConnection,
  type ProviderOperation,
  type ProviderOperationPage,
} from "./types";

/* ------------------------------------------------------------------ */
/* Configuration edit                                                  */
/* ------------------------------------------------------------------ */

function ConfigurationCard({
  connection,
  onChanged,
}: {
  connection: ProviderConnection;
  onChanged: () => void;
}) {
  const toast = useToast();
  const [displayName, setDisplayName] = useState(connection.display_name);
  const [baseUrl, setBaseUrl] = useState(connection.base_url);
  const [region, setRegion] = useState(connection.region);
  const [credentialScope, setCredentialScope] = useState(connection.credential_scope);
  const [capsJson, setCapsJson] = useState(
    connection.capability_declaration && Object.keys(connection.capability_declaration.features ?? {}).length > 0
      ? JSON.stringify(connection.capability_declaration, null, 2)
      : "",
  );
  const [reason, setReason] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    setDisplayName(connection.display_name);
    setBaseUrl(connection.base_url);
    setRegion(connection.region);
    setCredentialScope(connection.credential_scope);
    setCapsJson(
      connection.capability_declaration && Object.keys(connection.capability_declaration.features ?? {}).length > 0
        ? JSON.stringify(connection.capability_declaration, null, 2)
        : "",
    );
  }, [connection]);

  const save = async () => {
    const caps = parseJsonObject(capsJson);
    if (caps.error) {
      toast(`Capability declaration: ${caps.error}`, "error");
      return;
    }
    setSaving(true);
    try {
      await apiSend("PATCH", `/control/v1/provider-connections/${encodeURIComponent(connection.id)}`, {
        display_name: displayName,
        base_url: baseUrl,
        region,
        credential_scope: credentialScope,
        ...(caps.value ? { capability_declaration: caps.value } : {}),
        expected_revision: connection.revision,
        reason,
      });
      toast("Connection updated", "success");
      setReason("");
      onChanged();
    } catch (err) {
      toastMutationError(toast, err, onChanged);
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card
      title={
        <span>
          Configuration{" "}
          <span style={{ fontFamily: "var(--font-mono)", color: "var(--purple)", fontWeight: 500 }}>
            rev {connection.revision}
          </span>
        </span>
      }
    >
      <KeyValueList
        items={[
          { key: "Credential version", value: `v${connection.credential_version}`, mono: true },
          { key: "Created", value: formatDateTime(connection.created_at), mono: true },
          { key: "Updated", value: formatDateTime(connection.updated_at), mono: true },
        ]}
      />
      <div style={{ borderTop: "1px solid var(--line)", marginTop: 12, paddingTop: 12 }}>
        <Field label="Display name">
          <input value={displayName} onChange={(e) => setDisplayName(e.target.value)} style={inputStyle} />
        </Field>
        <Field label="Canonical base URL">
          <input value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} style={monoInputStyle} />
        </Field>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: "0 12px" }}>
          <Field label="Region">
            <input value={region} onChange={(e) => setRegion(e.target.value)} style={monoInputStyle} />
          </Field>
          <Field label="Credential scope">
            <input value={credentialScope} onChange={(e) => setCredentialScope(e.target.value)} style={monoInputStyle} />
          </Field>
        </div>
        <Field
          label="Capability declaration (JSON object)"
          hint='CapabilityProfile — e.g. {"features":{"text":"native","moderation":"translated"}}. Empty = leave unchanged.'
        >
          <textarea
            value={capsJson}
            onChange={(e) => setCapsJson(e.target.value)}
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
      </div>
      <div
        style={{
          marginTop: 12,
          padding: "9px 11px",
          borderRadius: 8,
          background: "var(--chip)",
          fontSize: 11.5,
          color: "var(--ink2)",
        }}
      >
        Credentials are <b>write-only</b>. The stored secret is never echoed here, in the API, or in audit data —
        only its version number is visible. Use credential rotation to replace it.
      </div>
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/* Operations list                                                     */
/* ------------------------------------------------------------------ */

function OperationRow({ operation }: { operation: ProviderOperation }) {
  const [expanded, setExpanded] = useState(false);
  const pending = !isTerminalStatus(operation.status);
  return (
    <div style={{ borderBottom: "1px solid var(--line)" }}>
      <button
        onClick={() => setExpanded((v) => !v)}
        style={{
          width: "100%",
          display: "flex",
          alignItems: "center",
          gap: 10,
          padding: "9px 14px",
          border: "none",
          background: "transparent",
          textAlign: "left",
          fontSize: 12,
          cursor: "pointer",
          color: "var(--ink)",
        }}
      >
        <Badge tone={statusTone(operation.status)}>{operation.status}</Badge>
        <span style={{ fontWeight: 500 }}>{OPERATION_TYPE_LABEL[operation.type] ?? operation.type}</span>
        {pending && <span style={{ fontSize: 11, color: "var(--ink3)" }}>polling…</span>}
        <span style={{ flex: 1 }} />
        <span style={{ fontFamily: "var(--font-mono)", fontSize: 10.5, color: "var(--ink3)" }}>
          {operation.started_at ? formatDateTime(operation.started_at) : "queued"} →{" "}
          {operation.completed_at ? formatDateTime(operation.completed_at) : "—"}
        </span>
      </button>
      {expanded && (
        <div style={{ padding: "0 14px 12px", fontSize: 12 }}>
          <div style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--purple)", marginBottom: 6 }}>
            {operation.id}
          </div>
          {operation.status === "uncertain" && (
            <div
              style={{
                padding: "8px 10px",
                borderRadius: 8,
                background: "var(--amber-bg)",
                color: "var(--amber)",
                fontSize: 11.5,
                fontWeight: 600,
                marginBottom: 6,
              }}
            >
              Outcome uncertain — the operation may have been applied. Do not retry blindly; verify the connection
              state (e.g. credential version) first.
            </div>
          )}
          {operation.error_message && (
            <div
              style={{
                padding: "8px 10px",
                borderRadius: 8,
                background: "var(--red-bg)",
                color: "var(--red)",
                fontSize: 11.5,
                fontFamily: "var(--font-mono)",
                marginBottom: 6,
              }}
            >
              {operation.error_code ? `${operation.error_code} — ` : ""}
              {operation.error_message}
            </div>
          )}
          {operation.result && Object.keys(operation.result).length > 0 && (
            <CodeBlock code={JSON.stringify(operation.result, null, 2)} lang="json" />
          )}
        </div>
      )}
    </div>
  );
}

function OperationsCard({ operations, loading }: { operations: ProviderOperation[]; loading: boolean }) {
  return (
    <Card title="Recent asynchronous operations" style={{ padding: 0 }}>
      <div style={{ padding: "0 0 4px" }}>
        {loading ? (
          <div style={{ padding: 24 }}><Loading /></div>
        ) : operations.length === 0 ? (
          <EmptyState
            title="No recent operations"
            hint="Run a probe, model discovery, or credential rotation to see it here."
          />
        ) : (
          operations.map((op) => <OperationRow key={op.id} operation={op} />)
        )}
      </div>
      <div style={{ padding: "9px 14px", fontSize: 11, color: "var(--ink3)", borderTop: "1px solid var(--line)" }}>
        Operations resolve to Succeeded, Failed, or <b>Uncertain</b> — uncertain means the outcome could not be
        confirmed; verify state before retrying.
      </div>
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/* Detail                                                              */
/* ------------------------------------------------------------------ */

export default function ProviderDetail({ connectionId }: { connectionId: string }) {
  const toast = useToast();
  const base = `/control/v1/provider-connections/${encodeURIComponent(connectionId)}`;
  const { data: connection, error, loading, retry } = useApi<ProviderConnection>(base);

  const [statusModal, setStatusModal] = useState<"enable" | "disable" | null>(null);
  const [probeOpen, setProbeOpen] = useState(false);
  const [discoverOpen, setDiscoverOpen] = useState(false);
  const [rotateOpen, setRotateOpen] = useState(false);
  const [rotateSecret, setRotateSecret] = useState("");
  const [actionBusy, setActionBusy] = useState(false);

  const [operations, setOperations] = useState<ProviderOperation[]>([]);
  const [operationsLoading, setOperationsLoading] = useState(true);

  const loadOperations = useCallback(
    () =>
      apiGet<ProviderOperationPage>(
        `/control/v1/provider-operations?connection_id=${encodeURIComponent(connectionId)}&limit=50`,
      ).then((page) => setOperations(page.data ?? [])),
    [connectionId],
  );

  useEffect(() => {
    let cancelled = false;
    setOperationsLoading(true);
    void loadOperations()
      .catch(() => undefined)
      .finally(() => {
        if (!cancelled) setOperationsLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [loadOperations]);

  useEffect(() => {
    const pending = operations.filter((op) => !isTerminalStatus(op.status));
    if (pending.length === 0) return;
    const timer = window.setTimeout(() => {
      void Promise.all(
        pending.map((op) => apiGet<ProviderOperation>(`/control/v1/provider-operations/${encodeURIComponent(op.id)}`)),
      )
        .then((fresh) => {
          setOperations((current) => {
            const becameTerminal = fresh.some(
              (op) =>
                isTerminalStatus(op.status) &&
                !isTerminalStatus(current.find((c) => c.id === op.id)?.status ?? ""),
            );
            if (becameTerminal) {
              retry();
              void loadOperations();
            }
            return current.map((op) => fresh.find((f) => f.id === op.id) ?? op);
          });
        })
        .catch(() => undefined);
    }, 1500);
    return () => window.clearTimeout(timer);
  }, [loadOperations, operations, retry]);

  const trackOperation = (operation: ProviderOperation) => {
    setOperations((current) => [operation, ...current.filter((op) => op.id !== operation.id)]);
  };

  const runStatusChange = async (reason: string) => {
    if (!connection || !statusModal) return;
    setActionBusy(true);
    try {
      await apiSend("POST", `${base}/${statusModal}`, { expected_revision: connection.revision, reason });
      toast(`Connection ${statusModal}d`, "success");
      setStatusModal(null);
      retry();
    } catch (err) {
      toastMutationError(toast, err, retry);
    } finally {
      setActionBusy(false);
    }
  };

  const startOperation = async (kind: "probes" | "model-discoveries" | "credential-rotations", reason: string) => {
    if (!connection) return;
    if (kind === "credential-rotations" && !rotateSecret.trim()) {
      toast("A new secret is required for credential rotation", "error");
      return;
    }
    setActionBusy(true);
    try {
      const operation = await apiSend<ProviderOperation>("POST", `${base}/${kind}`, {
        expected_revision: connection.revision,
        ...(kind === "credential-rotations" ? { secret: rotateSecret } : {}),
        reason,
      });
      toast("Request accepted — operation queued", "success");
      trackOperation(operation);
      void loadOperations();
      setProbeOpen(false);
      setDiscoverOpen(false);
      setRotateOpen(false);
      setRotateSecret("");
    } catch (err) {
      toastMutationError(toast, err, retry);
    } finally {
      setActionBusy(false);
    }
  };

  if (loading) return <Loading />;
  if (error) return <ErrorBanner error={error} retry={retry} />;
  if (!connection) return <EmptyState title="Connection not found" />;

  const enabled = connection.administrative_status === "enabled";
  const features = connection.capability_declaration?.features ?? {};

  return (
    <div style={{ padding: "20px 24px", maxWidth: 1360, margin: "0 auto" }}>
      <Button
        onClick={() => navigate("providers")}
        style={{ border: "none", padding: 0, marginBottom: 8, color: "var(--blue)", background: "transparent" }}
      >
        ← All connections
      </Button>
      <div style={{ display: "flex", alignItems: "flex-start", gap: 12, marginBottom: 16, flexWrap: "wrap" }}>
        <div style={{ flex: 1, minWidth: 240 }}>
          <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
            <h1 style={{ margin: 0, fontSize: 18, fontWeight: 600 }}>{connection.display_name}</h1>
            <Badge tone="purple">{connection.provider}</Badge>
            <Badge tone={statusTone(connection.administrative_status)}>{connection.administrative_status}</Badge>
          </div>
          <div style={{ marginTop: 3, fontFamily: "var(--font-mono)", fontSize: 11.5, color: "var(--ink3)" }}>
            {connection.id}
          </div>
        </div>
        <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
          <Button onClick={() => setProbeOpen(true)}>Run probe</Button>
          <Button onClick={() => setDiscoverOpen(true)}>Discover models</Button>
          <Button onClick={() => setRotateOpen(true)}>Rotate credential…</Button>
          <Button
            variant={enabled ? "ghost" : "primary"}
            style={enabled ? { border: "1px solid var(--red)", color: "var(--red)" } : undefined}
            onClick={() => setStatusModal(enabled ? "disable" : "enable")}
          >
            {enabled ? "Disable" : "Enable"} connection…
          </Button>
        </div>
      </div>
      <div
        style={{ display: "grid", gridTemplateColumns: "minmax(0,1.6fr) minmax(0,1fr)", gap: 14, alignItems: "start" }}
      >
        <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          <ConfigurationCard connection={connection} onChanged={retry} />
          <OperationsCard operations={operations} loading={operationsLoading} />
        </div>
        <Card title="Capability declaration">
          {Object.keys(features).length === 0 ? (
            <div style={{ fontSize: 12, color: "var(--ink3)" }}>No capabilities declared.</div>
          ) : (
            <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
              {Object.entries(features).map(([feature, support]) => (
                <div key={feature} style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 12 }}>
                  <span style={{ fontFamily: "var(--font-mono)", fontSize: 11.5 }}>{feature}</span>
                  <span style={{ flex: 1 }} />
                  <Badge tone={statusTone(support)}>{support}</Badge>
                </div>
              ))}
            </div>
          )}
          <div style={{ fontSize: 11, color: "var(--ink3)", marginTop: 10 }}>
            Declaration revision {connection.capability_declaration?.revision ?? 0} — updated by model discovery or
            explicit PATCH.
          </div>
        </Card>
      </div>

      <ReasonModal
        open={statusModal !== null}
        onClose={() => setStatusModal(null)}
        onConfirm={(reason) => void runStatusChange(reason)}
        title={`${statusModal === "enable" ? "Enable" : "Disable"} connection`}
        description="Administrative status is separate from observed health. Disabling stops new route resolution through this connection; it does not signal provider failure."
        confirmLabel={statusModal === "enable" ? "Enable connection" : "Disable connection"}
        danger={statusModal === "disable"}
        busy={actionBusy}
      />
      <ReasonModal
        open={probeOpen}
        onClose={() => setProbeOpen(false)}
        onConfirm={(reason) => void startOperation("probes", reason)}
        title="Run probe"
        description="Queues an asynchronous probe. The operation is polled until it reaches a terminal status."
        confirmLabel="Queue probe"
        busy={actionBusy}
      />
      <ReasonModal
        open={discoverOpen}
        onClose={() => setDiscoverOpen(false)}
        onConfirm={(reason) => void startOperation("model-discoveries", reason)}
        title="Discover models"
        description="Queues asynchronous model discovery. Discovered models and capability updates attach to the operation result."
        confirmLabel="Queue discovery"
        busy={actionBusy}
      />
      <ReasonModal
        open={rotateOpen}
        onClose={() => {
          setRotateOpen(false);
          setRotateSecret("");
        }}
        onConfirm={(reason) => void startOperation("credential-rotations", reason)}
        title="Rotate credential"
        description="Queues asynchronous credential rotation. The new secret is write-only: stored in secret custody, never read back."
        confirmLabel="Queue rotation"
        busy={actionBusy}
      >
        <Field label="New secret (write-only)">
          <input
            type="password"
            value={rotateSecret}
            onChange={(e) => setRotateSecret(e.target.value)}
            style={monoInputStyle}
            placeholder="sk-…"
            autoComplete="new-password"
          />
        </Field>
      </ReasonModal>
    </div>
  );
}
