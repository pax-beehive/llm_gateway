/** Read-only routes table for the published revision, with a detail drawer. */
import { useState } from "react";
import { Drawer } from "../../components/feedback";
import { Badge, CodeBlock, EmptyState, KeyValueList, Table, Td, Th } from "../../components/ui";
import { formatMicrosUSD } from "../../lib/format";
import { routeStatusTone, type ManagedRoute } from "./types";
import { CapabilityBadges } from "./widgets";

export function RoutesTable({ routes }: { routes: ManagedRoute[] }) {
  const [selected, setSelected] = useState<ManagedRoute | null>(null);

  if (routes.length === 0) {
    return <EmptyState title="No routes" hint="The published document contains no managed routes." />;
  }
  return (
    <>
      <Table>
        <thead>
          <tr>
            <Th>Route</Th>
            <Th>Public model</Th>
            <Th>Connection</Th>
            <Th>Provider model</Th>
            <Th>Region</Th>
            <Th>Status</Th>
            <Th>Capabilities</Th>
          </tr>
        </thead>
        <tbody>
          {routes.map((route) => (
            <tr key={route.route_id} onClick={() => setSelected(route)} style={{ cursor: "pointer" }}>
              <Td mono>{route.route_id}</Td>
              <Td mono>{route.public_model}</Td>
              <Td mono>{route.provider_connection_id}</Td>
              <Td mono>{route.provider_model}</Td>
              <Td mono>{route.execution_region}</Td>
              <Td>
                <Badge tone={routeStatusTone(route.administrative_status)}>{route.administrative_status}</Badge>
              </Td>
              <Td>
                <CapabilityBadges capabilities={route.capabilities} />
              </Td>
            </tr>
          ))}
        </tbody>
      </Table>
      <RouteDrawer route={selected} onClose={() => setSelected(null)} />
    </>
  );
}

function RouteDrawer({ route, onClose }: { route: ManagedRoute | null; onClose: () => void }) {
  return (
    <Drawer open={route !== null} onClose={onClose} title={route ? `Route ${route.route_id}` : "Route"} width={520}>
      {route && (
        <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
          <KeyValueList
            items={[
              { key: "Route ID", value: route.route_id, mono: true },
              { key: "Public model", value: route.public_model, mono: true },
              { key: "Provider connection", value: route.provider_connection_id, mono: true },
              { key: "Provider model", value: route.provider_model, mono: true },
              { key: "Execution region", value: route.execution_region, mono: true },
              { key: "Home region", value: route.home_region, mono: true },
              { key: "Status", value: <Badge tone={routeStatusTone(route.administrative_status)}>{route.administrative_status}</Badge> },
              { key: "Capability profile rev", value: String(route.capability_profile_revision), mono: true },
              {
                key: "Selection policy",
                value: `priority ${route.selection_policy.priority} · weight ${route.selection_policy.weight}`,
                mono: true,
              },
              {
                key: "Tenant visibility",
                value: route.tenant_visibility_policy.all_tenants
                  ? "All tenants"
                  : (route.tenant_visibility_policy.tenant_ids ?? []).join(", ") || "—",
                mono: true,
              },
              { key: "Cache usage reliable", value: route.cache_usage_reliable ? "yes" : "no" },
              {
                key: "Cache protection",
                value: route.cache_protection_policy.enabled
                  ? `enabled · ttl ${route.cache_protection_policy.ttl_seconds ?? 0}s`
                  : "disabled",
                mono: true,
              },
              {
                key: "Cost (in / out per 1M)",
                value: `${formatMicrosUSD(route.provider_cost_snapshot.input_per_million_micros)} / ${formatMicrosUSD(route.provider_cost_snapshot.output_per_million_micros)}`,
                mono: true,
              },
              ...(route.embedding_path ? [{ key: "Embedding path", value: route.embedding_path, mono: true }] : []),
              ...(route.moderation_path ? [{ key: "Moderation path", value: route.moderation_path, mono: true }] : []),
              ...(route.rerank_path ? [{ key: "Rerank path", value: route.rerank_path, mono: true }] : []),
              ...(route.embedding_dimensions
                ? [{ key: "Embedding dimensions", value: String(route.embedding_dimensions), mono: true }]
                : []),
            ]}
          />
          <div>
            <div style={{ fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 6 }}>CAPABILITIES</div>
            <CapabilityBadges capabilities={route.capabilities} />
          </div>
          <div>
            <div style={{ fontSize: 11, fontWeight: 600, color: "var(--ink3)", marginBottom: 6 }}>ROUTE JSON</div>
            <CodeBlock code={JSON.stringify(route, null, 2)} lang="json" />
          </div>
        </div>
      )}
    </Drawer>
  );
}
