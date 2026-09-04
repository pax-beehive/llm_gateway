/**
 * Provider Connections console: list with provider/region/status filters and
 * cursor pagination, register modal (write-only secret), and the routed
 * detail sub-view (#/providers/{id}).
 */
import { useEffect, useState } from "react";
import { apiSend } from "../../api/client";
import { useAuth } from "../../auth/AuthProvider";
import { PERMISSIONS } from "../../auth/permissions";
import { ErrorBanner, Loading, Modal, useToast } from "../../components/feedback";
import { Badge, Button, Card, EmptyState, Table, Td, Th } from "../../components/ui";
import { navigate } from "../../router";
import {
  Field,
  inputStyle,
  monoInputStyle,
  parseJsonObject,
  statusTone,
  toastMutationError,
  useHashTail,
  usePagedList,
} from "../tenants/common";
import ProviderDetail from "./detail";
import type { ProviderConnection } from "./types";

/* ------------------------------------------------------------------ */
/* Register modal                                                      */
/* ------------------------------------------------------------------ */

function RegisterConnectionModal({
  open,
  onClose,
  onRegistered,
}: {
  open: boolean;
  onClose: () => void;
  onRegistered: () => void;
}) {
  const toast = useToast();
  const [id, setId] = useState("");
  const [provider, setProvider] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [baseUrl, setBaseUrl] = useState("");
  const [region, setRegion] = useState("");
  const [credentialScope, setCredentialScope] = useState("");
  const [secret, setSecret] = useState("");
  const [showCaps, setShowCaps] = useState(false);
  const [capsJson, setCapsJson] = useState("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (open) {
      setId("");
      setProvider("");
      setDisplayName("");
      setBaseUrl("");
      setRegion("");
      setCredentialScope("");
      setSecret("");
      setShowCaps(false);
      setCapsJson("");
      setReason("");
    }
  }, [open]);

  const submit = async () => {
    const caps = parseJsonObject(capsJson);
    if (caps.error) {
      toast(`Capability declaration: ${caps.error}`, "error");
      return;
    }
    setBusy(true);
    try {
      await apiSend<ProviderConnection>("POST", "/control/v1/provider-connections", {
        id,
        provider,
        display_name: displayName,
        base_url: baseUrl,
        region,
        credential_scope: credentialScope,
        secret,
        ...(caps.value ? { capability_declaration: caps.value } : {}),
        reason,
      });
      toast("Provider connection registered", "success");
      onRegistered();
      onClose();
    } catch (err) {
      toastMutationError(toast, err);
    } finally {
      setBusy(false);
    }
  };

  const requiredFilled =
    id.trim() &&
    provider.trim() &&
    displayName.trim() &&
    baseUrl.trim() &&
    region.trim() &&
    credentialScope.trim() &&
    secret.trim() &&
    reason.trim();

  return (
    <Modal
      open={open}
      onClose={onClose}
      title="Register provider connection"
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" disabled={!requiredFilled || busy} onClick={() => void submit()}>
            Register connection
          </Button>
        </>
      }
    >
      <Field label="ID" hint="Immutable connection identifier, e.g. pc_openai_use1.">
        <input value={id} onChange={(e) => setId(e.target.value)} style={monoInputStyle} placeholder="pc_openai_use1" />
      </Field>
      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: "0 12px" }}>
        <Field label="Provider">
          <input value={provider} onChange={(e) => setProvider(e.target.value)} style={inputStyle} placeholder="OpenAI" />
        </Field>
        <Field label="Display name">
          <input value={displayName} onChange={(e) => setDisplayName(e.target.value)} style={inputStyle} placeholder="OpenAI Primary" />
        </Field>
      </div>
      <Field label="Base URL">
        <input value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} style={monoInputStyle} placeholder="https://api.openai.com/v1" />
      </Field>
      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: "0 12px" }}>
        <Field label="Region">
          <input value={region} onChange={(e) => setRegion(e.target.value)} style={monoInputStyle} placeholder="us-east" />
        </Field>
        <Field label="Credential scope">
          <input value={credentialScope} onChange={(e) => setCredentialScope(e.target.value)} style={monoInputStyle} placeholder="org-scoped" />
        </Field>
      </div>
      <Field
        label="Secret"
        hint="Write-only: stored in secret custody, cannot be read back. Only the credential version is visible afterwards."
      >
        <input
          type="password"
          value={secret}
          onChange={(e) => setSecret(e.target.value)}
          style={monoInputStyle}
          placeholder="sk-…"
          autoComplete="new-password"
        />
      </Field>
      <div style={{ marginBottom: 12 }}>
        <Button onClick={() => setShowCaps((v) => !v)}>
          {showCaps ? "Hide capability declaration" : "Capability declaration (advanced)"}
        </Button>
        {showCaps && (
          <textarea
            value={capsJson}
            onChange={(e) => setCapsJson(e.target.value)}
            rows={5}
            placeholder='{"revision":1,"features":{"text":"native","streaming":"native","moderation":"translated"}}'
            style={{ ...monoInputStyle, resize: "vertical", marginTop: 8 }}
          />
        )}
      </div>
      <Field label="Reason (required)">
        <textarea
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          rows={2}
          placeholder="Why is this connection being registered?"
          style={{ ...inputStyle, resize: "vertical" }}
        />
      </Field>
    </Modal>
  );
}

/* ------------------------------------------------------------------ */
/* List                                                                */
/* ------------------------------------------------------------------ */

function ConnectionList() {
  const { can } = useAuth();
  const canWrite = can(PERMISSIONS.providersWrite);
  const [providerFilter, setProviderFilter] = useState("");
  const [regionFilter, setRegionFilter] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [registerOpen, setRegisterOpen] = useState(false);

  const path =
    "/control/v1/provider-connections?limit=25" +
    (providerFilter ? `&provider=${encodeURIComponent(providerFilter)}` : "") +
    (regionFilter ? `&region=${encodeURIComponent(regionFilter)}` : "") +
    (statusFilter ? `&status=${encodeURIComponent(statusFilter)}` : "");
  const list = usePagedList<ProviderConnection>(path);

  return (
    <div style={{ padding: "20px 24px", maxWidth: 1360, margin: "0 auto" }}>
      <div style={{ display: "flex", alignItems: "flex-start", gap: 16, marginBottom: 14, flexWrap: "wrap" }}>
        <div style={{ flex: 1 }}>
          <h1 style={{ margin: 0, fontSize: 18, fontWeight: 600 }}>Provider Connections</h1>
          <div style={{ color: "var(--ink2)", marginTop: 2, fontSize: 12 }}>
            Upstream AI provider configurations. <b>Administrative status</b> (what you enabled) is independent of
            observed health.
          </div>
        </div>
        <Button
          variant="primary"
          disabled={!canWrite}
          title={canWrite ? undefined : "Requires platform:providers:write"}
          onClick={() => setRegisterOpen(true)}
        >
          Register connection
        </Button>
      </div>
      <Card>
        <div style={{ display: "flex", gap: 10, alignItems: "center", marginBottom: 12, flexWrap: "wrap" }}>
          <input
            value={providerFilter}
            onChange={(e) => setProviderFilter(e.target.value)}
            placeholder="Provider…"
            style={{ ...inputStyle, width: 180 }}
            aria-label="Filter by provider"
          />
          <input
            value={regionFilter}
            onChange={(e) => setRegionFilter(e.target.value)}
            placeholder="Region…"
            style={{ ...inputStyle, width: 160 }}
            aria-label="Filter by region"
          />
          <select
            value={statusFilter}
            onChange={(e) => setStatusFilter(e.target.value)}
            style={{ ...inputStyle, width: "auto" }}
            aria-label="Filter by status"
          >
            <option value="">All statuses</option>
            <option value="enabled">Enabled</option>
            <option value="disabled">Disabled</option>
          </select>
        </div>
        {list.loading ? (
          <Loading />
        ) : list.error ? (
          <ErrorBanner error={list.error} retry={list.reload} />
        ) : list.items.length === 0 ? (
          <EmptyState
            title="No provider connections"
            hint="Register one to route traffic to an upstream provider"
            action={
              <Button
                variant="primary"
                disabled={!canWrite}
                title={canWrite ? undefined : "Requires platform:providers:write"}
                onClick={() => setRegisterOpen(true)}
              >
                Register connection
              </Button>
            }
          />
        ) : (
          <>
            <Table>
              <thead>
                <tr>
                  <Th>Connection</Th>
                  <Th>Provider</Th>
                  <Th>Base URL</Th>
                  <Th>Region</Th>
                  <Th>Credential scope</Th>
                  <Th>Admin status</Th>
                  <Th>Credential</Th>
                  <Th>Revision</Th>
                </tr>
              </thead>
              <tbody>
                {list.items.map((conn) => (
                  <tr
                    key={conn.id}
                    onClick={() => navigate(`providers/${encodeURIComponent(conn.id)}`)}
                    style={{ cursor: "pointer" }}
                  >
                    <Td>
                      <div style={{ fontWeight: 600 }}>{conn.display_name}</div>
                      <div style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--ink3)" }}>
                        {conn.id}
                      </div>
                    </Td>
                    <Td>
                      <Badge tone="purple">{conn.provider}</Badge>
                    </Td>
                    <Td mono>{conn.base_url}</Td>
                    <Td mono>{conn.region}</Td>
                    <Td mono>{conn.credential_scope}</Td>
                    <Td>
                      <Badge tone={statusTone(conn.administrative_status)}>{conn.administrative_status}</Badge>
                    </Td>
                    <Td mono>v{conn.credential_version}</Td>
                    <Td mono>{conn.revision}</Td>
                  </tr>
                ))}
              </tbody>
            </Table>
            <div style={{ display: "flex", alignItems: "center", gap: 8, marginTop: 10, fontSize: 11, color: "var(--ink3)" }}>
              Showing {list.items.length} connection{list.items.length === 1 ? "" : "s"}
              <span style={{ flex: 1 }} />
              {list.nextCursor && (
                <Button onClick={list.loadMore} disabled={list.loadingMore}>
                  {list.loadingMore ? "Loading…" : "Load more"}
                </Button>
              )}
            </div>
          </>
        )}
      </Card>
      <div style={{ marginTop: 10, fontSize: 12, color: "var(--ink2)", display: "flex", gap: 16, flexWrap: "wrap" }}>
        <span>· Enabled does not imply healthy</span>
        <span>· Disabled is not a provider failure</span>
        <span>· Credentials are write-only — only the version number is visible</span>
      </div>
      <RegisterConnectionModal open={registerOpen} onClose={() => setRegisterOpen(false)} onRegistered={list.reload} />
    </div>
  );
}

export default function ProvidersPage() {
  const tail = useHashTail();
  const connectionId = tail[0];
  if (connectionId) return <ProviderDetail connectionId={connectionId} />;
  return <ConnectionList />;
}
