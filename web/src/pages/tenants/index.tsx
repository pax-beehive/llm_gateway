/**
 * Tenants console: list with status/search filters and cursor pagination,
 * create-tenant modal, and the routed detail sub-view (#/tenants/{id}).
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
} from "./common";
import TenantDetail from "./detail";
import type { Tenant } from "./types";

/* ------------------------------------------------------------------ */
/* Create tenant modal                                                 */
/* ------------------------------------------------------------------ */

function CreateTenantModal({
  open,
  onClose,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  onCreated: () => void;
}) {
  const toast = useToast();
  const [id, setId] = useState("");
  const [slug, setSlug] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [homeRegion, setHomeRegion] = useState("");
  const [metadata, setMetadata] = useState("");
  const [showPolicy, setShowPolicy] = useState(false);
  const [policyJson, setPolicyJson] = useState("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (open) {
      setId("");
      setSlug("");
      setDisplayName("");
      setHomeRegion("");
      setMetadata("");
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
      toast(`Initial policy: ${pol.error}`, "error");
      return;
    }
    setBusy(true);
    try {
      await apiSend<Tenant>("POST", "/control/v1/tenants", {
        id,
        slug,
        display_name: displayName,
        home_region: homeRegion,
        ...(meta.value ? { metadata: meta.value } : {}),
        ...(pol.value ? { initial_policy: pol.value } : {}),
        reason,
      });
      toast("Tenant created", "success");
      onCreated();
      onClose();
    } catch (err) {
      toastMutationError(toast, err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      open={open}
      onClose={onClose}
      title="Create tenant"
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button
            variant="primary"
            disabled={!id.trim() || !slug.trim() || !displayName.trim() || !homeRegion.trim() || !reason.trim() || busy}
            onClick={() => void submit()}
          >
            Create tenant
          </Button>
        </>
      }
    >
      <Field label="ID" hint="Immutable tenant identifier, e.g. tn_acme.">
        <input value={id} onChange={(e) => setId(e.target.value)} style={monoInputStyle} placeholder="tn_acme" />
      </Field>
      <Field label="Slug">
        <input value={slug} onChange={(e) => setSlug(e.target.value)} style={inputStyle} placeholder="acme" />
      </Field>
      <Field label="Display name">
        <input value={displayName} onChange={(e) => setDisplayName(e.target.value)} style={inputStyle} placeholder="Acme AI" />
      </Field>
      <Field label="Home region" hint="Region authorized for strongly consistent writes.">
        <input value={homeRegion} onChange={(e) => setHomeRegion(e.target.value)} style={monoInputStyle} placeholder="us-west" />
      </Field>
      <Field label="Metadata (optional JSON object)">
        <textarea
          value={metadata}
          onChange={(e) => setMetadata(e.target.value)}
          rows={3}
          placeholder='{"tier":"enterprise"}'
          style={{ ...monoInputStyle, resize: "vertical" }}
        />
      </Field>
      <div style={{ marginBottom: 12 }}>
        <Button onClick={() => setShowPolicy((v) => !v)}>
          {showPolicy ? "Hide initial policy" : "Initial policy (advanced)"}
        </Button>
        {showPolicy && (
          <div style={{ marginTop: 8 }}>
            <textarea
              value={policyJson}
              onChange={(e) => setPolicyJson(e.target.value)}
              rows={6}
              placeholder='{"limits":{"requests_per_minute":600,"daily_spend_micros":50000000}}'
              style={{ ...monoInputStyle, resize: "vertical" }}
            />
            <div style={{ fontSize: 11, color: "var(--ink3)", marginTop: 4 }}>
              A <code>TenantPolicy</code> JSON object — see <code>docs/design</code> and{" "}
              <code>internal/core/types.go</code> for field semantics. Must be valid JSON.
            </div>
          </div>
        )}
      </div>
      <Field label="Reason (required)">
        <textarea
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          rows={2}
          placeholder="Why is this tenant being created?"
          style={{ ...inputStyle, resize: "vertical" }}
        />
      </Field>
    </Modal>
  );
}

/* ------------------------------------------------------------------ */
/* List                                                                */
/* ------------------------------------------------------------------ */

function TenantList() {
  const { can } = useAuth();
  const canWrite = can(PERMISSIONS.tenantsWrite);
  const [statusFilter, setStatusFilter] = useState("");
  const [includeClosed, setIncludeClosed] = useState(false);
  const [search, setSearch] = useState("");
  const [createOpen, setCreateOpen] = useState(false);

  const path =
    "/control/v1/tenants?limit=25" +
    (statusFilter ? `&status=${encodeURIComponent(statusFilter)}` : "") +
    (includeClosed ? "&include_closed=true" : "");
  const list = usePagedList<Tenant>(path);

  const needle = search.trim().toLowerCase();
  const visible = needle
    ? list.items.filter(
        (t) =>
          t.id.toLowerCase().includes(needle) ||
          t.slug.toLowerCase().includes(needle) ||
          t.display_name.toLowerCase().includes(needle),
      )
    : list.items;

  return (
    <div style={{ padding: "20px 24px", maxWidth: 1360, margin: "0 auto" }}>
      <div style={{ display: "flex", alignItems: "flex-start", gap: 16, marginBottom: 14, flexWrap: "wrap" }}>
        <div style={{ flex: 1 }}>
          <h1 style={{ margin: 0, fontSize: 18, fontWeight: 600 }}>Tenants</h1>
          <div style={{ color: "var(--ink2)", marginTop: 2, fontSize: 12 }}>
            Isolated customer organizations. Each tenant has a Home Region authorized for strongly consistent writes.
          </div>
        </div>
        <Button
          variant="primary"
          disabled={!canWrite}
          title={canWrite ? undefined : "Requires platform:tenants:write"}
          onClick={() => setCreateOpen(true)}
        >
          Create Tenant
        </Button>
      </div>
      <Card>
        <div style={{ display: "flex", gap: 10, alignItems: "center", marginBottom: 12, flexWrap: "wrap" }}>
          <input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search id, slug, or name…"
            style={{ ...inputStyle, width: 240 }}
            aria-label="Search tenants"
          />
          <select
            value={statusFilter}
            onChange={(e) => setStatusFilter(e.target.value)}
            style={{ ...inputStyle, width: "auto" }}
            aria-label="Filter by status"
          >
            <option value="">All statuses</option>
            <option value="active">Active</option>
            <option value="suspended">Suspended</option>
            <option value="closed">Closed</option>
          </select>
          <label style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 12, color: "var(--ink2)" }}>
            <input type="checkbox" checked={includeClosed} onChange={(e) => setIncludeClosed(e.target.checked)} />
            Include closed
          </label>
        </div>
        {list.loading ? (
          <Loading />
        ) : list.error ? (
          <ErrorBanner error={list.error} retry={list.reload} />
        ) : visible.length === 0 ? (
          <EmptyState
            title="No tenants"
            hint={needle ? "No tenants match the search" : "Create one to get started"}
            action={
              !needle ? (
                <Button
                  variant="primary"
                  disabled={!canWrite}
                  title={canWrite ? undefined : "Requires platform:tenants:write"}
                  onClick={() => setCreateOpen(true)}
                >
                  Create Tenant
                </Button>
              ) : undefined
            }
          />
        ) : (
          <>
            <Table>
              <thead>
                <tr>
                  <Th>Tenant</Th>
                  <Th>Slug</Th>
                  <Th>Status</Th>
                  <Th>Home region</Th>
                  <Th>Revision</Th>
                </tr>
              </thead>
              <tbody>
                {visible.map((tenant) => (
                  <tr
                    key={tenant.id}
                    onClick={() => navigate(`tenants/${encodeURIComponent(tenant.id)}`)}
                    style={{ cursor: "pointer" }}
                  >
                    <Td>
                      <div style={{ fontWeight: 600 }}>{tenant.display_name}</div>
                      <div style={{ fontFamily: "var(--font-mono)", fontSize: 10.5, color: "var(--ink3)" }}>
                        {tenant.id}
                      </div>
                    </Td>
                    <Td mono>{tenant.slug}</Td>
                    <Td>
                      <Badge tone={statusTone(tenant.status)}>{tenant.status}</Badge>
                    </Td>
                    <Td mono>{tenant.home_region}</Td>
                    <Td mono>{tenant.revision}</Td>
                  </tr>
                ))}
              </tbody>
            </Table>
            <div style={{ display: "flex", alignItems: "center", gap: 8, marginTop: 10, fontSize: 11, color: "var(--ink3)" }}>
              Showing {visible.length} of {list.items.length} loaded tenant{list.items.length === 1 ? "" : "s"}
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
      <CreateTenantModal open={createOpen} onClose={() => setCreateOpen(false)} onCreated={list.reload} />
    </div>
  );
}

export default function TenantsPage() {
  const tail = useHashTail();
  const tenantId = tail[0];
  if (tenantId) return <TenantDetail tenantId={tenantId} />;
  return <TenantList />;
}
