/**
 * Gateway API Keys sub-resource of a tenant: list, issue (show-once secret),
 * per-key drawer with overview edit / revoke / rotate, and policy tabs.
 */
import { useEffect, useState } from "react";
import { apiSend } from "../../api/client";
import { Drawer, ErrorBanner, Loading, useToast } from "../../components/feedback";
import { Badge, Button, Card, CodeBlock, EmptyState, KeyValueList, Table, Tabs, Td, Th } from "../../components/ui";
import { useApi } from "../../api/useApi";
import { formatDateTime, timeAgo, truncateId } from "../../lib/format";
import {
  Field,
  inputStyle,
  monoInputStyle,
  parseJsonObject,
  parseStringList,
  ReasonModal,
  SecretPanel,
  statusTone,
  toastMutationError,
  usePagedList,
} from "./common";
import {
  EffectivePolicyPanel,
  limitsToStrings,
  PolicyRevisionsTab,
  QuotaLimitsEditor,
  QuotaSnapshotPanel,
  stringsToLimits,
  type LimitStrings,
} from "./policy";
import type { APIKeyPolicy, GatewayKey, IssuedKey, KeyEffectivePolicy, RotateKeyResult } from "./types";

/* ------------------------------------------------------------------ */
/* Issue key modal                                                     */
/* ------------------------------------------------------------------ */

function IssueKeyModal({
  tenantId,
  open,
  onClose,
  onIssued,
}: {
  tenantId: string;
  open: boolean;
  onClose: () => void;
  onIssued: (issued: IssuedKey) => void;
}) {
  const toast = useToast();
  const [name, setName] = useState("");
  const [metadata, setMetadata] = useState("");
  const [expiresAt, setExpiresAt] = useState("");
  const [showPolicy, setShowPolicy] = useState(false);
  const [policyJson, setPolicyJson] = useState("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (open) {
      setName("");
      setMetadata("");
      setExpiresAt("");
      setShowPolicy(false);
      setPolicyJson("");
      setReason("");
    }
  }, [open]);

  const submit = async () => {
    const meta = parseJsonObject(metadata);
    if (meta.error) {
      toast(`Metadata: ${meta.error}`, "error");
      return;
    }
    const pol = parseJsonObject(policyJson);
    if (pol.error) {
      toast(`Policy: ${pol.error}`, "error");
      return;
    }
    setBusy(true);
    try {
      const issued = await apiSend<IssuedKey>(
        "POST",
        `/control/v1/tenants/${encodeURIComponent(tenantId)}/gateway-api-keys`,
        {
          name,
          ...(meta.value ? { metadata: meta.value } : {}),
          ...(expiresAt ? { expires_at: new Date(expiresAt).toISOString() } : {}),
          ...(pol.value ? { policy: pol.value } : {}),
          reason,
        },
      );
      toast("API key issued", "success");
      onIssued(issued);
      onClose();
    } catch (err) {
      toastMutationError(toast, err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Drawer open={open} onClose={onClose} title="Issue Gateway API Key" width={460}>
      <Field label="Name">
        <input value={name} onChange={(e) => setName(e.target.value)} style={inputStyle} placeholder="batch-worker" />
      </Field>
      <Field label="Expires at (optional)">
        <input
          type="datetime-local"
          value={expiresAt}
          onChange={(e) => setExpiresAt(e.target.value)}
          style={inputStyle}
        />
      </Field>
      <Field label="Metadata (optional JSON object)">
        <textarea
          value={metadata}
          onChange={(e) => setMetadata(e.target.value)}
          rows={3}
          placeholder='{"team":"ml"}'
          style={{ ...monoInputStyle, resize: "vertical" }}
        />
      </Field>
      <div style={{ marginBottom: 12 }}>
        <Button onClick={() => setShowPolicy((v) => !v)}>
          {showPolicy ? "Hide initial policy" : "Initial policy (advanced)"}
        </Button>
        {showPolicy && (
          <textarea
            value={policyJson}
            onChange={(e) => setPolicyJson(e.target.value)}
            rows={6}
            placeholder='{"allowed_operations":["responses.create"],"limits":{"requests_per_minute":60}}'
            style={{ ...monoInputStyle, resize: "vertical", marginTop: 8 }}
          />
        )}
      </div>
      <Field label="Reason (required)">
        <textarea
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          rows={3}
          placeholder="Why is this key needed?"
          style={{ ...inputStyle, resize: "vertical" }}
        />
      </Field>
      <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
        <Button onClick={onClose}>Cancel</Button>
        <Button
          variant="primary"
          disabled={!name.trim() || !reason.trim() || busy}
          onClick={() => void submit()}
        >
          Issue key
        </Button>
      </div>
    </Drawer>
  );
}

/* ------------------------------------------------------------------ */
/* Key policy form                                                     */
/* ------------------------------------------------------------------ */

interface KeyPolicyFormState {
  allow_cache_protection: boolean;
  allow_content_inspection: boolean;
  allowed_public_models: string;
  allowed_operations: string;
  allowed_cidrs: string;
  allowed_regions: string;
  max_concurrent_responses: string;
  limits: LimitStrings;
}

function keyPolicyToForm(policy: APIKeyPolicy): KeyPolicyFormState {
  const list = (v: string[] | null | undefined) => (v ?? []).join("\n");
  return {
    allow_cache_protection: policy.allow_cache_protection ?? false,
    allow_content_inspection: policy.allow_content_inspection ?? false,
    allowed_public_models: list(policy.allowed_public_models),
    allowed_operations: list(policy.allowed_operations),
    allowed_cidrs: list(policy.allowed_cidrs),
    allowed_regions: list(policy.allowed_regions),
    max_concurrent_responses:
      policy.max_concurrent_responses === undefined || policy.max_concurrent_responses === null
        ? ""
        : String(policy.max_concurrent_responses),
    limits: limitsToStrings(policy.limits),
  };
}

function ListTextarea({
  label,
  hint,
  value,
  onChange,
}: {
  label: string;
  hint: string;
  value: string;
  onChange: (v: string) => void;
}) {
  return (
    <Field label={label} hint={hint}>
      <textarea
        value={value}
        onChange={(e) => onChange(e.target.value)}
        rows={3}
        style={{ ...monoInputStyle, resize: "vertical" }}
      />
    </Field>
  );
}

function KeyPolicyTab({ tenantId, keyId }: { tenantId: string; keyId: string }) {
  const toast = useToast();
  const path = `/control/v1/tenants/${encodeURIComponent(tenantId)}/gateway-api-keys/${encodeURIComponent(keyId)}/policy`;
  const { data, error, loading, retry } = useApi<APIKeyPolicy>(path);
  const [form, setForm] = useState<KeyPolicyFormState | null>(null);
  const [dirty, setDirty] = useState(false);
  const [reason, setReason] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (data) {
      setForm(keyPolicyToForm(data));
      setDirty(false);
    }
  }, [data]);

  if (error) return <ErrorBanner error={error} retry={retry} />;
  if (loading || !form) return <Loading />;
  if (!data) return <EmptyState title="No policy" />;

  const update = (patch: Partial<KeyPolicyFormState>) => {
    setForm((current) => (current ? { ...current, ...patch } : current));
    setDirty(true);
  };

  const save = async () => {
    const policy: APIKeyPolicy = {
      allow_cache_protection: form.allow_cache_protection,
      allow_content_inspection: form.allow_content_inspection,
    };
    const models = parseStringList(form.allowed_public_models);
    const operations = parseStringList(form.allowed_operations);
    const cidrs = parseStringList(form.allowed_cidrs);
    const regions = parseStringList(form.allowed_regions);
    if (models) policy.allowed_public_models = models;
    if (operations) policy.allowed_operations = operations;
    if (cidrs) policy.allowed_cidrs = cidrs;
    if (regions) policy.allowed_regions = regions;
    const maxConcurrent = form.max_concurrent_responses.trim();
    if (maxConcurrent) {
      if (!/^-?\d+$/.test(maxConcurrent)) {
        toast("Max concurrent responses must be an integer", "error");
        return;
      }
      policy.max_concurrent_responses = Number(maxConcurrent);
    }
    const limitsResult = stringsToLimits(form.limits);
    if (limitsResult.error) {
      toast(limitsResult.error, "error");
      return;
    }
    if (limitsResult.limits) policy.limits = limitsResult.limits;

    setSaving(true);
    try {
      // The service requires the published policy revision to advance by one.
      policy.revision = (data.revision ?? 0) + 1;
      await apiSend("PUT", path, { policy, expected_revision: data.revision ?? 0, reason });
      toast("Key policy saved", "success");
      setReason("");
      retry();
    } catch (err) {
      toastMutationError(toast, err, retry);
    } finally {
      setSaving(false);
    }
  };

  const checkboxRow = (
    label: string,
    key: "allow_cache_protection" | "allow_content_inspection",
    hint: string,
  ) => (
    <label
      style={{ display: "flex", alignItems: "flex-start", gap: 8, fontSize: 12, color: "var(--ink2)", marginBottom: 10 }}
      title={hint}
    >
      <input
        type="checkbox"
        checked={form[key]}
        onChange={(e) => update({ [key]: e.target.checked } as Partial<KeyPolicyFormState>)}
        style={{ marginTop: 2 }}
      />
      <span>
        <span style={{ fontWeight: 600, color: "var(--ink)" }}>{label}</span>
        <div style={{ fontSize: 11, color: "var(--ink3)" }}>{hint}</div>
      </span>
    </label>
  );

  return (
    <Card
      title={
        <span>
          Key policy{" "}
          <span style={{ fontFamily: "var(--font-mono)", color: "var(--purple)", fontWeight: 500 }}>
            rev {data.revision ?? 0}
          </span>
          {dirty && (
            <Badge tone="amber">
              <span style={{ marginLeft: 8 }}>Unsaved changes</span>
            </Badge>
          )}
        </span>
      }
    >
      {checkboxRow("Allow cache protection", "allow_cache_protection", "Permit cache-protection reservation on requests signed with this key.")}
      {checkboxRow("Allow content inspection", "allow_content_inspection", "Permit content inspection where cache protection requires it.")}
      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: "0 14px" }}>
        <ListTextarea
          label="Allowed public models"
          hint="One per line (or comma-separated). Empty = no restriction."
          value={form.allowed_public_models}
          onChange={(v) => update({ allowed_public_models: v })}
        />
        <ListTextarea
          label="Allowed operations"
          hint="One per line (or comma-separated). Empty = no restriction."
          value={form.allowed_operations}
          onChange={(v) => update({ allowed_operations: v })}
        />
        <ListTextarea
          label="Allowed CIDRs"
          hint="One per line (or comma-separated). Empty = no restriction."
          value={form.allowed_cidrs}
          onChange={(v) => update({ allowed_cidrs: v })}
        />
        <ListTextarea
          label="Allowed regions"
          hint="One per line (or comma-separated). Empty = no restriction."
          value={form.allowed_regions}
          onChange={(v) => update({ allowed_regions: v })}
        />
      </div>
      <Field label="Max concurrent responses" hint="Empty = inherit tenant value. 0 = explicit deny.">
        <input
          value={form.max_concurrent_responses}
          onChange={(e) => update({ max_concurrent_responses: e.target.value })}
          placeholder="inherit"
          inputMode="numeric"
          style={{ ...monoInputStyle, maxWidth: 220 }}
        />
      </Field>
      <div style={{ fontSize: 12, fontWeight: 600, margin: "4px 0 8px" }}>Quota limits</div>
      <QuotaLimitsEditor value={form.limits} onChange={(limits) => update({ limits })} />
      <div
        style={{
          display: "flex",
          alignItems: "center",
          gap: 8,
          marginTop: 14,
          paddingTop: 12,
          borderTop: "1px solid var(--line)",
        }}
      >
        <input
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          placeholder="Reason for change (required)"
          style={{ ...inputStyle, flex: 1 }}
        />
        <Button onClick={() => setForm(keyPolicyToForm(data))} disabled={!dirty || saving}>
          Discard
        </Button>
        <Button variant="primary" disabled={!dirty || !reason.trim() || saving} onClick={() => void save()}>
          Save policy
        </Button>
      </div>
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/* Key detail drawer                                                   */
/* ------------------------------------------------------------------ */

const KEY_TABS = ["Overview", "Policy", "Policy Revisions", "Effective Policy", "Quota"];

function KeyDetailDrawer({
  tenantId,
  keyId,
  onClose,
  onChanged,
}: {
  tenantId: string;
  keyId: string;
  onClose: () => void;
  onChanged: () => void;
}) {
  const toast = useToast();
  const base = `/control/v1/tenants/${encodeURIComponent(tenantId)}/gateway-api-keys/${encodeURIComponent(keyId)}`;
  const { data: key, error, loading, retry } = useApi<GatewayKey>(base);
  const effective = useApi<KeyEffectivePolicy>(`${base}/effective-policy`);
  const [tab, setTab] = useState("Overview");

  const [editName, setEditName] = useState("");
  const [editMetadata, setEditMetadata] = useState("");
  const [editExpiresAt, setEditExpiresAt] = useState("");
  const [clearExpires, setClearExpires] = useState(false);
  const [editReason, setEditReason] = useState("");
  const [saving, setSaving] = useState(false);

  const [revokeOpen, setRevokeOpen] = useState(false);
  const [rotateOpen, setRotateOpen] = useState(false);
  const [revokeImmediately, setRevokeImmediately] = useState(false);
  const [graceExpiresAt, setGraceExpiresAt] = useState("");
  const [actionBusy, setActionBusy] = useState(false);
  const [rotatedSecret, setRotatedSecret] = useState<string | null>(null);

  useEffect(() => {
    if (key) {
      setEditName(key.name);
      setEditMetadata(key.metadata ? JSON.stringify(key.metadata, null, 2) : "");
      setEditExpiresAt(key.expires_at ? key.expires_at.slice(0, 16) : "");
      setClearExpires(false);
    }
  }, [key]);

  const save = async () => {
    if (!key) return;
    const meta = parseJsonObject(editMetadata);
    if (meta.error) {
      toast(`Metadata: ${meta.error}`, "error");
      return;
    }
    setSaving(true);
    try {
      await apiSend("PATCH", base, {
        name: editName,
        ...(meta.value ? { metadata: meta.value } : {}),
        ...(clearExpires
          ? { clear_expires_at: true }
          : editExpiresAt
            ? { expires_at: new Date(editExpiresAt).toISOString() }
            : {}),
        expected_revision: key.revision,
        reason: editReason,
      });
      toast("API key updated", "success");
      setEditReason("");
      retry();
      onChanged();
    } catch (err) {
      toastMutationError(toast, err, retry);
    } finally {
      setSaving(false);
    }
  };

  const revoke = async (reason: string) => {
    if (!key) return;
    setActionBusy(true);
    try {
      await apiSend("POST", `${base}/revoke`, { expected_revision: key.revision, reason });
      toast("API key revoked", "success");
      setRevokeOpen(false);
      retry();
      onChanged();
    } catch (err) {
      toastMutationError(toast, err, retry);
    } finally {
      setActionBusy(false);
    }
  };

  const rotate = async (reason: string) => {
    if (!key) return;
    setActionBusy(true);
    try {
      const result = await apiSend<RotateKeyResult>("POST", `${base}/rotate`, {
        expected_revision: key.revision,
        revoke_immediately: revokeImmediately,
        ...(!revokeImmediately && graceExpiresAt
          ? { grace_expires_at: new Date(graceExpiresAt).toISOString() }
          : {}),
        reason,
      });
      toast("API key rotated", "success");
      setRotateOpen(false);
      if (result.secret) setRotatedSecret(result.secret);
      retry();
      onChanged();
    } catch (err) {
      toastMutationError(toast, err, retry);
    } finally {
      setActionBusy(false);
    }
  };

  return (
    <Drawer open onClose={onClose} title={key ? `API key — ${key.name}` : "API key"} width={640}>
      {loading ? (
        <Loading />
      ) : error ? (
        <ErrorBanner error={error} retry={retry} />
      ) : !key ? (
        <EmptyState title="Key not found" />
      ) : (
        <>
          {rotatedSecret && (
            <SecretPanel
              secret={rotatedSecret}
              heading="Replacement key secret"
              onClose={() => setRotatedSecret(null)}
            />
          )}
          <Tabs tabs={KEY_TABS} active={tab} onChange={setTab} />
          {tab === "Overview" && (
            <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
              <Card>
                <KeyValueList
                  items={[
                    { key: "ID", value: key.id, mono: true },
                    { key: "Prefix", value: key.prefix, mono: true },
                    {
                      key: "Status",
                      value: <Badge tone={statusTone(key.status)}>{key.status}</Badge>,
                    },
                    { key: "Digest version", value: key.digest_version, mono: true },
                    { key: "Revision", value: String(key.revision), mono: true },
                    { key: "Policy revision", value: String(key.policy?.revision ?? 0), mono: true },
                    { key: "Expires at", value: key.expires_at ? formatDateTime(key.expires_at) : "—", mono: true },
                    { key: "Revoked at", value: key.revoked_at ? formatDateTime(key.revoked_at) : "—", mono: true },
                    {
                      key: "Grace expires",
                      value: key.grace_expires_at ? formatDateTime(key.grace_expires_at) : "—",
                      mono: true,
                    },
                    { key: "Predecessor", value: key.predecessor_id ?? "—", mono: true },
                    { key: "Replacement", value: key.replacement_id ?? "—", mono: true },
                  ]}
                />
                {key.metadata && (
                  <div style={{ marginTop: 12 }}>
                    <CodeBlock code={JSON.stringify(key.metadata, null, 2)} lang="json" />
                  </div>
                )}
              </Card>
              {key.status === "active" && (
                <Card title="Edit key">
                  <Field label="Name">
                    <input value={editName} onChange={(e) => setEditName(e.target.value)} style={inputStyle} />
                  </Field>
                  <Field label="Metadata (JSON object)">
                    <textarea
                      value={editMetadata}
                      onChange={(e) => setEditMetadata(e.target.value)}
                      rows={3}
                      style={{ ...monoInputStyle, resize: "vertical" }}
                    />
                  </Field>
                  <Field label="Expires at">
                    <div style={{ display: "flex", gap: 10, alignItems: "center" }}>
                      <input
                        type="datetime-local"
                        value={editExpiresAt}
                        onChange={(e) => {
                          setEditExpiresAt(e.target.value);
                          setClearExpires(false);
                        }}
                        disabled={clearExpires}
                        style={{ ...inputStyle, flex: 1 }}
                      />
                      <label style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 12, color: "var(--ink2)" }}>
                        <input
                          type="checkbox"
                          checked={clearExpires}
                          onChange={(e) => setClearExpires(e.target.checked)}
                        />
                        Clear expiry
                      </label>
                    </div>
                  </Field>
                  <Field label="Reason (required)">
                    <textarea
                      value={editReason}
                      onChange={(e) => setEditReason(e.target.value)}
                      rows={2}
                      style={{ ...inputStyle, resize: "vertical" }}
                    />
                  </Field>
                  <div style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
                    <Button variant="primary" disabled={!editReason.trim() || saving} onClick={() => void save()}>
                      Save changes
                    </Button>
                  </div>
                </Card>
              )}
              <Card title="Lifecycle">
                <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
                  {key.status === "active" && (
                    <>
                      <Button onClick={() => setRotateOpen(true)}>Rotate…</Button>
                      <Button variant="danger" onClick={() => setRevokeOpen(true)}>
                        Revoke…
                      </Button>
                    </>
                  )}
                  {key.status !== "active" && (
                    <span style={{ fontSize: 12, color: "var(--ink3)" }}>
                      Revoked keys cannot be rotated or edited.
                    </span>
                  )}
                </div>
              </Card>
            </div>
          )}
          {tab === "Policy" && <KeyPolicyTab tenantId={tenantId} keyId={keyId} />}
          {tab === "Policy Revisions" && <PolicyRevisionsTab policyPath={`${base}/policy`} revisionsPath={`${base}/policy-revisions`} />}
          {tab === "Effective Policy" && (
            <EffectivePolicyPanel
              state={effective}
              render={(ep) => <CodeBlock code={JSON.stringify(ep, null, 2)} lang="json" />}
            />
          )}
          {tab === "Quota" && <QuotaSnapshotPanel path={`${base}/quota-snapshot`} />}
          <ReasonModal
            open={revokeOpen}
            onClose={() => setRevokeOpen(false)}
            onConfirm={(reason) => void revoke(reason)}
            title={`Revoke key ${key.name}`}
            description="Revocation is immediate — in-flight authentication with this secret starts failing as soon as the projection applies. This cannot be undone."
            confirmLabel="Revoke"
            danger
            busy={actionBusy}
          />
          <ReasonModal
            open={rotateOpen}
            onClose={() => setRotateOpen(false)}
            onConfirm={(reason) => void rotate(reason)}
            title={`Rotate key ${key.name}`}
            description="Issues a replacement secret (shown exactly once). Unless revoked immediately, the old secret stays valid through the grace period."
            confirmLabel="Rotate"
            busy={actionBusy}
          >
            <label
              style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 12, color: "var(--ink2)", marginBottom: 12 }}
            >
              <input
                type="checkbox"
                checked={revokeImmediately}
                onChange={(e) => setRevokeImmediately(e.target.checked)}
              />
              Revoke the old secret immediately (no grace period)
            </label>
            {!revokeImmediately && (
              <Field label="Grace expires at (optional)">
                <input
                  type="datetime-local"
                  value={graceExpiresAt}
                  onChange={(e) => setGraceExpiresAt(e.target.value)}
                  style={inputStyle}
                />
              </Field>
            )}
          </ReasonModal>
        </>
      )}
    </Drawer>
  );
}

/* ------------------------------------------------------------------ */
/* Keys tab (list)                                                     */
/* ------------------------------------------------------------------ */

export function GatewayKeysTab({ tenantId }: { tenantId: string }) {
  const [statusFilter, setStatusFilter] = useState("");
  const listPath =
    `/control/v1/tenants/${encodeURIComponent(tenantId)}/gateway-api-keys?limit=25` +
    (statusFilter ? `&status=${encodeURIComponent(statusFilter)}` : "");
  const list = usePagedList<GatewayKey>(listPath);
  const [issueOpen, setIssueOpen] = useState(false);
  const [selectedKeyId, setSelectedKeyId] = useState<string | null>(null);
  const [issuedSecret, setIssuedSecret] = useState<string | null>(null);

  return (
    <>
      {issuedSecret && (
        <SecretPanel
          secret={issuedSecret}
          heading="New API key secret"
          onClose={() => setIssuedSecret(null)}
        />
      )}
      <Card
        title="Gateway API Keys"
        actions={
          <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
            <select
              value={statusFilter}
              onChange={(e) => setStatusFilter(e.target.value)}
              style={{ ...inputStyle, width: "auto" }}
              aria-label="Filter by status"
            >
              <option value="">All statuses</option>
              <option value="active">Active</option>
              <option value="revoked">Revoked</option>
            </select>
            <Button variant="primary" onClick={() => setIssueOpen(true)}>
              Issue key…
            </Button>
          </div>
        }
      >
        {list.loading ? (
          <Loading />
        ) : list.error ? (
          <ErrorBanner error={list.error} retry={list.reload} />
        ) : list.items.length === 0 ? (
          <EmptyState
            title="No API keys"
            hint="Issue a Gateway API key to let tenant workloads call the gateway"
            action={<Button variant="primary" onClick={() => setIssueOpen(true)}>Issue key…</Button>}
          />
        ) : (
          <>
            <Table>
              <thead>
                <tr>
                  <Th>Name</Th>
                  <Th>Prefix</Th>
                  <Th>Status</Th>
                  <Th>Policy rev</Th>
                  <Th>Expires</Th>
                  <Th>Grace</Th>
                  <Th>Revision</Th>
                </tr>
              </thead>
              <tbody>
                {list.items.map((key) => (
                  <tr
                    key={key.id}
                    onClick={() => setSelectedKeyId(key.id)}
                    style={{ cursor: "pointer" }}
                    title={`Open ${key.id}`}
                  >
                    <Td>
                      <div style={{ fontWeight: 600 }}>{key.name}</div>
                      <div style={{ fontFamily: "var(--font-mono)", fontSize: 10.5, color: "var(--ink3)" }}>
                        {truncateId(key.id)}
                      </div>
                    </Td>
                    <Td mono>{key.prefix}</Td>
                    <Td>
                      <Badge tone={statusTone(key.status)}>{key.status}</Badge>
                    </Td>
                    <Td mono>
                      <span style={{ color: "var(--purple)" }}>{key.policy?.revision ?? 0}</span>
                    </Td>
                    <Td mono>{key.expires_at ? timeAgo(key.expires_at) : "—"}</Td>
                    <Td mono>{key.grace_expires_at ? timeAgo(key.grace_expires_at) : "—"}</Td>
                    <Td mono>{key.revision}</Td>
                  </tr>
                ))}
              </tbody>
            </Table>
            <div style={{ display: "flex", alignItems: "center", gap: 8, marginTop: 10, fontSize: 11, color: "var(--ink3)" }}>
              Showing {list.items.length} key{list.items.length === 1 ? "" : "s"}
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
      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 14, marginTop: 14 }}>
        <Card title="Secret handling" style={{ fontSize: 12, color: "var(--ink2)" }}>
          The raw key is shown <b>exactly once</b> after issue or rotation. It is never stored, logged, or displayed
          again — idempotent replays return metadata only. Rotation can keep the old secret valid through a grace
          period; revocation is immediate.
        </Card>
        <Card title="Effective policy" style={{ fontSize: 12, color: "var(--ink2)" }}>
          A key’s own policy intersects the tenant policy — the <b>most restrictive value wins</b> per limit. A key
          can never exceed its tenant.
        </Card>
      </div>
      <IssueKeyModal
        tenantId={tenantId}
        open={issueOpen}
        onClose={() => setIssueOpen(false)}
        onIssued={(issued) => {
          if (issued.secret) setIssuedSecret(issued.secret);
          list.reload();
        }}
      />
      {selectedKeyId && (
        <KeyDetailDrawer
          tenantId={tenantId}
          keyId={selectedKeyId}
          onClose={() => setSelectedKeyId(null)}
          onChanged={list.reload}
        />
      )}
    </>
  );
}
