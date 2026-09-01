/**
 * Tenant Limit Policy tab, policy-revision history (tenant + key), and the
 * quota-snapshot panel. Shared by the tenant detail and the key drawer.
 */
import { useEffect, useState, type ReactNode } from "react";
import { apiSend } from "../../api/client";
import { ErrorBanner, Loading, useToast } from "../../components/feedback";
import { Badge, Button, Card, CodeBlock, EmptyState, Table, Td, Th } from "../../components/ui";
import { useApi } from "../../api/useApi";
import { formatDateTime, formatMicrosUSD, formatNumber } from "../../lib/format";
import {
  Field,
  inputStyle,
  labelStyle,
  monoInputStyle,
  ReasonModal,
  statusTone,
  toastMutationError,
  usePagedList,
} from "./common";
import type {
  APIKeyPolicy,
  KeyPolicyRevision,
  QuotaBalance,
  QuotaLimits,
  QuotaSnapshot,
  TenantEffectivePolicy,
  TenantPolicy,
  TenantPolicyRevision,
} from "./types";

/* ------------------------------------------------------------------ */
/* Quota limits editor                                                 */
/* ------------------------------------------------------------------ */

interface QuotaField {
  key: keyof QuotaLimits;
  label: string;
  micros?: boolean;
}

const QUOTA_FIELDS: QuotaField[] = [
  { key: "max_input_tokens", label: "Max input tokens" },
  { key: "max_output_tokens", label: "Max output tokens" },
  { key: "max_cost_micros", label: "Max cost (micros USD)", micros: true },
  { key: "requests_per_minute", label: "Requests / minute" },
  { key: "tokens_per_minute", label: "Tokens / minute" },
  { key: "daily_spend_micros", label: "Daily spend (micros USD)", micros: true },
  { key: "monthly_spend_micros", label: "Monthly spend (micros USD)", micros: true },
  { key: "refresh_daily_spend_micros", label: "Refresh daily spend (micros USD)", micros: true },
  { key: "refresh_monthly_spend_micros", label: "Refresh monthly spend (micros USD)", micros: true },
  { key: "embedding_input_units", label: "Embedding input units" },
  { key: "rerank_documents", label: "Rerank documents" },
  { key: "capability_spend_micros", label: "Capability spend (micros USD)", micros: true },
  { key: "currency", label: "Currency" },
];

export type LimitStrings = Record<string, string>;

export function limitsToStrings(limits: QuotaLimits | undefined): LimitStrings {
  const out: LimitStrings = {};
  for (const field of QUOTA_FIELDS) {
    const value = limits?.[field.key];
    out[field.key] = value === undefined || value === null ? "" : String(value);
  }
  return out;
}

/** Empty string = field omitted (inherit); integer string = explicit value. */
export function stringsToLimits(strings: LimitStrings): { limits?: QuotaLimits; error?: string } {
  const limits: QuotaLimits = {};
  let any = false;
  for (const field of QUOTA_FIELDS) {
    const raw = (strings[field.key] ?? "").trim();
    if (!raw) continue;
    if (field.key === "currency") {
      limits.currency = raw;
      any = true;
      continue;
    }
    if (!/^-?\d+$/.test(raw)) return { error: `${field.label} must be an integer` };
    const value = Number(raw);
    if (!Number.isSafeInteger(value)) return { error: `${field.label} is out of range` };
    (limits as Record<string, unknown>)[field.key] = value;
    any = true;
  }
  return any ? { limits } : {};
}

export function QuotaLimitsEditor({
  value,
  onChange,
}: {
  value: LimitStrings;
  onChange: (next: LimitStrings) => void;
}) {
  return (
    <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit,minmax(230px,1fr))", gap: "10px 14px" }}>
      {QUOTA_FIELDS.map((field) => {
        const raw = value[field.key] ?? "";
        const numeric = /^-?\d+$/.test(raw.trim());
        return (
          <div key={field.key}>
            <label style={labelStyle}>{field.label}</label>
            <input
              value={raw}
              onChange={(e) => onChange({ ...value, [field.key]: e.target.value })}
              placeholder="inherit"
              inputMode={field.key === "currency" ? undefined : "numeric"}
              style={monoInputStyle}
            />
            {field.micros && numeric && raw.trim() !== "" && (
              <div style={{ fontSize: 11, color: "var(--ink3)", marginTop: 2 }}>
                = {formatMicrosUSD(Number(raw.trim()))}
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}

/* ------------------------------------------------------------------ */
/* Explicit-zero / inherit semantics hint                              */
/* ------------------------------------------------------------------ */

function PolicySemanticsHint() {
  return (
    <Card title="Policy semantics" style={{ fontSize: 12, color: "var(--ink2)" }}>
      <div style={{ display: "flex", flexDirection: "column", gap: 7 }}>
        <div>
          <Badge>Inherit</Badge> Empty field — the key is omitted from the request and the platform default applies.
        </div>
        <div>
          <Badge tone="red">Explicit 0</Badge> 0 = explicit deny/unset per policy semantics; it is not “unset”.
        </div>
        <div>
          <Badge tone="blue">Explicit</Badge> Set here. The <b>effective</b> policy for a request is the most
          restrictive of tenant and API-key values.
        </div>
      </div>
    </Card>
  );
}

function ConflictHint({ revision }: { revision?: number }) {
  return (
    <Card title="Revision conflicts" style={{ fontSize: 12, color: "var(--ink2)" }}>
      Saves send <code>expected_revision</code> on revision <b>{revision ?? "—"}</b>. If someone else saved first you
      get a 409 <code>revision_conflict</code> — the latest policy is reloaded, review their change, and re-apply
      yours. Nothing is overwritten silently.
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/* Tenant limit policy tab                                             */
/* ------------------------------------------------------------------ */

type TriState = "" | "true" | "false";

interface TenantPolicyForm {
  max_concurrent_responses: string;
  max_input_items: string;
  retention_seconds: string;
  allow_stored_responses: TriState;
  allow_cache_protection: TriState;
  allow_content_inspection: TriState;
  limits: LimitStrings;
}

function policyToForm(policy: TenantPolicy): TenantPolicyForm {
  const tri = (v: boolean | undefined): TriState => (v === undefined ? "" : v ? "true" : "false");
  return {
    max_concurrent_responses: policy.max_concurrent_responses ? String(policy.max_concurrent_responses) : "",
    max_input_items: policy.max_input_items ? String(policy.max_input_items) : "",
    retention_seconds: policy.retention_seconds ? String(policy.retention_seconds) : "",
    allow_stored_responses: tri(policy.allow_stored_responses),
    allow_cache_protection: tri(policy.allow_cache_protection),
    allow_content_inspection: tri(policy.allow_content_inspection),
    limits: limitsToStrings(policy.limits),
  };
}

function intField(raw: string, label: string): { value?: number; error?: string } {
  const trimmed = raw.trim();
  if (!trimmed) return {};
  if (!/^-?\d+$/.test(trimmed)) return { error: `${label} must be an integer` };
  const value = Number(trimmed);
  if (!Number.isSafeInteger(value)) return { error: `${label} is out of range` };
  return { value };
}

function TriStateSelect({
  label,
  value,
  onChange,
}: {
  label: string;
  value: TriState;
  onChange: (v: TriState) => void;
}) {
  return (
    <div>
      <label style={labelStyle}>{label}</label>
      <select value={value} onChange={(e) => onChange(e.target.value as TriState)} style={inputStyle}>
        <option value="">Inherit (field omitted)</option>
        <option value="true">Allow (true)</option>
        <option value="false">Deny (false)</option>
      </select>
    </div>
  );
}

export function TenantPolicyTab({ tenantId }: { tenantId: string }) {
  const toast = useToast();
  const path = `/control/v1/tenants/${encodeURIComponent(tenantId)}/policy`;
  const { data, error, loading, retry } = useApi<TenantPolicy>(path);
  const effective = useApi<TenantEffectivePolicy>(
    `/control/v1/tenants/${encodeURIComponent(tenantId)}/effective-policy`,
  );
  const [form, setForm] = useState<TenantPolicyForm | null>(null);
  const [dirty, setDirty] = useState(false);
  const [reason, setReason] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (data) {
      setForm(policyToForm(data));
      setDirty(false);
    }
  }, [data]);

  if (error) return <ErrorBanner error={error} retry={retry} />;
  if (loading || !form) return <Loading />;
  if (!data) return <EmptyState title="No policy" />;

  const update = (patch: Partial<TenantPolicyForm>) => {
    setForm((current) => (current ? { ...current, ...patch } : current));
    setDirty(true);
  };

  const save = async () => {
    const policy: TenantPolicy = {};
    for (const [key, label] of [
      ["max_concurrent_responses", "Max concurrent responses"],
      ["max_input_items", "Max input items"],
      ["retention_seconds", "Retention seconds"],
    ] as const) {
      const parsed = intField(form[key], label);
      if (parsed.error) {
        toast(parsed.error, "error");
        return;
      }
      if (parsed.value !== undefined) policy[key] = parsed.value;
    }
    if (form.allow_stored_responses) policy.allow_stored_responses = form.allow_stored_responses === "true";
    if (form.allow_cache_protection) policy.allow_cache_protection = form.allow_cache_protection === "true";
    if (form.allow_content_inspection) policy.allow_content_inspection = form.allow_content_inspection === "true";
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
      toast("Policy saved", "success");
      setReason("");
      retry();
      effective.retry();
    } catch (err) {
      toastMutationError(toast, err, retry);
    } finally {
      setSaving(false);
    }
  };

  return (
    <div
      style={{
        display: "grid",
        gridTemplateColumns: "minmax(0,1.8fr) minmax(0,1fr)",
        gap: 14,
        alignItems: "start",
      }}
    >
      <Card
        title={
          <span>
            Limit Policy{" "}
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
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit,minmax(200px,1fr))", gap: "10px 14px" }}>
          <Field label="Max concurrent responses">
            <input
              value={form.max_concurrent_responses}
              onChange={(e) => update({ max_concurrent_responses: e.target.value })}
              placeholder="inherit"
              inputMode="numeric"
              style={monoInputStyle}
            />
          </Field>
          <Field label="Max input items">
            <input
              value={form.max_input_items}
              onChange={(e) => update({ max_input_items: e.target.value })}
              placeholder="inherit"
              inputMode="numeric"
              style={monoInputStyle}
            />
          </Field>
          <Field label="Retention (seconds)">
            <input
              value={form.retention_seconds}
              onChange={(e) => update({ retention_seconds: e.target.value })}
              placeholder="inherit"
              inputMode="numeric"
              style={monoInputStyle}
            />
          </Field>
        </div>
        <div
          style={{
            display: "grid",
            gridTemplateColumns: "repeat(auto-fit,minmax(200px,1fr))",
            gap: "10px 14px",
            marginBottom: 14,
          }}
        >
          <TriStateSelect
            label="Allow stored responses"
            value={form.allow_stored_responses}
            onChange={(v) => update({ allow_stored_responses: v })}
          />
          <TriStateSelect
            label="Allow cache protection"
            value={form.allow_cache_protection}
            onChange={(v) => update({ allow_cache_protection: v })}
          />
          <TriStateSelect
            label="Allow content inspection"
            value={form.allow_content_inspection}
            onChange={(v) => update({ allow_content_inspection: v })}
          />
        </div>
        <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 8 }}>Quota limits</div>
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
          <Button onClick={() => setForm(policyToForm(data))} disabled={!dirty || saving}>
            Discard
          </Button>
          <Button variant="primary" disabled={!dirty || !reason.trim() || saving} onClick={() => void save()}>
            Save policy
          </Button>
        </div>
      </Card>
      <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        <PolicySemanticsHint />
        <ConflictHint revision={data.revision} />
        <EffectivePolicyPanel
          state={effective}
          render={(ep) => (
            <CodeBlock code={JSON.stringify({ tenant_policy: ep.tenant_policy, limits: ep.limits }, null, 2)} lang="json" />
          )}
        />
      </div>
    </div>
  );
}

/* ------------------------------------------------------------------ */
/* Effective policy (read-only)                                        */
/* ------------------------------------------------------------------ */

export function EffectivePolicyPanel<T>({
  state,
  render,
}: {
  state: { data: T | null; error: ReturnType<typeof useApi<T>>["error"]; loading: boolean; retry: () => void };
  render: (data: T) => ReactNode;
}) {
  return (
    <Card
      title="Effective policy (merged, read-only)"
      actions={<Button onClick={state.retry}>Refresh</Button>}
      style={{ fontSize: 12 }}
    >
      {state.loading ? (
        <Loading />
      ) : state.error ? (
        <ErrorBanner error={state.error} retry={state.retry} />
      ) : state.data ? (
        render(state.data)
      ) : (
        <EmptyState title="No effective policy" />
      )}
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/* Policy revisions (tenant + key share this)                          */
/* ------------------------------------------------------------------ */

type AnyRevision = TenantPolicyRevision | KeyPolicyRevision;

export function PolicyRevisionsTab({
  policyPath,
  revisionsPath,
}: {
  /** GET endpoint whose response carries the current policy `revision`. */
  policyPath: string;
  /** GET endpoint listing revisions; restore is PUT to policyPath. */
  revisionsPath: string;
}) {
  const toast = useToast();
  const list = usePagedList<AnyRevision>(`${revisionsPath}?limit=25`);
  const policy = useApi<TenantPolicy | APIKeyPolicy>(policyPath);
  const [restoreTarget, setRestoreTarget] = useState<AnyRevision | null>(null);
  const [viewing, setViewing] = useState<AnyRevision | null>(null);
  const [restoring, setRestoring] = useState(false);

  const restore = async (reason: string) => {
    if (!restoreTarget) return;
    setRestoring(true);
    try {
      await apiSend("PUT", policyPath, {
        restore_revision: restoreTarget.revision,
        expected_revision: policy.data?.revision ?? 0,
        reason,
      });
      toast(`Policy restored to revision ${restoreTarget.revision}`, "success");
      setRestoreTarget(null);
      list.reload();
      policy.retry();
    } catch (err) {
      toastMutationError(toast, err, () => {
        policy.retry();
        list.reload();
      });
    } finally {
      setRestoring(false);
    }
  };

  if (list.loading) return <Loading />;
  if (list.error) return <ErrorBanner error={list.error} retry={list.reload} />;
  if (list.items.length === 0) return <EmptyState title="No policy revisions" hint="Publish a policy to create the first revision" />;

  return (
    <Card style={{ maxWidth: 900 }}>
      <Table>
        <thead>
          <tr>
            <Th>Revision</Th>
            <Th>Created</Th>
            <Th>Actor</Th>
            <Th>Reason</Th>
            <Th />
          </tr>
        </thead>
        <tbody>
          {list.items.map((rev) => {
            const isCurrent = policy.data?.revision === rev.revision;
            return (
              <tr key={rev.revision}>
                <Td mono>
                  <span style={{ color: "var(--purple)" }}>rev {rev.revision}</span>{" "}
                  {isCurrent && <span style={{ color: "var(--blue)", fontSize: 10 }}>CURRENT</span>}
                </Td>
                <Td mono>{formatDateTime(rev.created_at)}</Td>
                <Td>
                  {rev.actor_type}:{rev.actor_id}
                </Td>
                <Td>{rev.change_reason || <span style={{ color: "var(--ink3)" }}>—</span>}</Td>
                <Td>
                  <div style={{ display: "flex", gap: 6 }}>
                    <Button onClick={() => setViewing(viewing?.revision === rev.revision ? null : rev)}>
                      {viewing?.revision === rev.revision ? "Hide" : "JSON"}
                    </Button>
                    {!isCurrent && <Button onClick={() => setRestoreTarget(rev)}>Restore</Button>}
                  </div>
                </Td>
              </tr>
            );
          })}
        </tbody>
      </Table>
      {viewing && (
        <div style={{ marginTop: 12 }}>
          <CodeBlock code={JSON.stringify(viewing.policy, null, 2)} lang="json" />
        </div>
      )}
      <div style={{ display: "flex", alignItems: "center", gap: 8, marginTop: 12, fontSize: 11, color: "var(--ink3)" }}>
        Showing {list.items.length} revision{list.items.length === 1 ? "" : "s"}
        <span style={{ flex: 1 }} />
        {list.nextCursor && (
          <Button onClick={list.loadMore} disabled={list.loadingMore}>
            {list.loadingMore ? "Loading…" : "Load more"}
          </Button>
        )}
      </div>
      <ReasonModal
        open={restoreTarget !== null}
        onClose={() => setRestoreTarget(null)}
        onConfirm={(reason) => void restore(reason)}
        title={`Restore policy revision ${restoreTarget?.revision ?? ""}`}
        description="Restore publishes the selected revision's policy as a new revision. The current revision stays in history."
        confirmLabel="Restore"
        danger
        busy={restoring}
      />
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/* Quota snapshot                                                      */
/* ------------------------------------------------------------------ */

function balanceValue(key: string, value: number | undefined): string {
  if (value === undefined || value === null) return "—";
  return key.endsWith("_micros") ? formatMicrosUSD(value) : formatNumber(value);
}

function BalanceBar({ balance }: { balance: QuotaBalance }) {
  if (balance.limit === undefined || balance.limit <= 0) {
    return <span style={{ color: "var(--ink3)", fontSize: 11 }}>no limit</span>;
  }
  const used = balance.committed + balance.reserved;
  const pct = Math.min(100, Math.round((used / balance.limit) * 100));
  const color = pct >= 90 ? "var(--red)" : pct >= 70 ? "var(--amber)" : "var(--green)";
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 8, minWidth: 140 }}>
      <div style={{ flex: 1, height: 6, borderRadius: 99, background: "var(--chip)", overflow: "hidden" }}>
        <div style={{ height: "100%", width: `${pct}%`, background: color, borderRadius: 99 }} />
      </div>
      <span style={{ fontFamily: "var(--font-mono)", fontSize: 10.5, color: "var(--ink3)" }}>{pct}%</span>
    </div>
  );
}

export function QuotaSnapshotPanel({ path }: { path: string }) {
  const { data, error, loading, retry } = useApi<QuotaSnapshot>(path);

  if (loading) return <Loading />;
  if (error) return <ErrorBanner error={error} retry={retry} />;
  if (!data) return <EmptyState title="No quota snapshot" />;

  const keys = Object.keys(data.balances ?? {}).sort();
  return (
    <Card
      title={
        <span>
          Quota snapshot{" "}
          <span style={{ fontWeight: 400, color: "var(--ink3)" }}>
            · observed {formatDateTime(data.observed_at)} · eventually consistent
          </span>
        </span>
      }
      actions={<Button onClick={retry}>Refresh</Button>}
      style={{ maxWidth: 900 }}
    >
      <div style={{ display: "flex", gap: 16, fontSize: 11, color: "var(--ink3)", marginBottom: 10, flexWrap: "wrap" }}>
        <span>
          Tenant policy revision <b style={{ color: "var(--purple)" }}>{data.tenant_policy_revision}</b>
        </span>
        {data.api_key_policy_revision !== undefined && data.api_key_policy_revision !== 0 && (
          <span>
            Key policy revision <b style={{ color: "var(--purple)" }}>{data.api_key_policy_revision}</b>
          </span>
        )}
        {data.currency && <span>Currency {data.currency}</span>}
      </div>
      {keys.length === 0 ? (
        <EmptyState title="No balances" hint="No quota limits are enforced for this scope" />
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Limit</Th>
              <Th>Limit value</Th>
              <Th>Reserved</Th>
              <Th>Committed</Th>
              <Th>Uncertain</Th>
              <Th>Remaining</Th>
              <Th>Usage</Th>
            </tr>
          </thead>
          <tbody>
            {keys.map((key) => {
              const balance = data.balances[key];
              return (
                <tr key={key}>
                  <Td mono>{key}</Td>
                  <Td mono>{balanceValue(key, balance.limit)}</Td>
                  <Td mono>{balanceValue(key, balance.reserved)}</Td>
                  <Td mono>{balanceValue(key, balance.committed)}</Td>
                  <Td mono>
                    {balance.uncertain > 0 ? (
                      <Badge tone={statusTone("uncertain")}>{balanceValue(key, balance.uncertain)}</Badge>
                    ) : (
                      balanceValue(key, balance.uncertain)
                    )}
                  </Td>
                  <Td mono>{balanceValue(key, balance.remaining)}</Td>
                  <Td>
                    <BalanceBar balance={balance} />
                  </Td>
                </tr>
              );
            })}
          </tbody>
        </Table>
      )}
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
        Reservations are taken before provider execution. “Uncertain” holds come from responses whose provider
        delivery could not be confirmed; they release on reconciliation.
      </div>
    </Card>
  );
}
