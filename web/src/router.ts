import { useSyncExternalStore } from "react";

/**
 * Tiny hash router. Routes are "#/overview" style; the route id is the first
 * path segment. Unknown or empty hashes resolve to the default route.
 */
export const DEFAULT_ROUTE = "overview";

export function currentRoute(): string {
  const hash = window.location.hash.replace(/^#\/?/, "");
  const segment = hash.split("/")[0]?.split("?")[0] ?? "";
  return segment || DEFAULT_ROUTE;
}

export function navigate(route: string): void {
  window.location.hash = `/${route}`;
}

function subscribe(callback: () => void): () => void {
  window.addEventListener("hashchange", callback);
  return () => window.removeEventListener("hashchange", callback);
}

export function useRoute(): string {
  return useSyncExternalStore(subscribe, currentRoute, () => DEFAULT_ROUTE);
}
