import { useEffect, useState } from "react";
import { apiGet, ApiError } from "../../api/client";
import type { ReadinessResult } from "./types";

export interface OpsState<T> {
  data: T | null;
  error: ApiError | null;
  loading: boolean;
  updatedAt: Date | null;
}

/**
 * GET-on-mount for operations endpoints that also refetches when `tick`
 * changes (manual refresh / 10s auto-refresh). Keeps previous data while
 * refetching so the view does not flash back to a spinner.
 */
export function useOps<T>(path: string, tick: number, onUpdated?: (at: Date) => void): OpsState<T> {
  const [state, setState] = useState<{ data: T | null; error: ApiError | null; loading: boolean; updatedAt: Date | null }>({
    data: null,
    error: null,
    loading: true,
    updatedAt: null,
  });

  useEffect(() => {
    const controller = new AbortController();
    setState((s) => ({ ...s, loading: s.data === null, error: null }));
    apiGet<T>(path, { signal: controller.signal })
      .then((data) => {
        if (controller.signal.aborted) return;
        const now = new Date();
        setState({ data, error: null, loading: false, updatedAt: now });
        onUpdated?.(now);
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState((s) => ({
          ...s,
          error: err instanceof ApiError ? err : new ApiError(0, "network", String(err)),
          loading: false,
        }));
      });
    return () => controller.abort();
  }, [path, tick]);

  return state;
}

/**
 * Process-level readiness probe. /readyz answers 503 with a ReadinessResult
 * body when not ready — not an error envelope — so this uses fetch directly
 * and treats any JSON body as the result.
 */
export function useReadiness(path: string, tick: number): ReadinessResult | null {
  const [result, setResult] = useState<ReadinessResult | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    fetch(`/api${path}`, { headers: { Accept: "application/json" }, signal: controller.signal })
      .then(async (response) => {
        try {
          const body = (await response.json()) as Partial<ReadinessResult>;
          if (controller.signal.aborted) return;
          setResult({
            ready: body.ready === true,
            checks: Array.isArray(body.checks) ? (body.checks as ReadinessResult["checks"]) : [],
            checked_at: typeof body.checked_at === "string" ? body.checked_at : new Date().toISOString(),
          });
        } catch {
          if (!controller.signal.aborted) {
            setResult({ ready: false, checks: [{ name: "process", status: "unavailable" }], checked_at: new Date().toISOString() });
          }
        }
      })
      .catch(() => {
        if (!controller.signal.aborted) {
          setResult({ ready: false, checks: [{ name: "process", status: "unavailable" }], checked_at: new Date().toISOString() });
        }
      });
    return () => controller.abort();
  }, [path, tick]);

  return result;
}
