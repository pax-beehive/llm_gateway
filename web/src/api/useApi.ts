import { useCallback, useEffect, useState } from "react";
import { apiGet, ApiError } from "./client";

export interface ApiState<T> {
  data: T | null;
  error: ApiError | null;
  loading: boolean;
  retry: () => void;
}

/**
 * Standard GET-on-mount hook. Pages use it like:
 *   const { data, error, loading, retry } = useApi<TenantList>("/control/v1/tenants");
 * Render `loading` → Spinner, `error` → ErrorBanner(retry), `data` → content.
 */
export function useApi<T>(path: string): ApiState<T> {
  const [state, setState] = useState<{ data: T | null; error: ApiError | null; loading: boolean }>({
    data: null,
    error: null,
    loading: true,
  });
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    setState((s) => ({ ...s, loading: true, error: null }));
    apiGet<T>(path, { signal: controller.signal })
      .then((data) => {
        if (!controller.signal.aborted) setState({ data, error: null, loading: false });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState({
          data: null,
          error: err instanceof ApiError ? err : new ApiError(0, "network", String(err)),
          loading: false,
        });
      });
    return () => controller.abort();
  }, [path, attempt]);

  const retry = useCallback(() => setAttempt((a) => a + 1), []);
  return { ...state, retry };
}
