import type { SessionView } from "./types";

/**
 * WorkOS permission strings from the session response. Frontend permissions
 * only hide/disable UI — the BFF and upstream services re-authorize every
 * request server-side. Never derive permissions from the role client-side.
 */
export const PERMISSIONS = {
  tenantsRead: "platform:tenants:read",
  tenantsWrite: "platform:tenants:write",
  meteringRead: "platform:metering:read",
  meteringWrite: "platform:metering:write",
  providersRead: "platform:providers:read",
  providersWrite: "platform:providers:write",
  routingRead: "platform:routing:read",
  routingWrite: "platform:routing:write",
  operationsRead: "platform:operations:read",
  modelsRead: "gateway:models:read",
  playgroundUse: "gateway:playground:use",
} as const;

export function hasPermission(session: SessionView | null, permission: string): boolean {
  return session?.permissions.includes(permission) ?? false;
}
